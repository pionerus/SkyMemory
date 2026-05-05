// Package trim owns auto-suggestion of in/out trim windows (and, for landing,
// a speech-start marker) per segment kind. Called automatically by the upload
// handler so the operator lands on a pre-trimmed rail; manual "Auto-trim"
// button re-runs the same logic on demand.
//
// Heuristics:
//
//   interview_pre / interview_plane — silencedetect at -30 dB / 0.3 s.
//                                     Trim leading/trailing silence + 0.5s pad.
//                                     No cap — keep all speech.
//   freefall                        — audio-RMS wind onset (loud roar) +
//                                     wind drop (canopy open). Falls back to
//                                     positional skip-2/take-30 if no audio.
//   landing                         — audio-RMS impact spike + smart silence-
//                                     bounded tail. Auto-places the
//                                     speech-start marker so music ducks
//                                     under the post-landing interview.
//   walk                            — middle 8s of clip (positional).
//   intro / closing (legacy)        — silencedetect with 15s cap; UI no longer
//                                     exposes these slots, branding bundle
//                                     supplies them.
//   custom_*                        — silencedetect with 15s cap.
package trim

import (
	"context"
	"fmt"
	"math"

	"github.com/pionerus/freefall/internal/studio/ffmpeg"
	"github.com/pionerus/freefall/internal/studio/state"
)

// Suggestion is what the auto-trimmer returns. TrimIn and TrimOut are seconds
// from the start of the source clip. Reason is a one-liner explaining the
// heuristic that was used — surfaced in the UI so the operator knows whether
// to trust it. SpeechStart > 0 only when the heuristic also detected a
// post-action speech onset (currently only the landing heuristic sets it);
// callers should persist via state.UpdateClipSpeechStart so the pipeline
// ducks music under voice from that timestamp.
type Suggestion struct {
	TrimIn      float64
	TrimOut     float64
	Reason      string
	SpeechStart float64
}

// Suggest dispatches to the per-kind heuristic.
//
// `clip` provides duration + has_audio + kind; we don't re-query the DB here so
// callers (cmd/studio handler) can validate first and reject early if a clip
// has no usable duration.
func Suggest(ctx context.Context, clip *state.Clip) (Suggestion, error) {
	if clip == nil {
		return Suggestion{}, fmt.Errorf("clip is nil")
	}
	if clip.DurationSeconds <= 0 {
		return Suggestion{}, fmt.Errorf("clip has no duration metadata — re-upload to refresh")
	}

	switch clip.Kind {
	case state.KindIntro, state.KindClosing:
		return suggestSilenceBound(ctx, clip, 15.0)

	case state.KindInterviewPre, state.KindInterviewPlane:
		return suggestInterviewSpeechBounds(ctx, clip)

	case state.KindWalk:
		return suggestMiddleWindow(clip, 8.0, "Middle 8s of clip"), nil

	case state.KindFreefall:
		return suggestFreefallByWindRMS(ctx, clip)

	case state.KindLanding:
		return suggestLandingSmart(ctx, clip)
	}

	// Custom segment — generic silence-bound trim, max 15s like intro.
	if isCustomKind(clip.Kind) {
		return suggestSilenceBound(ctx, clip, 15.0)
	}
	return Suggestion{}, fmt.Errorf("no heuristic for kind %q", clip.Kind)
}

// =====================================================================
// Per-kind heuristics
// =====================================================================

// suggestSilenceBound trims silence from both ends and caps at maxSec.
// Uses silencedetect with -30dB / 0.5s minimum.
func suggestSilenceBound(ctx context.Context, clip *state.Clip, maxSec float64) (Suggestion, error) {
	if !clip.HasAudio {
		// No audio at all — can't silence-detect. Fall back to "first maxSec".
		return Suggestion{
			TrimIn:  0,
			TrimOut: math.Min(clip.DurationSeconds, maxSec),
			Reason:  fmt.Sprintf("Clip has no audio; capped to first %.0fs", maxSec),
		}, nil
	}

	windows, err := ffmpeg.DetectSilence(ctx, clip.SourcePath, -30, 0.5)
	if err != nil {
		return Suggestion{}, fmt.Errorf("silencedetect: %w", err)
	}

	in := 0.0
	out := clip.DurationSeconds

	// Leading silence: if a window starts at ~0, trim_in = its end.
	for _, w := range windows {
		if w.Start <= 0.05 {
			in = w.End
			break
		}
	}

	// Trailing silence: if a window ends at ~clip end (or the parser had no
	// silence_end and Start==End at near-end), trim_out = its start.
	for i := len(windows) - 1; i >= 0; i-- {
		w := windows[i]
		end := w.End
		if end <= w.Start {
			end = clip.DurationSeconds // unmatched silence_start at clip tail
		}
		if end >= clip.DurationSeconds-0.05 {
			out = w.Start
			break
		}
	}

	if out <= in {
		return Suggestion{
			TrimIn:  0,
			TrimOut: math.Min(clip.DurationSeconds, maxSec),
			Reason:  "Silence detection inconclusive; capped to first " + fmtSec(maxSec),
		}, nil
	}

	// Cap to max. If the speech window is longer than max, keep its start.
	if out-in > maxSec {
		out = in + maxSec
	}

	return Suggestion{
		TrimIn:  in,
		TrimOut: out,
		Reason:  fmt.Sprintf("silencedetect -30dB: kept %s of speech, dropped silence at edges", fmtSec(out-in)),
	}, nil
}

