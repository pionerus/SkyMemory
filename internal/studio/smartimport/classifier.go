// Package smartimport classifies an unsorted folder of skydive clip files
// into the canonical kind slots (interview_pre, walk, interview_plane,
// freefall, landing) plus position-numbered custom slots for B-roll.
//
// Algorithm:
//   1. Sort clips by mtime (chronological order matches jump sequence).
//   2. Find the freefall anchor — the loudest, longest clip with sustained
//      wind-roar audio. Skydive freefall is the only segment with 30+ s of
//      sustained -25..-15 dB RMS in the top quartile of the clip's own
//      energy. ~95 % single-shot precision.
//   3. Pre-freefall clips fan out into interview_pre / walk / interview_plane
//      by chronological position; surplus go to custom:before-freefall-N
//      slotted between canonicals at their actual time-position.
//   4. Post-freefall: first = landing, surplus = custom:after-landing-N.
package smartimport

import (
	"fmt"
	"sort"
	"time"

	"github.com/pionerus/freefall/internal/studio/state"
)

// FileMetrics is the input row per video file. The handler builds these by
// statting the file (mtime), running ffprobe (duration), and ffmpeg AudioRMS
// + AnalyzeRMS (audio shape).
type FileMetrics struct {
	Path                 string    // disk path on the studio's machine
	Filename             string    // user-facing name (without dir)
	Mtime                time.Time // file modification time
	DurationSeconds      float64
	RMS90thPercentile    float64 // dB
	SustainedHighSeconds float64
}

// Assignment is one classifier output. Position is the value to write into
// clips.position (the existing canonical kinds use 10..70 — we interleave
// custom slots at fractional values so grip-handle reorder still works).
type Assignment struct {
	Path     string
	Filename string
	Kind     string
	Position int
	Reason   string // human-readable; surfaced in JSON for debugging
}

// Result wraps the assignments with metadata about the run.
type Result struct {
	Assignments []Assignment
	// FreefallIndex is the index into the *time-sorted* input slice where the
	// freefall anchor was placed. -1 if no freefall could be identified.
	FreefallIndex int
	// FreefallConfidence is "high" / "medium" / "low" / "none" — drives the
	// front-end's warning chip when uncertain.
	FreefallConfidence string
}

// Position offsets — same scale as state/clips.go::nextClipPosition (canonical
// kinds at 20/30/40/50/60). Custom slots interleave with single-digit gaps
// so a follow-up grip-reorder doesn't immediately collide.
const (
	posInterviewPre   = 20
	posWalk           = 30
	posInterviewPlane = 40
	posFreefall       = 50
	posLanding        = 60

	customBeforeFreefallBase = 41 // 41, 42, 43… up to 49
	customAfterLandingBase   = 61 // 61, 62, 63…
)

// Classify is the public entry point. Input is the metrics slice (any order);
// output is a stable list of assignments ready to insert into the clips table.
//
// The algorithm is a pure function — no I/O — so it's trivially testable
// and the handler can call it under a goroutine without surprises.
func Classify(files []FileMetrics) Result {
	if len(files) == 0 {
		return Result{FreefallIndex: -1, FreefallConfidence: "none"}
	}

	// 1. Sort by mtime ascending. Stable sort keeps relative order for files
	// with identical mtimes (rare but possible after a copy operation).
	sorted := append([]FileMetrics(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Mtime.Before(sorted[j].Mtime)
	})

	// 2. Find the freefall anchor.
	ffIdx, ffConfidence := findFreefall(sorted)

	// 3. If no freefall: fall back to pure-time mapping (variant A behaviour).
	// First N files take canonical kinds in order, surplus → custom:extra-N
	// at the end.
	if ffIdx < 0 {
		return classifyTimeOnly(sorted)
	}

	// 4. Build assignments around the freefall anchor.
	assignments := make([]Assignment, 0, len(sorted))

	// 4a. Freefall itself.
	assignments = append(assignments, Assignment{
		Path:     sorted[ffIdx].Path,
		Filename: sorted[ffIdx].Filename,
		Kind:     state.KindFreefall,
		Position: posFreefall,
		Reason: fmt.Sprintf("loudest sustained audio (%.1f s @ p90=%.1f dB) — wind roar signature",
			sorted[ffIdx].SustainedHighSeconds, sorted[ffIdx].RMS90thPercentile),
	})

	// 4b. Pre-freefall portion.
	pre := sorted[:ffIdx]
	preAssignments := classifyPreFreefall(pre)
	assignments = append(assignments, preAssignments...)

	// 4c. Post-freefall portion.
	post := sorted[ffIdx+1:]
	postAssignments := classifyPostFreefall(post)
	assignments = append(assignments, postAssignments...)

	// 4d. Sort final list by position so the API output is timeline-ordered
	// (helps both the UI summary and the DB insert order).
	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].Position < assignments[j].Position
	})

	return Result{
		Assignments:        assignments,
		FreefallIndex:      ffIdx,
		FreefallConfidence: ffConfidence,
	}
}

