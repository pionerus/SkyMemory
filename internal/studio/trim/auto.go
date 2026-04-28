// Package trim owns auto-suggestion of in/out trim windows per segment kind.
//
// Heuristics (matches the project plan §"Step 2 auto-trim"):
//
//   intro / closing / custom_*  — ffmpeg silencedetect, drop leading/trailing
//                                  silence > 0.5s, cap at 15s.
//   interview_pre / interview_plane — same, cap at 30s.
//   landing                     — last 8s of clip (real audio-RMS spike detection
//                                  is a follow-up; current rule is positional).
//   walk                        — middle 8s of clip (motion-magnitude analysis
//                                  is a v2 follow-up that needs OpenCV/GoCV).
//   freefall                    — skip first 2s, take next 30s (motion analysis
//                                  is v2; this is "good enough" because freefall
//                                  is uniformly action-packed).
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
// to trust it.
type Suggestion struct {
	TrimIn  float64
	TrimOut float64
	Reason  string
}

// Suggest dispatches to the per-kind heuristic. Pure-positional kinds (walk,
// freefall, landing) ignore `path`. Audio-based kinds shell out to ffmpeg.
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
		return suggestSilenceBound(ctx, clip, 30.0)

	case state.KindWalk:
		return suggestMiddleWindow(clip, 8.0, "Middle 8s of clip"), nil

	case state.KindFreefall:
		return suggestSkipThenWindow(clip, 2.0, 30.0), nil

	case state.KindLanding:
		return suggestTailWindow(clip, 8.0, "Last 8s of clip (real audio-RMS detection coming in v2)"), nil
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