// suggestMiddleWindow centers a fixed-length window in the clip.
func suggestMiddleWindow(clip *state.Clip, length float64, reason string) Suggestion {
	if clip.DurationSeconds <= length {
		return Suggestion{TrimIn: 0, TrimOut: clip.DurationSeconds, Reason: "Clip shorter than window; using full"}
	}
	in := (clip.DurationSeconds - length) / 2.0
	return Suggestion{TrimIn: in, TrimOut: in + length, Reason: reason}
}

// suggestSkipThenWindow skips an opening prefix, then takes up to length more
// (or whatever remains).
func suggestSkipThenWindow(clip *state.Clip, skip, length float64) Suggestion {
	if clip.DurationSeconds <= skip+0.5 {
		return Suggestion{TrimIn: 0, TrimOut: clip.DurationSeconds, Reason: "Clip too short to skip; using full"}
	}
	in := skip
	out := math.Min(clip.DurationSeconds, in+length)
	return Suggestion{
		TrimIn:  in,
		TrimOut: out,
		Reason:  fmt.Sprintf("Skipped first %s, kept next %s of action", fmtSec(skip), fmtSec(out-in)),
	}
}

// suggestTailWindow keeps the last `length` seconds (used for landing).
func suggestTailWindow(clip *state.Clip, length float64, reason string) Suggestion {
	if clip.DurationSeconds <= length {
		return Suggestion{TrimIn: 0, TrimOut: clip.DurationSeconds, Reason: "Clip shorter than window; using full"}
	}
	in := clip.DurationSeconds - length
	return Suggestion{TrimIn: in, TrimOut: clip.DurationSeconds, Reason: reason}
}

// =====================================================================
// Improved heuristics (auto-trim on upload)
// =====================================================================

// suggestInterviewSpeechBounds keeps ALL speech and only trims silence that
// touches either end of the clip. Pads each end by 0.5 s so a sharp
// silencedetect cut doesn't lop off the first/last syllable. No max cap —
// the operator's complaint was over-cutting, not over-keeping.
func suggestInterviewSpeechBounds(ctx context.Context, clip *state.Clip) (Suggestion, error) {
	if !clip.HasAudio {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  "Interview: no audio detected — using full clip",
		}, nil
	}

	// d=0.3 (was 0.5) — loose enough to ignore between-word pauses while still
	// detecting real silence at the head/tail of the clip.
	windows, err := ffmpeg.DetectSilence(ctx, clip.SourcePath, -30, 0.3)
	if err != nil {
		// Don't fail upload on probe error — fall back to full clip + log.
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  fmt.Sprintf("Interview: silencedetect failed (%v); using full clip", err),
		}, nil
	}

	in := 0.0
	out := clip.DurationSeconds

	// Leading silence — first window starting at ~0 → trim_in = its end.
	for _, w := range windows {
		if w.Start <= 0.05 {
			in = w.End
		}
		break
	}
	// Trailing silence — last window ending at ~clip duration → trim_out = its start.
	for i := len(windows) - 1; i >= 0; i-- {
		w := windows[i]
		end := w.End
		if end <= w.Start {
			end = clip.DurationSeconds
		}
		if end >= clip.DurationSeconds-0.05 {
			out = w.Start
			break
		}
	}

	// 0.5 s pad both ends, clamped to clip bounds.
	in = math.Max(0, in-0.5)
	out = math.Min(clip.DurationSeconds, out+0.5)
	if out <= in {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  "Interview: silence detection inconclusive; using full clip",
		}, nil
	}

	return Suggestion{
		TrimIn:  in,
		TrimOut: out,
		Reason: fmt.Sprintf("Interview: kept %s of speech (silencedetect -30dB, 0.5s pad)",
			fmtSec(out-in)),
	}, nil
}