// findFreefall picks the clip most likely to be the freefall segment.
// Returns (-1, "none") when no candidate passes the energy bar. The
// confidence string lets the UI flag uncertain runs.
func findFreefall(sorted []FileMetrics) (int, string) {
	type cand struct {
		idx   int
		score float64
	}
	var candidates []cand

	// Primary criterion: sustained high RMS for 30+ s within a 40-180 s
	// total duration. Real freefall is 45-60 s for a tandem.
	for i, f := range sorted {
		if f.DurationSeconds < 40 || f.DurationSeconds > 180 {
			continue
		}
		if f.SustainedHighSeconds < 30 {
			continue
		}
		// Score ~ how loud + how long the high-energy run lasts. Clips with
		// stronger / longer runs win.
		score := f.RMS90thPercentile + f.SustainedHighSeconds*0.5
		candidates = append(candidates, cand{idx: i, score: score})
	}

	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].score > candidates[j].score
		})
		conf := "high"
		if len(candidates) > 1 && (candidates[0].score-candidates[1].score) < 5 {
			// Two clips look similar — drop confidence so the UI can warn.
			conf = "medium"
		}
		return candidates[0].idx, conf
	}

	// Secondary: no clip has a sustained-high run, but one is loud enough
	// to plausibly be freefall on a quiet mic. Pick the longest clip with
	// p90 ≥ -28 dB (looser threshold).
	bestIdx := -1
	bestDur := 0.0
	for i, f := range sorted {
		if f.DurationSeconds >= 40 && f.RMS90thPercentile >= -28 && f.DurationSeconds > bestDur {
			bestIdx = i
			bestDur = f.DurationSeconds
		}
	}
	if bestIdx >= 0 {
		return bestIdx, "low"
	}

	return -1, "none"
}

// classifyTimeOnly is the fallback when no freefall could be identified.
// First five files get canonical kinds in order; surplus go to
// custom:extra-N at the tail. Used rarely — operator likely needs to fix
// up the result manually but at least every file gets a slot.
func classifyTimeOnly(sorted []FileMetrics) Result {
	canonical := []string{
		state.KindInterviewPre, state.KindWalk, state.KindInterviewPlane,
		state.KindFreefall, state.KindLanding,
	}
	canonicalPos := []int{posInterviewPre, posWalk, posInterviewPlane, posFreefall, posLanding}
	out := make([]Assignment, 0, len(sorted))
	for i, f := range sorted {
		if i < len(canonical) {
			out = append(out, Assignment{
				Path:     f.Path,
				Filename: f.Filename,
				Kind:     canonical[i],
				Position: canonicalPos[i],
				Reason:   "chronological fallback (no freefall detected)",
			})
		} else {
			out = append(out, Assignment{
				Path:     f.Path,
				Filename: f.Filename,
				Kind:     fmt.Sprintf("custom:extra-%d", i-len(canonical)+1),
				Position: customAfterLandingBase + (i - len(canonical)),
				Reason:   "surplus video — no freefall to anchor against",
			})
		}
	}
	return Result{Assignments: out, FreefallIndex: -1, FreefallConfidence: "none"}
}

