package highlights

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/pionerus/freefall/internal/studio/ffmpeg"
	"github.com/pionerus/freefall/internal/studio/state"
)

// PhotoPick is one chosen photo destination — describes which timestamp
// the picker wants and a label for the operator. After ExtractPhotoPack
// runs, ResultPath is populated.
type PhotoPick struct {
	SourcePath string
	SourceKind string  // KindFreefall, etc — for logging / S3 prefixing
	T          float64 // chosen timestamp inside the source clip
	Label      string  // e.g. "exit", "freefall body 3", "canopy open"
	ResultPath string  // set by ExtractPhotoPack on success
	Sharpness  float64 // Laplacian-variance score of the chosen frame
}

// PlanPhotoPack picks up to 20 timestamps across the FREEFALL clip only,
// starting 5 seconds after exit (skipping the disorienting tumble) and
// ending right before canopy opens. No walk / plane / landing / interview
// stills — operator's product is the freefall body shots, that's what the
// jumper buys.
//
// `operatorMarks` is the optional list of operator-set timestamps from the
// photo_marks table. When non-empty, those go in first; the planner
// auto-fills the remaining slots (up to 20 total) with even-spacing picks
// that DON'T sit too close to an operator mark — so the auto fill never
// duplicates a frame the operator already captured.
//
// Pass nil for operatorMarks to get the previous all-auto behaviour.
func PlanPhotoPack(ctx context.Context, clipByKind map[string]*state.Clip, operatorMarks []float64) ([]PhotoPick, string) {
	const (
		want         = 20
		exitSkip     = 5.0
		minWindowSec = 4.0
		canopyGuard  = 0.5
		// Auto-fill picks closer than this to an operator mark are dropped
		// in favour of moving to the next gap — avoids "operator marked
		// 12.0s, auto-pack added a duplicate at 12.3s".
		autoFillEpsilon = 1.0
	)

	ff := clipByKind[state.KindFreefall]
	if ff == nil {
		return nil, "no freefall clip — photo pack needs freefall footage"
	}
	if ff.DurationSeconds < exitSkip+minWindowSec {
		return nil, fmt.Sprintf("freefall clip too short (%.1fs) for a photo pack", ff.DurationSeconds)
	}

	a, _ := AnalyzeFreefallClip(ctx, ff.SourcePath, ff.DurationSeconds)
	exit := a.Exit.T
	canopy := a.Canopy.T
	if exit <= 0 {
		exit = ff.TrimInSeconds
	}
	if canopy <= 0 {
		canopy = ff.EffectiveTrimOut()
	}
	if canopy <= exit+1 {
		canopy = ff.DurationSeconds
	}

	bodyLo := exit + exitSkip
	bodyHi := canopy - canopyGuard
	if bodyHi-bodyLo < minWindowSec {
		maxSkip := math.Max(0, (canopy-exit-canopyGuard)/2)
		bodyLo = exit + math.Min(exitSkip, maxSkip)
	}
	if bodyHi-bodyLo < minWindowSec {
		return nil, fmt.Sprintf("freefall window too short after exit+5s skip (exit=%.1f canopy=%.1f)", exit, canopy)
	}

	picks := make([]PhotoPick, 0, want)

	// === Operator marks first, in time order ===
	for i, t := range operatorMarks {
		if len(picks) >= want {
			break
		}
		picks = append(picks, PhotoPick{
			SourcePath: ff.SourcePath,
			SourceKind: state.KindFreefall,
			T:          t,
			Label:      fmt.Sprintf("operator mark %02d", i+1),
		})
	}

	// === Auto-fill the slack ===
	// Even spacing across [bodyLo, bodyHi], skipping slots within
	// autoFillEpsilon of any operator mark.
	if len(picks) < want {
		need := want - len(picks)
		// Generate candidate slots evenly. We over-generate (need + len(operatorMarks))
		// so dropouts near operator marks don't leave us short.
		generate := need + len(operatorMarks)
		gap := (bodyHi - bodyLo) / float64(generate+1)
		added := 0
		for i := 1; i <= generate && added < need; i++ {
			t := bodyLo + gap*float64(i)
			if nearAnyTime(operatorMarks, t, autoFillEpsilon) {
				continue
			}
			picks = append(picks, PhotoPick{
				SourcePath: ff.SourcePath,
				SourceKind: state.KindFreefall,
				T:          t,
				Label:      fmt.Sprintf("auto %02d", added+1),
			})
			added++
		}
	}

	// Clamp every T to safe bounds inside the clip.
	for i := range picks {
		if picks[i].T < 0.5 {
			picks[i].T = 0.5
		}
		if picks[i].T > ff.DurationSeconds-0.2 {
			picks[i].T = ff.DurationSeconds - 0.2
		}
	}

	autoCount := len(picks) - len(operatorMarks)
	if autoCount < 0 {
		autoCount = 0
	}
	reason := fmt.Sprintf("Planned %d freefall stills (%d operator marks + %d auto, window %.1fs..%.1fs)",
		len(picks), len(operatorMarks), autoCount, bodyLo, bodyHi)
	return picks, reason
}