// suggestFreefallByWindRMS finds the LONGEST sustained-loud window in the
// audio energy envelope and uses its bounds as the freefall section. The
// loud floor is calibrated against the clip's own quiet level so the
// heuristic adapts to operators who film at different gain levels.
//
// Falls back to "keep full clip" rather than aggressive positional cutting —
// prior versions were eating the actual freefall when the threshold heuristic
// missed. Operator drags handles to refine.
func suggestFreefallByWindRMS(ctx context.Context, clip *state.Clip) (Suggestion, error) {
	if !clip.HasAudio {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  "Freefall: no audio — kept full clip; drag handles to refine",
		}, nil
	}
	frames, err := ffmpeg.AudioRMS(ctx, clip.SourcePath)
	if err != nil || len(frames) < 6 {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason: func() string {
				if err != nil {
					return fmt.Sprintf("Freefall: AudioRMS failed (%v) — kept full clip", err)
				}
				return "Freefall: clip too short for RMS — kept full clip"
			}(),
		}, nil
	}

	start, end, ok := findLongestLoudWindow(frames)
	if !ok {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  "Freefall: no sustained loud section detected — kept full clip; drag to refine",
		}, nil
	}

	// Pad each side. Wide pads to be conservative — better to keep extra than
	// to lose action.
	in := math.Max(0, start-1.5)
	out := math.Min(clip.DurationSeconds, end+2.5)
	// Refuse to shrink the kept window below 50% of the clip — usually means
	// the heuristic latched onto something narrow (engine noise spike, etc).
	if out-in < clip.DurationSeconds*0.5 {
		return Suggestion{
			TrimIn:  0,
			TrimOut: clip.DurationSeconds,
			Reason:  "Freefall: detected window too narrow to trust — kept full clip; drag to refine",
		}, nil
	}
	return Suggestion{
		TrimIn:  in,
		TrimOut: out,
		Reason: fmt.Sprintf("Freefall: kept loudest %ds window (%.1fs–%.1fs); drag to refine",
			int(end-start), start, end),
	}, nil
}

