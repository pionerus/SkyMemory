// Package pipeline orchestrates the FFmpeg-driven render of a project.
//
// The render is structured around two ffmpeg invocations:
//
//  1. renderSinglePass — one filter_complex chain that ingests every clip
//     file, per-input trims/scales/pads/fps-locks/format-pins, then
//     xfades+acrossfades them together, and finally fades-up-from-black
//     and fades-out-to-black. One decode + one filter pass + one encode.
//     Encoder is QuickSync (h264_qsv) when available, libx264 otherwise.
//
//  2. mixMusic — adds the picked music bed under the project audio,
//     dynamically ducked by sidechain compression triggered by the project
//     audio itself. Video is stream-copied (no re-encode), so this is a
//     quick mux. Skipped when the project has no music picked (we just
//     rename concat_only.mp4 to output_1080p.mp4).
//
// Final output: ~/.freefall-studio/jobs/<project_id>/output_1080p.mp4.
//
// 4K / vertical 1080×1920 outputs and photo extraction are roadmap items
// that will reuse renderSinglePass with different encoder args / filter tails.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pionerus/freefall/internal/studio/branding"
	"github.com/pionerus/freefall/internal/studio/ffprobe"
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

	// BrandingProvider resolves the tenant's watermark + intro/outro bundle.
	// Optional — when nil, renders run without any branding overlay.
	BrandingProvider BrandingProviderLike

	// FFmpegPath / FFprobePath are looked up from PATH on first Run if empty.
	FFmpegPath  string
	FFprobePath string

	// QSV (Intel QuickSync) detection — the probe runs once on the first
	// Run() call and caches the result for the process lifetime. When useQSV
	// is true, all encode stages route through h264_qsv; otherwise libx264.
	qsvOnce sync.Once
	useQSV  bool

	// Per-Run target frame size. Defaults to 1920×1080 — but Run() bumps it
	// to (2560, 1440) or (3840, 2160) when project.Output4K is on AND the
	// source clips are tall enough to justify it. We never UPSCALE: a 1080p
	// source rendered with Output4K still produces 1080p, just labelled as
	// 4K-eligible to the cloud.
	targetW, targetH int
}