// nearAnyTime returns true if `t` is within `eps` of any value in xs.
func nearAnyTime(xs []float64, t, eps float64) bool {
	for _, x := range xs {
		if math.Abs(x-t) < eps {
			return true
		}
	}
	return false
}

// ExtractPhotoPack runs the picks through ffmpeg + sharpness scoring.
// For every plan entry we extract 3 candidate frames at T-0.4, T, T+0.4
// (180ms frames apart at 30fps) and keep the sharpest. Files land under
// `outDir`. The picks slice is populated with ResultPath + Sharpness
// in-place. Returns the count of successfully-extracted picks.
func ExtractPhotoPack(ctx context.Context, picks []PhotoPick, outDir string) (int, error) {
	return ExtractPhotoPackWithProgress(ctx, picks, outDir, ffmpeg.WatermarkOptions{}, nil)
}

// ExtractPhotoPackWithProgress is the variant that calls onProgress(done)
// after each pick — used by the studio's render goroutine to surface a
// real progress bar on the generate page. wm carries the club's branding
// overlay; pass a zero-value WatermarkOptions to extract clean frames.
func ExtractPhotoPackWithProgress(ctx context.Context, picks []PhotoPick, outDir string, wm ffmpeg.WatermarkOptions, onProgress func(done int)) (int, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir: %w", err)
	}
	const candOffsets = 3
	offsets := []float64{-0.4, 0, 0.4}
	got := 0
	for i := range picks {
		bestPath := ""
		bestScore := -1.0
		for k := 0; k < candOffsets; k++ {
			candPath := filepath.Join(outDir, fmt.Sprintf("photo_%02d_c%d.jpg", i, k))
			t := picks[i].T + offsets[k]
			if t < 0 {
				continue
			}
			if err := ffmpeg.ExtractFrameWithWatermark(ctx, picks[i].SourcePath, t, candPath, wm); err != nil {
				continue
			}
			score := SharpnessScore(candPath)
			if score > bestScore {
				if bestPath != "" {
					_ = os.Remove(bestPath)
				}
				bestPath = candPath
				bestScore = score
			} else {
				_ = os.Remove(candPath)
			}
		}
		if bestPath == "" {
			continue
		}
		// Rename to canonical name (drops the cN suffix).
		final := filepath.Join(outDir, fmt.Sprintf("photo_%02d.jpg", i))
		if err := os.Rename(bestPath, final); err != nil {
			picks[i].ResultPath = bestPath // keep whatever we had
		} else {
			picks[i].ResultPath = final
		}
		picks[i].Sharpness = bestScore
		got++
		if onProgress != nil {
			onProgress(i + 1)
		}
	}
	return got, nil
}

