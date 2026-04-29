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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	segments := make([]segmentMeta, 0, len(clips))
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
		segments = append(segments, segmentMeta{path: out, duration: effectiveDuration(c)})
	}

	// === Stage B: concatenate with crossfades ===
	_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
		Status:      ptr(state.GenStatusConcating),
		StepLabel:   ptr("crossfading segments"),
		ProgressPct: ptr(85),
	})

	concatOnly := filepath.Join(intermediatesDir, "concat_only.mp4")
	_ = os.Remove(concatOnly)
	concatDur, err := r.concatWithCrossfades(ctx, segments, concatOnly)
	if err != nil {
		return "", r.fail(ctx, generationID, "concat: "+err.Error())
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

		// Use the ACTUAL post-crossfade duration so the music fade-out lands on
		// the real end-of-video, not (N-1)*crossfade seconds past it.
		if err := r.mixMusic(ctx, concatOnly, musicPath, concatDur, finalOut); err != nil {
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

// mixMusic adds a music bed under the existing project audio AND ducks the
// music dynamically using sidechain compression triggered by the project
// audio itself.
//
// Why this is the right shape (matches the pipeline plan §"Step 6 mix"):
//   • Action segments (intro/walk/freefall/landing/closing/custom) had their
//     audio muted in Stage A → silence at the duck trigger → music plays
//     at full volume. The wind-noise / footsteps the operator didn't want
//     are gone, replaced by the score.
//   • Interview segments kept their speech → trigger the compressor → music
//     ducks below the dialogue automatically. No manual volume keyframes.
//
// Filter chain:
//   [1:a] aloop=loop=-1:size=2e9, afade=t=in:st=0:d=1, volume=0.7  [music_in]
//          # repeat if music shorter than video, 1s fade-in at start, gentle
//          # pre-attenuation so even un-ducked music isn't overpowering
//   [music_in][0:a]
//          sidechaincompress=threshold=0.05:ratio=8:attack=20:release=400
//                                                                    [duck]
//          # music compressed by project audio: speech in [0:a] makes [duck]
//          # drop ~8x; silence keeps [duck] at the [music_in] level.
//   [0:a][duck] amix=inputs=2:duration=first:dropout_transition=0,
//          afade=t=out:st=<dur-1>:d=1                              [outa]
//          # mix speech and ducked music; 1s fade-out on the final result.
//
// Video is stream-copied (-c:v copy) — no re-encode, no quality loss.
func (r *Runner) mixMusic(ctx context.Context, videoPath, musicPath string, totalSec float64, dstPath string) error {
	musicIn := "[1:a]aloop=loop=-1:size=2e9,afade=t=in:st=0:d=1,volume=0.7[music_in]"
	duckChain := "[music_in][0:a]sidechaincompress=threshold=0.05:ratio=8:attack=20:release=400[duck]"

	finalChain := "[0:a][duck]amix=inputs=2:duration=first:dropout_transition=0"
	if totalSec > 1.5 {
		fadeStart := totalSec - 1.0
		finalChain += fmt.Sprintf(",afade=t=out:st=%s:d=1", floatStr(fadeStart))
	}
	finalChain += "[outa]"

	filterComplex := musicIn + ";" + duckChain + ";" + finalChain

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

// isInterviewKind returns true for clip kinds where the operator wants the
// jumper's spoken audio preserved in the final mix. Everything else (intro,
// walk, freefall, landing, closing, custom_*) is "action" — the source audio
// is replaced with silence so only the music score is heard there. The
// silence ALSO becomes the sidechain duck trigger: zero level → no duck →
// music plays at full volume during action.
func isInterviewKind(kind string) bool {
	return kind == state.KindInterviewPre || kind == state.KindInterviewPlane
}

// trimAndNormalise runs ffmpeg with -ss/-to + scale-pad to bring an arbitrary
// source clip into the canonical 1080p H.264 + AAC stereo intermediate format
// that the concat demuxer can stitch with -c copy.
//
// Audio handling depends on clip kind:
//   • interview_pre / interview_plane: clip's own audio is kept (speech).
//   • everything else (intro / walk / freefall / landing / closing / custom):
//     audio is replaced with stereo silence via `anullsrc`. The downstream
//     mix stage uses the concatenated audio as a sidechain duck trigger —
//     muted action segments → no duck → music plays at full volume there.
//
// Frame-accurate -ss/-to AFTER -i (slower than fast-seek but correct — the
// before-input form snaps to keyframes and can over-cut by up to a GOP).
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

	keepRealAudio := c.HasAudio && isInterviewKind(c.Kind)

	var args []string
	if keepRealAudio {
		args = append([]string{
			"-nostats", "-hide_banner", "-y",
			"-i", c.SourcePath,
		}, commonOut...)
	} else {
		// Either the source has no audio at all (anullsrc fills in the silent
		// channel) OR it's an action clip whose audio we deliberately mute
		// (anullsrc replaces it). Same code path.
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

// segmentMeta pairs an intermediate file with its known trimmed duration —
// the xfade chain needs durations to compute per-clip offsets, and re-running
// ffprobe N times to read them is slower than just remembering what we already
// asked ffmpeg to render.
type segmentMeta struct {
	path     string
	duration float64
}

// effectiveDuration replicates Clip.EffectiveTrimOut() math but returns the
// LENGTH (out - in) instead of the absolute end time, since that's what the
// xfade offset calculator needs.
func effectiveDuration(c state.Clip) float64 {
	in := c.TrimInSeconds
	if in < 0 {
		in = 0
	}
	out := c.TrimOutSeconds
	if out <= 0 || out > c.DurationSeconds {
		out = c.DurationSeconds
	}
	return out - in
}

// concatWithCrossfades stitches the trimmed intermediates with a 0.5-second
// xfade (video) + acrossfade (audio) at every seam. Replaces the previous
// "concat demuxer + -c copy" path, which produced hard cuts.
//
// Returns the actual output duration in seconds. With crossfades the timeline
// is shorter than sum(d_i) by (N-1) * crossfadeDur — the caller (Stage C)
// uses this value to schedule the music's afade-out at the real end.
//
// Filter chain (3 clips example, durations d0 d1 d2):
//   [0:v][1:v] xfade=transition=fade:duration=0.5:offset=d0-0.5  [v01]
//   [v01][2:v] xfade=transition=fade:duration=0.5:offset=d0+d1-1 [v012]
//   [0:a][1:a] acrossfade=d=0.5                                  [a01]
//   [a01][2:a] acrossfade=d=0.5                                  [a012]
//   -map [v012] -map [a012]
//
// For very short clips we shrink the crossfade to clip_duration / 3 so the
// offset stays positive. Single-clip projects skip xfade entirely (copy).
func (r *Runner) concatWithCrossfades(ctx context.Context, segments []segmentMeta, dstPath string) (float64, error) {
	if len(segments) == 0 {
		return 0, errors.New("no segments to concatenate")
	}

	// Single-clip → just promote the intermediate to the output path.
	if len(segments) == 1 {
		if err := copyFileContents(segments[0].path, dstPath); err != nil {
			return 0, err
		}
		return segments[0].duration, nil
	}

	// Pick a crossfade duration that fits the SHORTEST clip — keeps offsets positive.
	crossfade := 0.5
	for _, s := range segments {
		if s.duration < crossfade*2 {
			c := s.duration / 3.0
			if c < crossfade {
				crossfade = c
			}
		}
	}
	if crossfade < 0.05 {
		// Some clip is < 0.15s — operator should fix the trim. Bail.
		return 0, errors.New("a clip is too short to crossfade (< 0.15s effective duration)")
	}

	// Build ffmpeg invocation: one -i per segment + a filter_complex chain.
	args := []string{"-nostats", "-hide_banner", "-y"}
	for _, s := range segments {
		args = append(args, "-i", s.path)
	}

	var fc strings.Builder
	prevV := "[0:v]"
	prevA := "[0:a]"
	cumulativeDur := segments[0].duration

	for i := 1; i < len(segments); i++ {
		offset := cumulativeDur - crossfade
		outV := fmt.Sprintf("[v%d]", i)
		outA := fmt.Sprintf("[a%d]", i)

		fc.WriteString(fmt.Sprintf(
			"%s[%d:v]xfade=transition=fade:duration=%s:offset=%s%s;",
			prevV, i, floatStr(crossfade), floatStr(offset), outV,
		))
		fc.WriteString(fmt.Sprintf(
			"%s[%d:a]acrossfade=d=%s%s;",
			prevA, i, floatStr(crossfade), outA,
		))

		prevV = outV
		prevA = outA
		// xfade overlap means timeline grows by (duration - crossfade), not duration.
		cumulativeDur += segments[i].duration - crossfade
	}
	filterComplex := strings.TrimSuffix(fc.String(), ";")

	args = append(args,
		"-filter_complex", filterComplex,
		"-map", prevV,
		"-map", prevA,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "192k",
		"-ar", "48000",
		"-movflags", "+faststart",
		dstPath,
	)

	cmd := exec.CommandContext(ctx, r.FFmpegPath, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("ffmpeg crossfade chain: %v\n%s", err, string(out))
	}
	return cumulativeDur, nil
}

// copyFileContents is a small file-copy shim used by the single-clip fast
// path. We don't os.Rename across the intermediates dir → projects dir
// boundary because both might be on different filesystems on some setups.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// =====================================================================
// helpers
// =====================================================================

func ptr[T any](v T) *T { return &v }

func floatStr(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
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