// BrandingProviderLike abstracts internal/studio/branding.Cache so the
// pipeline doesn't tie itself to the exact cache implementation. The Run
// caller (cmd/studio) wires a concrete cache that closes over the studio's
// tenant ID, so this method takes no tenant argument.
type BrandingProviderLike interface {
	EnsureForRun(ctx context.Context) (branding.Bundle, error)
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
	r.detectQSV(ctx)
	encTag := "CPU"
	if r.useQSV {
		encTag = "QSV"
	}

	rawClips, err := r.DB.ListClips(ctx, projectID)
	if err != nil {
		return "", r.fail(ctx, generationID, "list clips: "+err.Error())
	}

	// Drop legacy intro/closing clips — those are now club-admin branding
	// inputs (concat'd via bundle.IntroPath / bundle.OutroPath below).
	// Old projects that have those uploaded would otherwise be rendered twice.
	clips := make([]state.Clip, 0, len(rawClips))
	for _, c := range rawClips {
		if state.IsLegacyBrandingKind(c.Kind) {
			log.Printf("pipeline: skipping legacy %q clip — using club-admin branding instead", c.Kind)
			continue
		}
		clips = append(clips, c)
	}
	if len(clips) == 0 {
		return "", r.fail(ctx, generationID, "no clips uploaded yet")
	}
	for _, c := range clips {
		if c.DurationSeconds <= 0 {
			return "", r.fail(ctx, generationID, fmt.Sprintf("clip %q has no duration metadata (was ffprobe missing on upload? re-upload to fix)", c.Kind))
		}
	}

	// Resolve target output resolution. Project.Output4K bumps it past 1080p
	// when source clips are tall enough; otherwise we stay at 1080p.
	{
		proj, perr := r.DB.GetProject(ctx, projectID)
		if perr == nil && proj != nil {
			r.targetW, r.targetH = chooseTargetDims(proj.Output4K, clips)
		} else {
			r.targetW, r.targetH = 1920, 1080
		}
		log.Printf("pipeline target: %dx%d (encoder=%s, output4k=%v)",
			r.targetW, r.targetH, encTag,
			func() bool { if proj != nil { return proj.Output4K }; return false }())
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

	// Progress-bar budget. The single render pass owns the bulk; the music
	// mux is short, and the final stat is instant.
	renderRange := pctRange{lo: 0, hi: 85}
	musicRange := pctRange{lo: 85, hi: 97}

	// === Render: one ffmpeg invocation = trim + normalise + xfade + fade + encode ===
	_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
		Status:      ptr(state.GenStatusTrimming),
		StepLabel:   ptr(fmt.Sprintf("rendering %d clips [%s]", len(clips), encTag)),
		ProgressPct: ptr(renderRange.lo),
	})

	concatOnly := filepath.Join(intermediatesDir, "concat_only.mp4")
	_ = os.Remove(concatOnly)
	renderProg := func(frac float64) {
		_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
			ProgressPct: ptr(renderRange.at(frac)),
		})
	}
	// Fetch all cut zones for this project once — pipeline turns each clip's
	// cuts into split+concat sub-ranges inside renderSinglePass.
	cutsByClip, err := r.DB.ListCutsForProject(ctx, projectID)
	if err != nil {
		return "", r.fail(ctx, generationID, "list cuts: "+err.Error())
	}

	// Resolve the tenant's branding bundle (watermark PNG + intro/outro mp4).
	// Best-effort — if the cloud is briefly unreachable, render without
	// branding rather than failing the whole job.
	var bundle branding.Bundle
	if r.BrandingProvider != nil {
		brandCtx, brandCancel := context.WithTimeout(ctx, 30*time.Second)
		b, berr := r.BrandingProvider.EnsureForRun(brandCtx)
		brandCancel()
		if berr != nil {
			fmt.Fprintf(os.Stderr, "branding: skipping overlay (%v)\n", berr)
		} else {
			bundle = b
		}
	}

	totalDur, err := r.renderSinglePass(ctx, clips, cutsByClip, bundle, concatOnly, renderProg)
	if err != nil {
		return "", r.fail(ctx, generationID, "render: "+err.Error())
	}

	// Output filename reflects actual rendered dims so a 4K render doesn't
	// silently overwrite the 1080p one. Cloud-side artifact `kind` is set
	// by the upload caller based on the same dims via OutputDims().
	finalOut := filepath.Join(projectDir, outputFilename(r.targetW, r.targetH))
	_ = os.Remove(finalOut) // overwrite previous

	// === Mix music (only if project has a picked track) ===
	project, err := r.DB.GetProject(ctx, projectID)
	if err != nil {
		return "", r.fail(ctx, generationID, "load project for music: "+err.Error())
	}

	if project.MusicTrackID > 0 && r.MusicCache != nil {
		_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
			Status:      ptr(state.GenStatusConcating),
			StepLabel:   ptr("downloading music + mixing"),
			ProgressPct: ptr(musicRange.lo),
		})

		musicPath, err := r.MusicCache.Ensure(ctx, project.MusicTrackID)
		if err != nil {
			return "", r.fail(ctx, generationID, "music download: "+err.Error())
		}

		musicProg := func(frac float64) {
			_ = r.DB.UpdateGeneration(ctx, generationID, state.GenerationPatch{
				ProgressPct: ptr(musicRange.at(frac)),
			})
		}
		// Use the ACTUAL post-xfade duration so the music fade-out lands on
		// the real end-of-video, not (N-1)*crossfade seconds past it.
		if err := r.mixMusic(ctx, concatOnly, musicPath, totalDur, finalOut, musicProg); err != nil {
			return "", r.fail(ctx, generationID, "music mix: "+err.Error())
		}
	} else {
		// No music picked: rename concat-only into the canonical output path.
		if err := os.Rename(concatOnly, finalOut); err != nil {
			return "", r.fail(ctx, generationID, "rename concat output: "+err.Error())
		}
	}

	// === Stat output, mark done ===
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
func (r *Runner) mixMusic(ctx context.Context, videoPath, musicPath string, totalSec float64, dstPath string, onProgress progressFn) error {
	musicIn := "[1:a]aloop=loop=-1:size=2e9,afade=t=in:st=0:d=1,volume=0.7[music_in]"
	duckChain := "[music_in][0:a]sidechaincompress=threshold=0.05:ratio=8:attack=20:release=400[duck]"

	finalChain := "[0:a][duck]amix=inputs=2:duration=first:dropout_transition=0"
	if totalSec > 1.5 {
		fadeStart := totalSec - 1.0
		finalChain += fmt.Sprintf(",afade=t=out:st=%s:d=1", floatStr(fadeStart))
	}
	finalChain += "[outa]"

	filterComplex := musicIn + ";" + duckChain + ";" + finalChain

	args := []string{
		"-hide_banner", "-y",
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
	}
	if err := r.runFFmpeg(ctx, args, totalSec, onProgress); err != nil {
		return fmt.Errorf("ffmpeg mix: %w", err)
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

// keepSeg is one [start, end] sub-range of a clip we keep — the gaps
// between cut zones, plus the head and tail of the trim window.
type keepSeg struct{ start, end float64 }

// writeClipVideoChain emits the per-clip video filter chain into fc.
// • 1 keep-segment (no cuts): single trim+setpts → scale → pad → fps → format.
// • N>1 keep-segments: split=N → N×(trim+setpts) → concat → scale → pad → fps → format.
//
// The input source label is "[<srcIdx>:v]"; the output label is "[v<idx>]".
// videoNormalizeChain returns the per-clip filter that maps a source frame
// onto the runner's current target resolution (default 1920×1080). Made a
// method so it can read r.targetW/r.targetH set per-Run().
func (r *Runner) videoNormalizeChain() string {
	w, h := r.outputDims()
	return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,"+
		"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,fps=30,format=yuv420p",
		w, h, w, h)
}

// outputDims returns the runner's chosen target W,H, falling back to 1080p
// when the per-Run setup hasn't run yet (defensive — every code path that
// uses this should go through Run()).
func (r *Runner) outputDims() (int, int) {
	if r.targetW > 0 && r.targetH > 0 {
		return r.targetW, r.targetH
	}
	return 1920, 1080
}

// OutputDims is the public wrapper so the studio's upload caller can ask
// "what kind of artifact did this render produce" without poking at
// private fields.
func (r *Runner) OutputDims() (int, int) { return r.outputDims() }

// outputFilename names the rendered output based on its frame size so a
// 4K render doesn't clobber the 1080p one in the same project dir.
func outputFilename(w, h int) string {
	switch {
	case h >= 2160:
		return "output_4k.mp4"
	case h >= 1440:
		return "output_2k.mp4"
	default:
		return "output_1080p.mp4"
	}
}

// chooseTargetDims picks the output resolution for this run. With 4K opt-in
// and a high-res source, it bumps to 1440p or 2160p; otherwise it stays at
// 1080p. Never upscales beyond what the source actually has — paying for
// 4K render time on 1080p source is a waste.
func chooseTargetDims(want4K bool, clips []state.Clip) (int, int) {
	if !want4K {
		return 1920, 1080
	}
	maxW := 0
	for _, c := range clips {
		if c.Width > maxW {
			maxW = c.Width
		}
	}
	switch {
	case maxW >= 3840:
		return 3840, 2160
	case maxW >= 2560:
		return 2560, 1440
	default:
		return 1920, 1080
	}
}

func (r *Runner) writeClipVideoChain(fc *strings.Builder, srcIdx, idx int, segs []keepSeg) {
	normChain := r.videoNormalizeChain()
	src := fmt.Sprintf("[%d:v]", srcIdx)
	out := fmt.Sprintf("[v%d]", idx)

	if len(segs) == 1 {
		s := segs[0]
		fc.WriteString(fmt.Sprintf(
			"%strim=start=%s:end=%s,setpts=PTS-STARTPTS,%s%s;",
			src, floatStr(s.start), floatStr(s.end), normChain, out,
		))
		return
	}

	// Split source into N branches, trim each, concat, then normalize.
	n := len(segs)
	splitOuts := make([]string, n)
	for k := range segs {
		splitOuts[k] = fmt.Sprintf("[v%d_s%d]", idx, k)
	}
	fc.WriteString(fmt.Sprintf("%ssplit=%d%s;", src, n, strings.Join(splitOuts, "")))

	trimmedOuts := make([]string, n)
	for k, s := range segs {
		trimmedOuts[k] = fmt.Sprintf("[v%d_t%d]", idx, k)
		fc.WriteString(fmt.Sprintf(
			"%strim=start=%s:end=%s,setpts=PTS-STARTPTS%s;",
			splitOuts[k], floatStr(s.start), floatStr(s.end), trimmedOuts[k],
		))
	}

	concatLbl := fmt.Sprintf("[v%d_c]", idx)
	fc.WriteString(fmt.Sprintf("%sconcat=n=%d:v=1:a=0%s;",
		strings.Join(trimmedOuts, ""), n, concatLbl,
	))
	fc.WriteString(fmt.Sprintf("%s%s%s;", concatLbl, normChain, out))
}

// writeClipAudioChainFromSource is the audio counterpart — used when the clip
// keeps source audio (interview kinds). asplit + atrim + concat (a=1) → aformat.
const audioNormalizeChain = "aformat=channel_layouts=stereo:sample_rates=48000"

func writeClipAudioChainFromSource(fc *strings.Builder, src string, idx int, segs []keepSeg) {
	out := fmt.Sprintf("[a%d]", idx)

	if len(segs) == 1 {
		s := segs[0]
		fc.WriteString(fmt.Sprintf(
			"%satrim=start=%s:end=%s,asetpts=PTS-STARTPTS,%s%s;",
			src, floatStr(s.start), floatStr(s.end), audioNormalizeChain, out,
		))
		return
	}

	n := len(segs)
	splitOuts := make([]string, n)
	for k := range segs {
		splitOuts[k] = fmt.Sprintf("[a%d_s%d]", idx, k)
	}
	fc.WriteString(fmt.Sprintf("%sasplit=%d%s;", src, n, strings.Join(splitOuts, "")))

	trimmedOuts := make([]string, n)
	for k, s := range segs {
		trimmedOuts[k] = fmt.Sprintf("[a%d_t%d]", idx, k)
		fc.WriteString(fmt.Sprintf(
			"%satrim=start=%s:end=%s,asetpts=PTS-STARTPTS%s;",
			splitOuts[k], floatStr(s.start), floatStr(s.end), trimmedOuts[k],
		))
	}

	concatLbl := fmt.Sprintf("[a%d_c]", idx)
	fc.WriteString(fmt.Sprintf("%sconcat=n=%d:v=0:a=1%s;",
		strings.Join(trimmedOuts, ""), n, concatLbl,
	))
	fc.WriteString(fmt.Sprintf("%s%s%s;", concatLbl, audioNormalizeChain, out))
}

// writeClipAudioChainHybrid emits a clip's audio output [aN] when the clip
// has a speech-start marker. Audio is silence before the marker, source
// audio after. Cuts in keepSegs are honoured by both halves.
//
//   srcAudioRef  : "[<clipIdx>:a]"  — source audio of the clip
//   silAudioRef  : "[<silIdx>:a]"   — pre-registered anullsrc of >= effDur seconds
//   splitAt      : source-clip seconds where speech begins (same scale as keepSegs)
func writeClipAudioChainHybrid(fc *strings.Builder, srcAudioRef, silAudioRef string,
	idx int, segs []keepSeg, splitAt float64) {
	out := fmt.Sprintf("[a%d]", idx)

	type subSeg struct {
		from string  // "src" or "sil"
		s, e float64 // seconds in source-clip space (or local for sil — doesn't matter for atrim length)
	}

	// Walk keepSegs and break each one across the marker. Silence sub-segs
	// use cumulative-offset within the anullsrc input (since anullsrc starts
	// at t=0), source sub-segs use the original clip seconds.
	var subs []subSeg
	silOffset := 0.0
	for _, s := range segs {
		a, b := s.start, s.end
		if b <= splitAt {
			subs = append(subs, subSeg{from: "sil", s: silOffset, e: silOffset + (b - a)})
			silOffset += (b - a)
		} else if a >= splitAt {
			subs = append(subs, subSeg{from: "src", s: a, e: b})
		} else {
			subs = append(subs, subSeg{from: "sil", s: silOffset, e: silOffset + (splitAt - a)})
			silOffset += (splitAt - a)
			subs = append(subs, subSeg{from: "src", s: splitAt, e: b})
		}
	}

	// Count sub-segs per source so we asplit each input the right number of times.
	srcN, silN := 0, 0
	for _, ss := range subs {
		if ss.from == "src" {
			srcN++
		} else {
			silN++
		}
	}

	// asplit the source audio into srcN branches (skip if 0, e.g. marker at trim_out edge).
	srcOuts := make([]string, srcN)
	for k := 0; k < srcN; k++ {
		srcOuts[k] = fmt.Sprintf("[a%d_src%d]", idx, k)
	}
	if srcN > 0 {
		fc.WriteString(fmt.Sprintf("%sasplit=%d%s;", srcAudioRef, srcN, strings.Join(srcOuts, "")))
	}
	silOuts := make([]string, silN)
	for k := 0; k < silN; k++ {
		silOuts[k] = fmt.Sprintf("[a%d_sil%d]", idx, k)
	}
	if silN > 0 {
		fc.WriteString(fmt.Sprintf("%sasplit=%d%s;", silAudioRef, silN, strings.Join(silOuts, "")))
	}

	// atrim each sub-seg.
	srcK, silK := 0, 0
	trimmedOuts := make([]string, len(subs))
	for k, ss := range subs {
		lbl := fmt.Sprintf("[a%d_t%d]", idx, k)
		trimmedOuts[k] = lbl
		if ss.from == "src" {
			fc.WriteString(fmt.Sprintf(
				"%satrim=start=%s:end=%s,asetpts=PTS-STARTPTS%s;",
				srcOuts[srcK], floatStr(ss.s), floatStr(ss.e), lbl,
			))
			srcK++
		} else {
			fc.WriteString(fmt.Sprintf(
				"%satrim=start=%s:end=%s,asetpts=PTS-STARTPTS%s;",
				silOuts[silK], floatStr(ss.s), floatStr(ss.e), lbl,
			))
			silK++
		}
	}

	if len(subs) == 1 {
		fc.WriteString(fmt.Sprintf("%s%s%s;", trimmedOuts[0], audioNormalizeChain, out))
		return
	}
	concatLbl := fmt.Sprintf("[a%d_c]", idx)
	fc.WriteString(fmt.Sprintf("%sconcat=n=%d:v=0:a=1%s;",
		strings.Join(trimmedOuts, ""), len(subs), concatLbl,
	))
	fc.WriteString(fmt.Sprintf("%s%s%s;", concatLbl, audioNormalizeChain, out))
}

// computeKeepSegments turns a trim window + sorted cut zones into the list
// of sub-ranges to keep. Cuts that sit outside the trim window are clipped
// to its bounds; zero-width cuts are dropped silently.
func computeKeepSegments(trimIn, trimOut float64, cuts []state.Cut) []keepSeg {
	var segs []keepSeg
	cursor := trimIn
	for _, c := range cuts {
		cs := c.StartSeconds
		if cs < trimIn {
			cs = trimIn
		}
		ce := c.EndSeconds
		if ce > trimOut {
			ce = trimOut
		}
		if ce <= cs {
			continue
		}
		if cs > cursor {
			segs = append(segs, keepSeg{cursor, cs})
		}
		if ce > cursor {
			cursor = ce
		}
	}
	if cursor < trimOut {
		segs = append(segs, keepSeg{cursor, trimOut})
	}
	return segs
}

// renderSinglePass performs the entire video assembly in one ffmpeg call:
// per-input trim/scale/pad/fps-lock + xfade chain + tenant branding overlay
// + intro/outro concat + opening/closing fade, then a single h264 encode.
// One decode + one filter pass + one encode — no intermediate per-clip
// files, no double-decoding the same content.
//
// Audio handling per clip:
//   • Interview clips with audio (interview_pre, interview_plane) keep their
//     source audio, trimmed and resampled to 48k stereo inside filter_complex.
//   • Everything else (action clips, or interview clips without audio) gets
//     a fresh anullsrc input of exactly the trimmed length, so the assembled
//     audio is silent during action segments. mixMusic later uses that
//     silence as the sidechain duck trigger.
//
// Cut zones (clip_cuts table) split the trim window into N+1 keep-segments;
// the per-clip filter chain becomes split → N trim+setpts branches → concat.
// effDur (sum of keep lengths) is what xfade offsets are computed against,
// not the raw trim_out - trim_in.
//
// Branding (Phase 6): when bundle.HasWatermark, the watermark PNG is overlaid
// on the assembled main timeline ONLY (not on intro/outro — those are the
// tenant's own branded clips and don't need decoration). When bundle.HasIntro
// / HasOutro, the supplied mp4s are normalised to the same 1920×1080 30fps
// stereo 48k canon and concatenated to either side of the main timeline.
// The opening fade-in / closing fade-out land on the very first / very last
// frame of the assembled output (so they cover intro/outro too).
//
// Returns the actual post-everything timeline duration (intro + main +
// outro), used by mixMusic to time the music fade-out.
func (r *Runner) renderSinglePass(
	ctx context.Context,
	clips []state.Clip,
	cutsByClipID map[int64][]state.Cut,
	bundle branding.Bundle,
	dstPath string,
	onProgress progressFn,
) (float64, error) {
	if len(clips) == 0 {
		return 0, errors.New("no clips to render")
	}

	// Pre-compute trim bounds + keep-segments + effective length per clip.
	type clipMeta struct {
		in, out  float64
		keepSegs []keepSeg
		effDur   float64
	}
	metas := make([]clipMeta, len(clips))
	for i, c := range clips {
		in := c.TrimInSeconds
		if in < 0 {
			in = 0
		}
		out := c.TrimOutSeconds
		if out <= 0 || out > c.DurationSeconds {
			out = c.DurationSeconds
		}
		if out <= in {
			return 0, fmt.Errorf("clip %s: empty trim window (trim_out <= trim_in)", c.Kind)
		}

		segs := computeKeepSegments(in, out, cutsByClipID[c.ID])
		if len(segs) == 0 {
			return 0, fmt.Errorf("clip %s: cut zones cover the entire trim window — remove a cut to render", c.Kind)
		}
		var eff float64
		for _, s := range segs {
			eff += s.end - s.start
		}
		if eff < 0.05 {
			return 0, fmt.Errorf("clip %s: less than 0.05s remains after cuts", c.Kind)
		}
		metas[i] = clipMeta{in: in, out: out, keepSegs: segs, effDur: eff}
	}

	// === Build inputs ===
	// For each clip, append `-i clip.mp4`. We then decide a per-clip audio
	// mode: pure source (interview), pure silence (action without marker),
	// or hybrid (action with operator-placed speech_start marker — silent
	// before, source after). Hybrid clips need BOTH the clip's source
	// audio and an anullsrc lavfi input.
	args := []string{"-hide_banner", "-y"}
	videoIdx := make([]int, len(clips))
	audioRef := make([]string, len(clips))    // "[X:a]" stream reference per clip
	silenceRef := make([]string, len(clips))  // "[Y:a]" anullsrc reference (hybrid + silence modes)
	type audioMode int
	const (
		modeSilence audioMode = iota // anullsrc only
		modeSource                   // [clipIdx:a] only
		modeHybrid                   // both — split at speech_start marker
	)
	modes := make([]audioMode, len(clips))
	nextInputIdx := 0
	for i, c := range clips {
		args = append(args, "-i", c.SourcePath)
		videoIdx[i] = nextInputIdx
		clipAudioRef := fmt.Sprintf("[%d:a]", videoIdx[i])
		nextInputIdx++

		// Decide the mode. Speech-start marker only meaningful on action
		// clips that actually have audio — interview clips already keep
		// full source audio so the marker would be a no-op.
		switch {
		case c.HasAudio && isInterviewKind(c.Kind):
			modes[i] = modeSource
			audioRef[i] = clipAudioRef
		case c.HasAudio && c.HasSpeechStart():
			modes[i] = modeHybrid
			audioRef[i] = clipAudioRef
			args = append(args,
				"-f", "lavfi",
				"-i", fmt.Sprintf(
					"anullsrc=channel_layout=stereo:sample_rate=48000:d=%s",
					floatStr(metas[i].effDur),
				),
			)
			silenceRef[i] = fmt.Sprintf("[%d:a]", nextInputIdx)
			nextInputIdx++
		default:
			modes[i] = modeSilence
			args = append(args,
				"-f", "lavfi",
				"-i", fmt.Sprintf(
					"anullsrc=channel_layout=stereo:sample_rate=48000:d=%s",
					floatStr(metas[i].effDur),
				),
			)
			audioRef[i] = fmt.Sprintf("[%d:a]", nextInputIdx)
			silenceRef[i] = audioRef[i]
			nextInputIdx++
		}
	}

	// === Probe + register branding inputs (intro / outro / watermark PNG) ===
	// These come AFTER the clip+anullsrc inputs so existing index math stays
	// untouched. ffprobe gives us each clip's duration (for totalDur arithmetic)
	// and HasAudio (so silent intros get an anullsrc fallback).
	type bumperInput struct {
		videoIdx, audioIdx int     // -1 audioIdx means "use [videoIdx:a]"
		duration           float64 // used to extend totalDur and time the music fade
	}
	var introIn, outroIn *bumperInput

	if bundle.HasIntro() {
		meta, perr := ffprobe.Probe(ctx, bundle.IntroPath)
		if perr != nil {
			return 0, fmt.Errorf("ffprobe intro: %w", perr)
		}
		bi := bumperInput{duration: meta.DurationSeconds}
		args = append(args, "-i", bundle.IntroPath)
		bi.videoIdx = nextInputIdx
		nextInputIdx++
		if meta.HasAudio {
			bi.audioIdx = bi.videoIdx
		} else {
			args = append(args, "-f", "lavfi", "-i", fmt.Sprintf(
				"anullsrc=channel_layout=stereo:sample_rate=48000:d=%s",
				floatStr(meta.DurationSeconds),
			))
			bi.audioIdx = nextInputIdx
			nextInputIdx++
		}
		introIn = &bi
	}
	if bundle.HasOutro() {
		meta, perr := ffprobe.Probe(ctx, bundle.OutroPath)
		if perr != nil {
			return 0, fmt.Errorf("ffprobe outro: %w", perr)
		}
		bi := bumperInput{duration: meta.DurationSeconds}
		args = append(args, "-i", bundle.OutroPath)
		bi.videoIdx = nextInputIdx
		nextInputIdx++
		if meta.HasAudio {
			bi.audioIdx = bi.videoIdx
		} else {
			args = append(args, "-f", "lavfi", "-i", fmt.Sprintf(
				"anullsrc=channel_layout=stereo:sample_rate=48000:d=%s",
				floatStr(meta.DurationSeconds),
			))
			bi.audioIdx = nextInputIdx
			nextInputIdx++
		}
		outroIn = &bi
	}
	wmInputIdx := -1
	if bundle.HasWatermark() {
		args = append(args, "-i", bundle.WatermarkPath)
		wmInputIdx = nextInputIdx
		nextInputIdx++
	}

	// === Build filter_complex ===
	var fc strings.Builder

	// Per-clip normalise.
	// Without cuts (1 keep-segment): single trim+setpts → scale → pad → fps → format.
	// With cuts (N>1 keep-segments): split=N → N×(trim+setpts) → concat → scale...
	for i, c := range clips {
		m := metas[i]
		r.writeClipVideoChain(&fc, videoIdx[i], i, m.keepSegs)
		switch modes[i] {
		case modeSource:
			writeClipAudioChainFromSource(&fc, audioRef[i], i, m.keepSegs)
		case modeHybrid:
			writeClipAudioChainHybrid(&fc, audioRef[i], silenceRef[i], i, m.keepSegs, c.SpeechStartSeconds)
		default: // modeSilence
			fc.WriteString(fmt.Sprintf(
				"%saformat=channel_layouts=stereo:sample_rates=48000[a%d];",
				audioRef[i], i,
			))
		}
	}

	// === xfade / acrossfade chain → produces [mainV][mainA] + mainDur ===
	// All durations here are POST-CUT effective durations (effDur), not the
	// raw trim window — xfade offsets must match what each branch actually
	// emits after split+concat.
	var mainV, mainA string
	var mainDur float64

	if len(clips) == 1 {
		mainV = "[v0]"
		mainA = "[a0]"
		mainDur = metas[0].effDur
	} else {
		// Pick a crossfade that fits the shortest clip — keeps offsets positive.
		crossfade := 0.5
		for _, m := range metas {
			if m.effDur < crossfade*2 {
				if c := m.effDur / 3.0; c < crossfade {
					crossfade = c
				}
			}
		}
		if crossfade < 0.05 {
			return 0, errors.New("a clip is too short to crossfade (< 0.15s effective duration)")
		}

		prevV := "[v0]"
		prevA := "[a0]"
		cumulative := metas[0].effDur
		for i := 1; i < len(clips); i++ {
			offset := cumulative - crossfade
			outV := fmt.Sprintf("[xv%d]", i)
			outA := fmt.Sprintf("[xa%d]", i)
			fc.WriteString(fmt.Sprintf(
				"%s[v%d]xfade=transition=fade:duration=%s:offset=%s%s;",
				prevV, i, floatStr(crossfade), floatStr(offset), outV,
			))
			fc.WriteString(fmt.Sprintf(
				"%s[a%d]acrossfade=d=%s%s;",
				prevA, i, floatStr(crossfade), outA,
			))
			prevV = outV
			prevA = outA
			cumulative += metas[i].effDur - crossfade
		}
		mainV = prevV
		mainA = prevA
		mainDur = cumulative
	}

	// === Watermark overlay on main timeline (intro/outro stay clean) ===
	// scale=W:-1 keeps aspect; format=rgba + colorchannelmixer applies opacity
	// without affecting the underlying video colour space. format=auto on the
	// overlay means ffmpeg picks yuv420p so the encoder is happy.
	if bundle.HasWatermark() && wmInputIdx >= 0 {
		targetW, _ := r.outputDims()
		wmW := targetW * bundle.WatermarkSizePct / 100
		if wmW < 32 {
			wmW = 32
		}
		opacity := float64(bundle.WatermarkOpacityPct) / 100.0
		const margin = 24
		var posExpr string
		switch bundle.WatermarkPosition {
		case "bottom-left":
			posExpr = fmt.Sprintf("x=%d:y=main_h-overlay_h-%d", margin, margin)
		case "top-right":
			posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=%d", margin, margin)
		case "top-left":
			posExpr = fmt.Sprintf("x=%d:y=%d", margin, margin)
		default: // bottom-right is the documented default
			posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=main_h-overlay_h-%d", margin, margin)
		}
		fc.WriteString(fmt.Sprintf(
			"[%d:v]scale=%d:-1,format=rgba,colorchannelmixer=aa=%s[wmready];",
			wmInputIdx, wmW, floatStr(opacity),
		))
		fc.WriteString(fmt.Sprintf(
			"%s[wmready]overlay=%s:format=auto[mainVwm];",
			mainV, posExpr,
		))
		mainV = "[mainVwm]"
	}

	// === Intro / outro normalise + concat onto the main timeline ===
	// Branding clips are uploaded by the operator as arbitrary mp4s. Run them
	// through the same Stage A normalisation chain (1920×1080 30fps + 48 kHz
	// stereo) so concat=v=1:a=1 sees identical formats. The watermark is NOT
	// re-applied here — bumpers carry their own branding.
	finalV := mainV
	finalA := mainA
	totalDur := mainDur
	if introIn != nil || outroIn != nil {
		var concatParts []string

		if introIn != nil {
			fc.WriteString(fmt.Sprintf("[%d:v]%s[iv];", introIn.videoIdx, r.videoNormalizeChain()))
			fc.WriteString(fmt.Sprintf("[%d:a]%s[ia];", introIn.audioIdx, audioNormalizeChain))
			concatParts = append(concatParts, "[iv]", "[ia]")
			totalDur += introIn.duration
		}
		concatParts = append(concatParts, mainV, mainA)
		if outroIn != nil {
			fc.WriteString(fmt.Sprintf("[%d:v]%s[ov];", outroIn.videoIdx, r.videoNormalizeChain()))
			fc.WriteString(fmt.Sprintf("[%d:a]%s[oa];", outroIn.audioIdx, audioNormalizeChain))
			concatParts = append(concatParts, "[ov]", "[oa]")
			totalDur += outroIn.duration
		}
		n := len(concatParts) / 2
		fc.WriteString(fmt.Sprintf("%sconcat=n=%d:v=1:a=1[fullV][fullA];",
			strings.Join(concatParts, ""), n,
		))
		finalV = "[fullV]"
		finalA = "[fullA]"
	}

	// === Opening + closing fade on the fully assembled stream ===
	if totalDur > 2*finalFadeDur {
		fadeOutStart := totalDur - finalFadeDur
		fc.WriteString(fmt.Sprintf(
			"%sfade=t=in:st=0:d=%s,fade=t=out:st=%s:d=%s[vfinal];",
			finalV, floatStr(finalFadeDur), floatStr(fadeOutStart), floatStr(finalFadeDur),
		))
		fc.WriteString(fmt.Sprintf(
			"%safade=t=in:st=0:d=%s,afade=t=out:st=%s:d=%s[afinal];",
			finalA, floatStr(finalFadeDur), floatStr(fadeOutStart), floatStr(finalFadeDur),
		))
		finalV = "[vfinal]"
		finalA = "[afinal]"
	}

	// === Build encode args. Try QSV first when available; on a
	// QSV-specific encoder error we fall back to libx264 inline so the
	// operator gets a finished render in one click instead of having to
	// retry. ===
	baseFC := fc.String() // snapshot before we append the final format step
	build := func(useQSV bool) (string, []string) {
		// Force the encoder's expected pix_fmt at the very end of the
		// graph. QSV is happiest with nv12; libx264 accepts both but the
		// CPU path was tested with yuv420p so keep that.
		pixFmt := "yuv420p"
		if useQSV {
			pixFmt = "nv12"
		}
		full := baseFC + finalV + "format=" + pixFmt + "[venc];"
		filterComplex := strings.TrimSuffix(full, ";")

		args := append([]string{}, args...) // copy parent args (inputs)
		args = append(args,
			"-filter_complex", filterComplex,
			"-map", "[venc]",
			"-map", finalA,
			// Constant framerate. With sources at 50fps + 29.97fps + lavfi
			// inputs the default vfr emits jittered timestamps that QSV's
			// frame-type tracker rejects.
			"-fps_mode", "cfr",
		)
		// Inline encoder args (we toggle useQSV per attempt without
		// touching r.useQSV until we know the result).
		if useQSV {
			args = append(args,
				"-c:v", "h264_qsv",
				"-preset", "veryfast",
				"-global_quality", "22",
				"-pix_fmt", "nv12",
				"-look_ahead", "0",
			)
		} else {
			args = append(args,
				"-c:v", "libx264",
				"-preset", "veryfast",
				"-crf", "20",
				"-pix_fmt", "yuv420p",
			)
		}
		args = append(args,
			"-c:a", "aac",
			"-b:a", "192k",
			"-ar", "48000",
			"-movflags", "+faststart",
			dstPath,
		)
		return filterComplex, args
	}

	// Attempt 1: whatever encoder the auto-detect picked.
	useQSV := r.useQSV
	_, args1 := build(useQSV)
	err := r.runFFmpeg(ctx, args1, totalDur, onProgress)
	if err == nil {
		return totalDur, nil
	}
	// Attempt 2: QSV failed → swap to libx264 and retry once.
	if useQSV && isQSVEncoderError(err.Error()) {
		log.Printf("QSV encode failed: %v", err)
		log.Printf("retrying with libx264 (CPU encoder) — QSV disabled for the rest of this studio session")
		r.useQSV = false // remember for the rest of the session
		_, args2 := build(false)
		if err2 := r.runFFmpeg(ctx, args2, totalDur, onProgress); err2 != nil {
			return 0, fmt.Errorf("single-pass render (libx264 fallback): %w", err2)
		}
		log.Printf("libx264 fallback succeeded")
		return totalDur, nil
	}
	if useQSV {
		log.Printf("QSV ffmpeg error did not match isQSVEncoderError pattern, no fallback attempted: %v", err)
	}
	return 0, fmt.Errorf("single-pass render: %w", err)
}

// =====================================================================
// helpers
// =====================================================================

func ptr[T any](v T) *T { return &v }

func floatStr(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// detectQSV runs once on the first Run() call. It tries to encode a tiny
// 320×240 test pattern with h264_qsv to /dev/null — if that succeeds, the
// machine has working Intel QuickSync. Otherwise we stay on libx264.
//
// Probing this way (real encode, not just `-h encoder=h264_qsv`) catches the
// case where ffmpeg is compiled with QSV but the iGPU driver / runtime is
// unavailable, broken, or already in use by another process.
func (r *Runner) detectQSV(ctx context.Context) {
	r.qsvOnce.Do(func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, r.FFmpegPath,
			"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=0.5:size=320x240:rate=10",
			"-c:v", "h264_qsv",
			"-pix_fmt", "nv12",
			"-look_ahead", "0",
			"-f", "null", "-",
		)
		if err := cmd.Run(); err == nil {
			r.useQSV = true
		}
	})
}

