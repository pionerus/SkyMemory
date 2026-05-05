package highlights

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/pionerus/freefall/internal/studio/ffmpeg"
	"github.com/pionerus/freefall/internal/studio/state"
)

// Segment is one source-clip sub-range to include in a multi-cut reel.
// Order in the slice = order in the rendered output.
type Segment struct {
	SourcePath string
	Kind       string  // KindFreefall, KindLanding, etc — for logging
	Start      float64 // source-clip seconds
	End        float64
	Label      string // operator-facing description ("exit", "wide angle", "canopy open")
}

// Duration returns seg length.
func (s Segment) Duration() float64 { return s.End - s.Start }

// PickWOWReelSegments builds a 30–40 second freefall-only reel from the
// freefall clip. Strategy:
//
//   • 1st cut: 3 seconds starting at the exit moment (the most important
//     beat — operator's body leaving the door).
//   • 2nd–4th cuts: 4–6 seconds each, distributed across the loud-RMS
//     freefall window between exit and canopy-open. We anchor on scene
//     changes when available so each cut feels like a natural angle change.
//   • Last cut: 5–7 seconds wrapping the canopy-open moment so the reel
//     ends on a payoff (open → quiet sky).
//
// Returns ok=false if the freefall clip has no detectable exit/canopy
// signals — caller should skip the WOW deliverable rather than render
// nonsense.
func PickWOWReelSegments(ctx context.Context, freefall *state.Clip) ([]Segment, bool, string) {
	if freefall == nil || freefall.DurationSeconds < 8 {
		return nil, false, "freefall clip too short for a reel"
	}

	a, _ := AnalyzeFreefallClip(ctx, freefall.SourcePath, freefall.DurationSeconds)

	// Anchor the cut window. Order of preference:
	//   1. Detected exit + canopy (RMS-based, highest confidence)
	//   2. Detected exit only (no canopy → use trim_out / dur)
	//   3. Operator's trim window (use trim_in/trim_out as freefall bounds)
	//   4. Whole clip
	exit := a.Exit.T
	canopy := a.Canopy.T
	usedFallback := false

	if exit <= 0 {
		// No detection — use the operator-set trim window. The trim represents
		// "what the operator wants kept" and is the best signal we have when
		// audio analysis fails.
		exit = freefall.TrimInSeconds
		canopy = freefall.EffectiveTrimOut()
		if canopy <= exit+5 || (exit == 0 && canopy == 0) {
			// No useful trim either → use the whole clip.
			exit = 0
			canopy = freefall.DurationSeconds
		}
		usedFallback = true
	} else if canopy <= 0 {
		canopy = freefall.EffectiveTrimOut()
		if canopy <= exit+5 {
			canopy = math.Min(freefall.DurationSeconds, exit+30)
		}
	}
	if canopy <= exit+3 {
		return nil, false, "freefall window too short to slice"
	}

	out := []Segment{}

	// Cut 1: exit, 3 seconds.
	out = append(out, Segment{
		SourcePath: freefall.SourcePath,
		Kind:       state.KindFreefall,
		Start:      exit,
		End:        math.Min(exit+3.0, canopy-2),
		Label:      "exit",
	})

	// Middle cuts. Take all scene-change times that fall inside (exit+3, canopy-3),
	// dedupe to ≥4 s apart, pick top 3 by score.
	mid := pickMidCuts(a.Scenes, exit+3.0, canopy-3.0, 3)
	for i, t := range mid {
		segLen := 4.0
		if i == len(mid)-1 {
			segLen = 5.0
		}
		out = append(out, Segment{
			SourcePath: freefall.SourcePath,
			Kind:       state.KindFreefall,
			Start:      t,
			End:        math.Min(t+segLen, canopy-1),
			Label:      fmt.Sprintf("angle %d", i+1),
		})
	}
	// Pad with evenly-spaced fallbacks if scene-change gave us <3 cuts.
	if len(out) < 4 {
		gap := (canopy - exit) / 4.0
		for i := 1; i < 4 && len(out) < 4; i++ {
			t := exit + gap*float64(i)
			if hasOverlap(out, t, t+4) {
				continue
			}
			out = append(out, Segment{
				SourcePath: freefall.SourcePath,
				Kind:       state.KindFreefall,
				Start:      t,
				End:        math.Min(t+4.0, canopy-1),
				Label:      fmt.Sprintf("body %d (positional)", i),
			})
		}
	}

	// Last cut: canopy open + 5 seconds.
	out = append(out, Segment{
		SourcePath: freefall.SourcePath,
		Kind:       state.KindFreefall,
		Start:      math.Max(exit+3, canopy-1.0),
		End:        math.Min(freefall.DurationSeconds, canopy+5.0),
		Label:      "canopy open",
	})

	// Drop zero-or-negative-duration segments + sort.
	clean := out[:0]
	for _, s := range out {
		if s.End-s.Start >= 0.5 {
			clean = append(clean, s)
		}
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Start < clean[j].Start })

	if len(clean) < 2 {
		return nil, false, "could not pick at least two distinct segments"
	}

	if usedFallback {
		return clean, true, fmt.Sprintf("Picked %d segments from trim window %s..%s (no audio anchors — drag to refine)",
			len(clean), fmtSec(exit), fmtSec(canopy))
	}
	return clean, true, fmt.Sprintf("Picked %d segments (exit @%s, canopy @%s)",
		len(clean), fmtSec(exit), fmtSec(canopy))
}

// pickMidCuts walks the scene-change list inside [lo, hi], dedupes to ≥4s
// apart, returns up to wantN highest-scored timestamps in chronological
// order. Falls back to evenly-spaced positions when scenes is empty.
func pickMidCuts(scenes []ffmpeg.SceneChange, lo, hi float64, wantN int) []float64 {
	type cand struct {
		t, score float64
	}
	cands := []cand{}
	for _, sc := range scenes {
		if sc.T > lo && sc.T < hi {
			cands = append(cands, cand{t: sc.T, score: sc.Score})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	// Sort by score desc, take wantN, then sort by time asc.
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	out := []float64{}
	for _, c := range cands {
		tooClose := false
		for _, t := range out {
			if math.Abs(t-c.t) < 4.0 {
				tooClose = true
				break
			}
		}
		if !tooClose {
			out = append(out, c.t)
		}
		if len(out) >= wantN {
			break
		}
	}
	sort.Float64s(out)
	return out
}

// hasOverlap reports whether any segment in slice overlaps [s, e].
func hasOverlap(segs []Segment, s, e float64) bool {
	for _, x := range segs {
		if !(e <= x.Start || s >= x.End) {
			return true
		}
	}
	return false
}

