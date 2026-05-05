package highlights

import (
	"context"
	"fmt"
	"math"

	"github.com/pionerus/freefall/internal/studio/state"
)

// PickInstaReelSegments builds a ~50-second story-style vertical reel:
//
//	walk (3–5s) → plane (3–5s) → freefall multi-cut (~30s) → landing (5s)
//
// Falls back gracefully when individual clips are missing — operator can
// still get a freefall-only Insta reel if they didn't film walk/plane.
//
// Returns segments oriented for centre-cropped 9:16 rendering. Caller
// passes the SAME slice into RunReel with Aspect=AspectVertical.
func PickInstaReelSegments(ctx context.Context, clips map[string]*state.Clip) ([]Segment, bool, string) {
	out := []Segment{}

	if walk := clips[state.KindWalk]; walk != nil && walk.DurationSeconds >= 3 {
		// Middle 4 seconds.
		mid := walk.EffectiveTrimOut() / 2
		out = append(out, Segment{
			SourcePath: walk.SourcePath,
			Kind:       state.KindWalk,
			Start:      math.Max(0, mid-2),
			End:        math.Min(walk.DurationSeconds, mid+2),
			Label:      "walk",
		})
	}

	if plane := clips[state.KindInterviewPlane]; plane != nil && plane.DurationSeconds >= 3 {
		// Last 4 seconds of the trim window — usually closer to exit prep.
		end := plane.EffectiveTrimOut()
		out = append(out, Segment{
			SourcePath: plane.SourcePath,
			Kind:       state.KindInterviewPlane,
			Start:      math.Max(0, end-4),
			End:        end,
			Label:      "plane",
		})
	}

	// Freefall multi-cut. Reuse the WOW picker — same exit/canopy anchors.
	if freefall := clips[state.KindFreefall]; freefall != nil {
		wow, ok, _ := PickWOWReelSegments(ctx, freefall)
		if ok {
			// Trim WOW to ~30s total instead of 40s — Insta reel needs room
			// for the bookends.
			out = append(out, capTotalLength(wow, 30.0)...)
		}
	}

	if landing := clips[state.KindLanding]; landing != nil && landing.DurationSeconds >= 3 {
		// First 5s of the trim window — captures touchdown + reaction onset.
		start := landing.TrimInSeconds
		out = append(out, Segment{
			SourcePath: landing.SourcePath,
			Kind:       state.KindLanding,
			Start:      start,
			End:        math.Min(landing.DurationSeconds, start+5),
			Label:      "landing",
		})
	}

	if len(out) < 2 {
		return nil, false, "not enough source clips for an Insta reel"
	}
	totalDur := 0.0
	for _, s := range out {
		totalDur += s.Duration()
	}
	return out, true, fmt.Sprintf("Picked %d segments, ~%s total", len(out), fmtSec(totalDur))
}

// capTotalLength clamps a segment list so the cumulative duration ≤ maxSec.
// Trims from the END of the list (preserves the first cuts which are
// usually more valuable — exit moment etc).
func capTotalLength(segs []Segment, maxSec float64) []Segment {
	out := []Segment{}
	cum := 0.0
	for _, s := range segs {
		dur := s.Duration()
		if cum+dur <= maxSec {
			out = append(out, s)
			cum += dur
			continue
		}
		// Try shrinking the last accepted segment.
		if remaining := maxSec - cum; remaining > 1.0 {
			s.End = s.Start + remaining
			out = append(out, s)
		}
		break
	}
	return out
}
