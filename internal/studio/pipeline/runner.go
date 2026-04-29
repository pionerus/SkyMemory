// Package pipeline orchestrates the FFmpeg-driven render of a project.
//
// Stage 1 (this file) = minimum-viable Generate:
//   1. Pre-flight: every clip must have ffprobe metadata. Bail early if not.
//   2. Trim each clip using ffmpeg -ss/-to into a per-clip intermediate that's
//      already at the canonical encode (H.264 1080p 30fps, AAC 48k stereo).
//      We always re-encode at this step rather than -c copy because the
//      operator's source clips probably have mixed codecs/resolutions.
//   3. Concat the intermediates with ffmpeg's concat demuxer (-f concat).
//   4. Move the result to ~/.freefall-studio/jobs/<id>/output_1080p.mp4.
//
// Music mixing, sidechain ducking, crossfades, vertical/4K, photos, and
// uploads to cloud storage are deferred to follow-up stages — each builds
// directly on this orchestrator without changing the public Run signature.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pionerus/freefall/internal/studio/state"
)

// Runner owns the dependencies the pipeline needs at runtime. Construct once
// at studio boot; Run() is safe to call concurrently for different projects.
type Runner struct {
	DB      *state.DB
	JobsDir string // ~/.freefall-studio/jobs

	// MusicCache resolves a project's music_track_id to a local file path,
	// downloading from cloud on first call. Optional — when nil, the pipeline
	// silently skips the mix stage and the output has only the project audio.
	MusicCache MusicCacheLike

	// FFmpegPath / FFprobePath are looked up from PATH on first Run if empty.
	FFmpegPath  string
	FFprobePath string
}

// MusicCacheLike abstracts internal/studio/music.Cache so the pipeline package
// doesn't import studio/music directly (avoids an import cycle with anything
// that wants to use both packages from cmd/studio).
type MusicCacheLike interface {
	Ensure(ctx context.Context, trackID int64) (string, error)
}

