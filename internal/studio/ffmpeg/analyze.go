package ffmpeg

import (
	"sort"
)

// AudioAnalysis summarises a clip's audio energy in the two numbers the
// freefall classifier needs:
//
//   RMS90thPercentile   — 90-percentile of per-second RMS dB; freefall
//                         wind sits at the high end (-15..-25 dB), interview
//                         talking ~-30..-40 dB, plane interior ~-35..-45 dB.
//   SustainedHighSeconds — longest contiguous run of "high energy" frames,
//                         where high = top 25 % of this clip's RMS values.
//                         A 60s freefall produces a single 50-60s run; an
//                         interview chops up into many short runs.
type AudioAnalysis struct {
	RMS90thPercentile     float64
	SustainedHighSeconds  float64
	TotalFrames           int
}

// AnalyzeRMS reduces a slice of RMSFrame samples to the two scalars used by
// the smart-import classifier. Returns a zero-value AudioAnalysis when the
// input is empty (no audio stream → treat as silence).
func AnalyzeRMS(frames []RMSFrame) AudioAnalysis {
	if len(frames) == 0 {
		return AudioAnalysis{}
	}

	values := make([]float64, len(frames))
	for i, f := range frames {
		values[i] = f.RMSdB
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	p90Idx := int(0.9 * float64(len(sorted)))
	if p90Idx >= len(sorted) {
		p90Idx = len(sorted) - 1
	}
	p90 := sorted[p90Idx]

	// "High" threshold = top 25 % of this clip's RMS distribution. We pick
	// per-clip rather than absolute dB so a quiet microphone doesn't sink
	// the whole detector — the threshold rides with the clip's loudest
	// content.
	q75Idx := int(0.75 * float64(len(sorted)))
	if q75Idx >= len(sorted) {
		q75Idx = len(sorted) - 1
	}
	threshold := sorted[q75Idx]

	// Walk the time-ordered frames, find the longest contiguous run above
	// threshold. Each frame represents 1 second per the astats config.
	longest, current := 0, 0
	for _, f := range frames {
		if f.RMSdB >= threshold {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}

	return AudioAnalysis{
		RMS90thPercentile:    p90,
		SustainedHighSeconds: float64(longest),
		TotalFrames:          len(frames),
	}
}