// EncoderName returns "QSV" or "CPU" depending on what the runner will use
// for the next render. Triggers the lazy QSV probe if it hasn't run yet so
// callers (e.g. the studio settings dialog) get an accurate answer at any
// time, not just after the first Generate click.
func (r *Runner) EncoderName(ctx context.Context) string {
	if r.FFmpegPath == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			r.FFmpegPath = p
		}
	}
	if r.FFmpegPath != "" {
		r.detectQSV(ctx)
	}
	if r.useQSV {
		return "QSV"
	}
	return "CPU"
}

// videoEncArgs returns the encoder flags every encode stage uses. Stage A
// (per-clip normalise), Stage B (xfade chain), and the single-clip Stage B
// fade-pass all want the same encoder choice — keep it in one place.
//
// QSV branch: hardware H.264 on Intel iGPU. global_quality 22 ≈ libx264 crf
// 22 in perceived quality but ~3-5× faster on UHD-class iGPUs.
// libx264 fallback: kept conservative (veryfast crf 20) for machines without
// working QuickSync.
//
// Note we used to ship `-low_power 1` here for an extra 1.5-2× via VDENC,
// but that path produces "Invalid FrameType:0 / Error encoding a frame"
// on this dev box (UHD 620, mixed 50fps + 29.97fps + lavfi inputs). The
// default PAK encoder works around it. Real-world cost: ~30% slower
// QSV; still hardware-accelerated, still QSV.
func (r *Runner) videoEncArgs() []string {
	if r.useQSV {
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-global_quality", "22",
			"-pix_fmt", "nv12",
			"-look_ahead", "0",
		}
	}
	return []string{
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
	}
}

// isQSVEncoderError tells if the ffmpeg error string looks like a QSV
// encoder failure (driver/iGPU specific) rather than a general filter
// problem. These errors mean "QSV broke; try libx264 instead", not
// "the user's inputs are wrong".
func isQSVEncoderError(errMsg string) bool {
	for _, needle := range []string{
		"h264_qsv",
		"Invalid FrameType",
		"Error submitting video frame",
		"MFX_ERR",
	} {
		if strings.Contains(errMsg, needle) {
			return true
		}
	}
	return false
}

// Sentinel for callers that need a no-op duration to wait between progress
// pings. Kept here so handler tests don't sprinkle magic numbers.
const ProgressPollInterval = 1 * time.Second

// finalFadeDur is the length of the opening fade-in (from black) and the
// closing fade-out (to black) applied to the assembled output. Stage B owns
// these fades so both the music and no-music branches inherit them.
const finalFadeDur = 1.0