// Run kicks off the synchronous pipeline. Caller is expected to invoke it
// inside a goroutine — Run streams progress via UpdateGeneration calls.
//
// Errors here are stored in generations.error; the function returns them as
// well so callers can log to studio's stderr.
func (r *Runner) Run(ctx context.Context, projectID, generationID int64) (string, error) {
	if r.FFmpegPath == "" {
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return "", r.fail(ctx, generationID, "ffmpeg not on PATH — install ffmpeg and restart studio")
		}
		r.FFmpegPath = p
	}

	clips, err := r.DB.ListClips(ctx, projectID)
	if err != nil {
		return "", r.fail(ctx, generationID, "list clips: "+err.Error())
	}
	if len(clips) == 0 {
		return "", r.fail(ctx, generationID, "no clips uploaded yet")
	}
	for _, c := range clips {
		if c.DurationSeconds <= 0 {
			return "", r.fail(ctx, generationID, fmt.Sprintf("clip %q has no duration metadata (was ffprobe missing on upload? re-upload to fix)", c.Kind))
		}
	}

	projectDir := filepath.Join(r.JobsDir, strconv.FormatInt(projectID, 10))
	intermediatesDir := filepath.Join(projectDir, "intermediates")
	if err := os.MkdirAll(intermediatesDir, 0o755); err != nil {
		return "", r.fail(ctx, generationID, "mkdir intermediates: "+err.Error())
	}
	// Clean any leftover intermediates from a prior run.
	if entries, err := os.ReadDir(intermediatesDir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(intermediatesDir, e.Name()))
		}
	}

	// === Stage A: trim+normalise each clip in canonical order ===
	// ListClips already returns canonical-then-custom order.
	intermediates := make([]string, 0, len(clips))
	for i, c := range clips {
		stepLabel := fmt.Sprintf("trimming %s (%d/%d)", c.Kind, i+1, len(clips))
		_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
			Status:      ptr(state.GenStatusTrimming),
			StepLabel:   ptr(stepLabel),
			ProgressPct: ptr(int(float64(i) / float64(len(clips)) * 80)), // 0..80%
		})

		out := filepath.Join(intermediatesDir, fmt.Sprintf("seg_%02d_%s.mp4", i, sanitizeForFilename(c.Kind)))
		if err := r.trimAndNormalise(ctx, c, out); err != nil {
			return "", r.fail(ctx, generationID, fmt.Sprintf("trim %s: %v", c.Kind, err))
		}
		intermediates = append(intermediates, out)
	}

	// === Stage B: build concat list, run concat demuxer ===
	_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
		Status:      ptr(state.GenStatusConcating),
		StepLabel:   ptr("concatenating segments"),
		ProgressPct: ptr(85),
	})

	listFile := filepath.Join(intermediatesDir, "concat.txt")
	if err := writeConcatList(listFile, intermediates); err != nil {
		return "", r.fail(ctx, generationID, "write concat list: "+err.Error())
	}

	concatOnly := filepath.Join(intermediatesDir, "concat_only.mp4")
	_ = os.Remove(concatOnly)

	cmd := exec.CommandContext(ctx, r.FFmpegPath,
		"-nostats", "-hide_banner",
		"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy", // intermediates are already H.264/AAC at 1080p — concat without re-encode
		"-movflags", "+faststart",
		concatOnly,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", r.fail(ctx, generationID, fmt.Sprintf("ffmpeg concat failed: %v\n%s", err, string(out)))
	}

	finalOut := filepath.Join(projectDir, "output_1080p.mp4")
	_ = os.Remove(finalOut) // overwrite previous

	// === Stage C: mix music (only if project has a picked track) ===
	project, err := r.DB.GetProject(ctx, projectID)
	if err != nil {
		return "", r.fail(ctx, generationID, "load project for music: "+err.Error())
	}

	if project.MusicTrackID > 0 && r.MusicCache != nil {
		_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
			Status:      ptr(state.GenStatusConcating),
			StepLabel:   ptr("downloading music + mixing"),
			ProgressPct: ptr(92),
		})

		musicPath, err := r.MusicCache.Ensure(ctx, project.MusicTrackID)
		if err != nil {
			return "", r.fail(ctx, generationID, "music download: "+err.Error())
		}

		// Total project duration (sum of trim windows) — used for the music fade-out.
		// Falls back to "skip fade-out" if zero.
		totalSec, _ := r.DB.SumProjectClipDuration(ctx, projectID)

		if err := r.mixMusic(ctx, concatOnly, musicPath, totalSec, finalOut); err != nil {
			return "", r.fail(ctx, generationID, "music mix: "+err.Error())
		}
	} else {
		// No music picked: rename concat-only into the canonical output path.
		if err := os.Rename(concatOnly, finalOut); err != nil {
			return "", r.fail(ctx, generationID, "rename concat output: "+err.Error())
		}
	}

	// === Stage D: stat output, mark done ===
	stat, err := os.Stat(finalOut)
	if err != nil {
		return "", r.fail(ctx, generationID, "stat output: "+err.Error())
	}
	size := stat.Size()
	_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
		Status:      ptr(state.GenStatusDone),
		StepLabel:   ptr("done"),
		ProgressPct: ptr(100),
		OutputPath:  &finalOut,
		OutputSize:  &size,
		Finish:      true,
	})
	return finalOut, nil
}

// mixMusic adds a music bed under the existing project audio.
//
// Filter chain:
//   • [1:a] aloop=loop=-1:size=2e9   — repeat music if shorter than video
//   • volume=0.25                    — duck music to 25% so dialogue stays clear
//   • afade=t=in:st=0:d=1            — 1-sec fade-in at music start
//   • [outa] amix duration=first     — output ends when project audio ends
//   • afade=t=out:st=<end-1>:d=1     — 1-sec fade-out on the mixed result
//
// Video is stream-copied (-c:v copy) — no re-encode, no quality loss.
func (r *Runner) mixMusic(ctx context.Context, videoPath, musicPath string, totalSec float64, dstPath string) error {
	// Music stream filter: loop infinitely, drop to 25%, fade in over 1s.
	bgFilter := "[1:a]aloop=loop=-1:size=2e9,volume=0.25,afade=t=in:st=0:d=1[bg]"

	// Mix project audio with music. Apply a 1-second fade-out at the very end
	// of the result if we know the duration; otherwise skip the fade.
	mixFilter := "[0:a][bg]amix=inputs=2:duration=first:dropout_transition=0"
	if totalSec > 1.5 {
		fadeStart := totalSec - 1.0
		mixFilter += fmt.Sprintf(",afade=t=out:st=%s:d=1", floatStr(fadeStart))
	}
	mixFilter += "[outa]"

	filterComplex := bgFilter + ";" + mixFilter

	cmd := exec.CommandContext(ctx, r.FFmpegPath,
		"-nostats", "-hide_banner",
		"-y",
		"-i", videoPath,
		"-i", musicPath,
		"-filter_complex", filterComplex,
		"-map", "0:v",
		"-map", "[outa]",
		"-c:v", "copy",
		"-c:a", "aac",
		"-b:a", "192k",
		"-ar", "48000",
		"-movflags", "+faststart",
		dstPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg mix: %v\n%s", err, string(out))
	}
	return nil
}