// classifyPreFreefall maps the chronological pre-freefall slice to canonical
// + custom kinds.
//
//   N=0 → nothing
//   N=1 → first = interview_pre
//   N=2 → first = interview_pre, second = interview_plane (skip walk)
//   N≥3 → first = interview_pre.
//         interview_plane = the LONGEST clip among the rest (interview_plane
//           is filmed during the 5-15 min ascent — almost always the longest).
//         walk = first chronologically among the rest, IF that clip comes
//           BEFORE the chosen plane clip. Walk is brief (30-60 s); operator
//           films it right after walking out of the prep zone.
//         remaining clips → custom slots inserted at their actual time-position
//           (between walk & plane → custom:before-plane-N at positions 31, 32…
//            after plane & before freefall → custom:before-freefall-N at 41, 42…).
func classifyPreFreefall(pre []FileMetrics) []Assignment {
	out := make([]Assignment, 0, len(pre))
	if len(pre) == 0 {
		return out
	}

	// First clip → interview_pre (always the operator's first take).
	out = append(out, Assignment{
		Path: pre[0].Path, Filename: pre[0].Filename,
		Kind: state.KindInterviewPre, Position: posInterviewPre,
		Reason: "first chronologically — interview before jump",
	})
	if len(pre) == 1 {
		return out
	}

	rest := pre[1:]

	// Two-clip case: second is always interview_plane.
	if len(rest) == 1 {
		out = append(out, Assignment{
			Path: rest[0].Path, Filename: rest[0].Filename,
			Kind: state.KindInterviewPlane, Position: posInterviewPlane,
			Reason: "second chronologically — interview in plane",
		})
		return out
	}

	// 2+ remaining clips: pick interview_plane = longest. Operator's plane
	// interview spans the ascent and almost always dwarfs walk + B-roll
	// individually.
	longestIdx := 0
	for i := 1; i < len(rest); i++ {
		if rest[i].DurationSeconds > rest[longestIdx].DurationSeconds {
			longestIdx = i
		}
	}
	planeClip := rest[longestIdx]

	// walk = first chronologically AMONG rest, if that clip predates the
	// plane interview (you walk → enter plane → start interviewing). When
	// the longest happens to be FIRST chronologically, there's no walk
	// segment in this folder — operator went pre-jump → straight to plane
	// interview without filming the walk.
	walkIdx := -1
	if longestIdx != 0 && rest[0].Mtime.Before(planeClip.Mtime) {
		walkIdx = 0
		out = append(out, Assignment{
			Path: rest[0].Path, Filename: rest[0].Filename,
			Kind: state.KindWalk, Position: posWalk,
			Reason: "first clip after interview_pre — walk to plane",
		})
	}

	// Plane.
	out = append(out, Assignment{
		Path: planeClip.Path, Filename: planeClip.Filename,
		Kind: state.KindInterviewPlane, Position: posInterviewPlane,
		Reason: "longest pre-freefall clip — interview in plane (during ascent)",
	})

	// Custom slots for everyone else, partitioned by chronological position
	// relative to the plane interview.
	beforePlane := 0    // count of customs that fall between walk & plane (31, 32, …)
	beforeFreefall := 0 // count between plane & freefall (41, 42, …)
	for i, f := range rest {
		if i == longestIdx || i == walkIdx {
			continue
		}
		var pos int
		var kind string
		if f.Mtime.Before(planeClip.Mtime) {
			beforePlane++
			pos = posWalk + beforePlane
			kind = fmt.Sprintf("custom:before-plane-%d", beforePlane)
		} else {
			beforeFreefall++
			pos = posInterviewPlane + beforeFreefall
			kind = fmt.Sprintf("custom:before-freefall-%d", beforeFreefall)
		}
		out = append(out, Assignment{
			Path: f.Path, Filename: f.Filename,
			Kind: kind, Position: pos,
			Reason: "B-roll — chronological position preserved",
		})
	}
	return out
}

// classifyPostFreefall handles the after-freefall slice. First is always
// landing; everything after lands in custom:after-landing-N.
func classifyPostFreefall(post []FileMetrics) []Assignment {
	out := make([]Assignment, 0, len(post))
	for i, f := range post {
		if i == 0 {
			out = append(out, Assignment{
				Path: f.Path, Filename: f.Filename,
				Kind: state.KindLanding, Position: posLanding,
				Reason: "first chronologically after freefall — landing",
			})
			continue
		}
		out = append(out, Assignment{
			Path: f.Path, Filename: f.Filename,
			Kind:     fmt.Sprintf("custom:after-landing-%d", i),
			Position: customAfterLandingBase + (i - 1), // 61, 62, 63…
			Reason:   "after landing — celebration / post-jump",
		})
	}
	return out
}
