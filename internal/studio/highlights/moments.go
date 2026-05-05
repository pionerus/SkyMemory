// Package highlights detects key moments inside skydive clips — the exit
// from the aircraft, the canopy-open transition, the touchdown impact —
// and uses those anchors to pick segments for short-form deliverables
// (WOW reel, Instagram reel) and timestamps for the photo pack.
//
// Built on top of internal/studio/ffmpeg probes. No external CV/ML deps —
// signals come from per-second audio RMS + ffmpeg scdet scene changes.
package highlights

import (
	"context"
	"math"

	"github.com/pionerus/freefall/internal/studio/ffmpeg"
)

// Moment is one detected anchor inside a clip with confidence + a one-line
// reason. UI surfaces the reason so operators can sanity-check the heuristic
// before committing the deliverable.
type Moment struct {
	T          float64
	Confidence float64 // 0..1; >0.6 trustworthy, <0.4 fall back to defaults
	Reason     string
}

// FindExitMoment locates the airplane exit inside a freefall clip. Strongest
// signal is the audio-RMS jump from cabin drone (~-30 dB) to wind roar
// (~-15 dB) over <1 s. A scene change at the same timestamp boosts
// confidence — cabin→sky is the biggest visual delta in the clip.
//
// Inputs may be empty: rms=nil disables audio fusion, scenes=nil disables
// scene fusion. Returns ok=false when neither signal is conclusive.
func FindExitMoment(rms []ffmpeg.RMSFrame, scenes []ffmpeg.SceneChange, clipDur float64) (Moment, bool) {
	if len(rms) < 6 {
		return Moment{}, false
	}

	// Pre-exit cabin floor: median of frames in the first 30% of the clip.
	// We assume the operator started filming inside the plane.
	preLen := int(math.Min(float64(len(rms)), float64(len(rms))*0.3))
	if preLen < 3 {
		preLen = 3
	}
	cabinMedian := medianRMS(rms[:preLen])

	// Walk forward looking for the largest 3-frame ramp where window mean
	// jumps ≥10 dB above cabin median.
	const rampWindow = 3
	const minJumpDB = 10.0
	bestIdx := -1
	bestRamp := 0.0
	for i := preLen; i+rampWindow < len(rms); i++ {
		windowMean := 0.0
		for k := 0; k < rampWindow; k++ {
			windowMean += rms[i+k].RMSdB
		}
		windowMean /= rampWindow
		ramp := windowMean - cabinMedian
		if ramp > minJumpDB && ramp > bestRamp {
			bestRamp = ramp
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return Moment{}, false
	}

	exitT := rms[bestIdx].T
	conf := math.Min(1.0, bestRamp/20.0) // 10 dB → 0.5, 20 dB → 1.0

	// Boost confidence if a scene change lands within ±2 s.
	for _, sc := range scenes {
		if math.Abs(sc.T-exitT) <= 2.0 {
			conf = math.Min(1.0, conf+0.25)
			return Moment{
				T:          exitT,
				Confidence: conf,
				Reason:     "Exit detected: RMS jump + scene change at ≈" + fmtSec(exitT),
			}, true
		}
	}

	return Moment{
		T:          exitT,
		Confidence: conf,
		Reason:     "Exit detected: RMS jump from cabin floor at ≈" + fmtSec(exitT),
	}, true
}

// FindCanopyOpenMoment is the inverse of exit detection: a sustained drop
// in RMS from wind roar to relative quiet, ≥5 s into freefall. Optional
// scene boost when canopy deploys with a visible flag/colour change in
// view.
func FindCanopyOpenMoment(rms []ffmpeg.RMSFrame, scenes []ffmpeg.SceneChange, exitT, clipDur float64) (Moment, bool) {
	if len(rms) < 6 {
		return Moment{}, false
	}

	// Window starting >=5s after exit.
	startIdx := -1
	for i, f := range rms {
		if f.T >= exitT+5.0 {
			startIdx = i
			break
		}
	}
	if startIdx < 0 || startIdx >= len(rms)-3 {
		return Moment{}, false
	}

	// Average of post-exit-loud window for reference.
	loudMean := 0.0
	loudN := 0
	for i := startIdx; i < len(rms) && rms[i].T < exitT+15.0; i++ {
		loudMean += rms[i].RMSdB
		loudN++
	}
	if loudN == 0 {
		return Moment{}, false
	}
	loudMean /= float64(loudN)

	const dropDelta = 12.0
	const runFrames = 3
	dropIdx := -1
	for i := startIdx; i+runFrames < len(rms); i++ {
		quietRun := true
		for k := 0; k < runFrames; k++ {
			if rms[i+k].RMSdB > loudMean-dropDelta {
				quietRun = false
				break
			}
		}
		if quietRun {
			dropIdx = i
			break
		}
	}
	if dropIdx == -1 {
		return Moment{}, false
	}

	dropT := rms[dropIdx].T
	conf := 0.6
	for _, sc := range scenes {
		if math.Abs(sc.T-dropT) <= 2.0 {
			conf = math.Min(1.0, conf+0.25)
		}
	}
	return Moment{
		T:          dropT,
		Confidence: conf,
		Reason:     "Canopy open: RMS dropped ≥12dB at ≈" + fmtSec(dropT),
	}, true
}

// medianRMS returns the median dB of the given frames. Sorted-copy so we
// don't mutate the caller's slice; n is small.
func medianRMS(frames []ffmpeg.RMSFrame) float64 {
	if len(frames) == 0 {
		return -120
	}
	tmp := make([]float64, len(frames))
	for i, f := range frames {
		tmp[i] = f.RMSdB
	}
	for i := 1; i < len(tmp); i++ {
		v := tmp[i]
		j := i - 1
		for j >= 0 && tmp[j] > v {
			tmp[j+1] = tmp[j]
			j--
		}
		tmp[j+1] = v
	}
	return tmp[len(tmp)/2]
}

// AnalyzeFreefall is the convenience aggregate the WOW reel + photo pack
// pickers call. Runs both probes and returns Moments + the underlying
// scenes (for reels that want to align cuts to natural boundaries).
type FreefallAnalysis struct {
	RMS    []ffmpeg.RMSFrame
	Scenes []ffmpeg.SceneChange
	Exit   Moment
	Canopy Moment
}

// AnalyzeFreefallClip probes a freefall video file and returns everything
// needed for downstream pickers. Errors from individual probes are not
// fatal — caller decides whether to render with partial signal or skip.
func AnalyzeFreefallClip(ctx context.Context, path string, clipDur float64) (FreefallAnalysis, error) {
	out := FreefallAnalysis{}
	if rms, err := ffmpeg.AudioRMS(ctx, path); err == nil {
		out.RMS = rms
	}
	if scenes, err := ffmpeg.SceneChanges(ctx, path, 12.0); err == nil {
		out.Scenes = scenes
	}
	if m, ok := FindExitMoment(out.RMS, out.Scenes, clipDur); ok {
		out.Exit = m
	}
	if out.Exit.T > 0 {
		if m, ok := FindCanopyOpenMoment(out.RMS, out.Scenes, out.Exit.T, clipDur); ok {
			out.Canopy = m
		}
	}
	return out, nil
}