// trimAndNormalise runs ffmpeg with -ss/-to + scale-pad to bring an arbitrary
// source clip into the canonical 1080p H.264 + AAC stereo intermediate format
// that the concat demuxer can stitch with -c copy.
//
// We put -ss/-to AFTER the -i so cuts are frame-accurate (slower than the
// before-input fast-seek form, but correct — the fast form snaps to keyframes
// which can over-cut by up to a GOP).
//
// When the source has no audio, we synthesise a silent stereo track via
// `anullsrc` and map it explicitly, so all intermediates have a uniform
// stream layout — concat demuxer chokes on heterogeneous streams.
func (r *Runner) trimAndNormalise(ctx context.Context, c state.Clip, dstPath string) error {
	in := c.TrimInSeconds
	if in < 0 {
		in = 0
	}
	out := c.TrimOutSeconds
	if out <= 0 || out > c.DurationSeconds {
		out = c.DurationSeconds
	}
	if out <= in {
		return errors.New("trim_out <= trim_in — operator-set window is empty")
	}

	const videoFilter = "scale=1920:1080:force_original_aspect_ratio=decrease," +
		"pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black,fps=30"

	commonOut := []string{
		"-vf", videoFilter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "192k",
		"-ar", "48000",
		"-ac", "2",
		"-ss", floatStr(in),
		"-to", floatStr(out),
		"-movflags", "+faststart",
		dstPath,
	}

	var args []string
	if c.HasAudio {
		args = append([]string{
			"-nostats", "-hide_banner", "-y",
			"-i", c.SourcePath,
		}, commonOut...)
	} else {
		// Two inputs: real video + synthetic silence. -shortest stops at the
		// shorter of the two (the trimmed video).
		args = append([]string{
			"-nostats", "-hide_banner", "-y",
			"-i", c.SourcePath,
			"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
			"-map", "0:v:0",
			"-map", "1:a:0",
			"-shortest",
		}, commonOut...)
	}

	cmd := exec.CommandContext(ctx, r.FFmpegPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg trim: %v\n%s", err, string(out))
	}
	return nil
}

// fail sets the generation row to status='failed' with the given error and
// finished_at = now(). Returns the same error the caller can propagate.
func (r *Runner) fail(ctx context.Context, generationID int64, errMsg string) error {
	_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
		Status: ptr(state.GenStatusFailed),
		Error:  &errMsg,
		Finish: true,
	})
	return errors.New(errMsg)
}

// =====================================================================
// helpers
// =====================================================================

func ptr[T any](v T) *T { return &v }

func floatStr(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func writeConcatList(path string, segments []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, s := range segments {
		// Concat demuxer is picky: paths must be quoted on Windows AND backslashes
		// either forward-slashed or escaped. We use forward slashes which both
		// ffmpeg and Windows file APIs accept.
		fwd := filepath.ToSlash(s)
		if _, err := fmt.Fprintf(f, "file '%s'\n", fwd); err != nil {
			return err
		}
	}
	return nil
}

func sanitizeForFilename(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// Sentinel for callers that need a no-op duration to wait between progress
// pings. Kept here so handler tests don't sprinkle magic numbers.
const ProgressPollInterval = 1 * time.Second