// findLongestLoudWindow scans per-second RMS frames and returns the bounds
// of the longest contiguous run of "loud" frames. "Loud" is defined as
// frames at least 12 dB above the clip's median RMS — adapts to operators
// who shoot at different input gain levels. Returns ok=false if there is
// no run of ≥4 seconds (clip is uniformly quiet).
func findLongestLoudWindow(frames []ffmpeg.RMSFrame) (start, end float64, ok bool) {
	if len(frames) < 4 {
		return 0, 0, false
	}

	// Sorted-copy for median.
	sorted := make([]float64, len(frames))
	for i, f := range frames {
		sorted[i] = f.RMSdB
	}
	for i := 1; i < len(sorted); i++ {
		v := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > v {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = v
	}
	median := sorted[len(sorted)/2]
	loudFloor := median + 12.0
	if loudFloor > -10.0 {
		loudFloor = -10.0 // saturate at very loud — no clip is louder than -10dB peak
	}

	bestStart, bestEnd := -1, -1
	bestLen := 0
	curStart := -1
	for i, f := range frames {
		if f.RMSdB > loudFloor {
			if curStart == -1 {
				curStart = i
			}
			curLen := i - curStart + 1
			if curLen > bestLen {
				bestLen = curLen
				bestStart = curStart
				bestEnd = i
			}
		} else {
			curStart = -1
		}
	}

	const minRunFrames = 4 // 4 seconds of sustained loud
	if bestLen < minRunFrames || bestStart < 0 {
		return 0, 0, false
	}
	return frames[bestStart].T, frames[bestEnd].T, true
}

// suggestLandingSmart anchors the cut on the touchdown impact (RMS spike
// after a quiet canopy descent), then ends the clip after the last spoken
// phrase + 1s pad (smart silencedetect on the tail). Also returns the
// speech-start marker timestamp so the pipeline ducks music under the
// post-landing interview without operator action.
func suggestLandingSmart(ctx context.Context, clip *state.Clip) (Suggestion, error) {
	if !clip.HasAudio {
		s := suggestTailWindow(clip, 8.0, "Landing: no audio — last 8s positional fallback")
		return s, nil
	}

	frames, err := ffmpeg.AudioRMS(ctx, clip.SourcePath)
	spike, spikeOK := findImpactSpike(frames)

	if err != nil || !spikeOK {
		// No clean impact — keep last 30s (was 8s) so the operator gets the
		// full post-landing chat by default; they can drag thumbs to refine.
		s := suggestTailWindow(clip, 30.0, "Landing: no impact detected; last 30s")
		return s, nil
	}

	in := math.Max(0, spike-1.0)
	speechStart := math.Min(clip.DurationSeconds-0.1, spike+1.0)

	// Smart trim_out: silencedetect on the FULL clip (cheap), then look at
	// only the tail silence-windows AFTER the impact. The last silence
	// touching the clip end → trim_out = its start + 1s pad. Otherwise the
	// last phrase runs to the end.
	out := clip.DurationSeconds
	lastSpeechT := clip.DurationSeconds
	silWindows, sErr := ffmpeg.DetectSilence(ctx, clip.SourcePath, -30, 0.5)
	if sErr == nil {
		for i := len(silWindows) - 1; i >= 0; i-- {
			w := silWindows[i]
			end := w.End
			if end <= w.Start {
				end = clip.DurationSeconds
			}
			if w.Start <= spike {
				break
			}
			if end >= clip.DurationSeconds-0.05 {
				lastSpeechT = w.Start
				out = math.Min(clip.DurationSeconds, w.Start+1.0)
				break
			}
		}
	}
	// Floor: at least 10s after the impact. Avoid hyper-aggressive cuts on
	// clips where the operator stops talking right after touchdown.
	if out < spike+10 {
		out = math.Min(clip.DurationSeconds, spike+10)
	}

	return Suggestion{
		TrimIn:      in,
		TrimOut:     out,
		SpeechStart: speechStart,
		Reason: fmt.Sprintf("Landing: impact @%.1fs, last speech ends @%.1fs, marker @%.1fs",
			spike, lastSpeechT, speechStart),
	}, nil
}

// findImpactSpike scans per-second RMS frames for a single high-energy
// frame (>-20 dB) that is preceded by ≥1 s of relative quiet (<-30 dB).
// That's the signature of the canopy-then-touchdown transition.
func findImpactSpike(frames []ffmpeg.RMSFrame) (float64, bool) {
	const (
		spikeDB = -20.0
		quietDB = -30.0
	)
	for i := 1; i < len(frames); i++ {
		if frames[i].RMSdB <= spikeDB {
			continue
		}
		// Look back ≥1 s for at least one quiet frame.
		quietBefore := false
		for j := i - 1; j >= 0; j-- {
			if frames[i].T-frames[j].T > 3.0 {
				break
			}
			if frames[j].RMSdB < quietDB {
				quietBefore = true
				break
			}
		}
		if quietBefore {
			return frames[i].T, true
		}
	}
	return 0, false
}

// SpeechStartSuggestion is the analogue of Suggestion for the speech-start
// marker — tells the operator where the post-action interview kicks in
// inside an action clip (typical case: landing footage that ends with the
// jumper turning to camera and talking).
type SpeechStartSuggestion struct {
	SpeechStart float64 // source-clip seconds (inside [trim_in, trim_out])
	Reason      string  // operator-facing label
}

// SuggestSpeechStart finds the first non-trivial speech onset INSIDE the
// trim window. We run silencedetect over the trim window and pick the
// first end-of-silence after at least 0.5s of trim window has elapsed
// (so the clip's leading silence — the action portion — is excluded).
// Falls back to mid-window when audio is missing or silence detection
// is inconclusive.
func SuggestSpeechStart(ctx context.Context, clip *state.Clip) (SpeechStartSuggestion, error) {
	if clip == nil {
		return SpeechStartSuggestion{}, fmt.Errorf("clip is nil")
	}
	if clip.DurationSeconds <= 0 {
		return SpeechStartSuggestion{}, fmt.Errorf("clip has no duration metadata — re-upload to refresh")
	}

	tIn := clip.TrimInSeconds
	tOut := clip.EffectiveTrimOut()
	if tOut <= tIn+1.0 {
		return SpeechStartSuggestion{}, fmt.Errorf("trim window is too short to host an interview tail")
	}

	if !clip.HasAudio {
		// No audio — we can only guess. Place the marker 60% of the way
		// through the trim window so the operator can drag from there.
		mid := tIn + (tOut-tIn)*0.6
		return SpeechStartSuggestion{
			SpeechStart: mid,
			Reason:      "Clip has no audio — placed at 60% of trim window; drag to refine",
		}, nil
	}

	windows, err := ffmpeg.DetectSilence(ctx, clip.SourcePath, -30, 0.5)
	if err != nil {
		return SpeechStartSuggestion{}, fmt.Errorf("silencedetect: %w", err)
	}

	// Walk silence windows looking for the first silence that ENDS inside
	// the trim window — that end timestamp is where speech begins.
	for _, w := range windows {
		end := w.End
		if end <= w.Start {
			end = clip.DurationSeconds
		}
		if end > tIn+0.5 && end < tOut-0.5 {
			return SpeechStartSuggestion{
				SpeechStart: end,
				Reason:      fmt.Sprintf("First speech onset at %s (silencedetect)", fmtSec(end-tIn)),
			}, nil
		}
	}

	// Nothing detected — drop a marker at 60% so the operator has a starting point.
	mid := tIn + (tOut-tIn)*0.6
	return SpeechStartSuggestion{
		SpeechStart: mid,
		Reason:      "No silence boundary found; placed at 60% of trim window — drag to refine",
	}, nil
}

// =====================================================================
// helpers
// =====================================================================
func isCustomKind(k string) bool {
	return len(k) > len(state.CustomPrefix) && k[:len(state.CustomPrefix)] == state.CustomPrefix
}

func fmtSec(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
}
