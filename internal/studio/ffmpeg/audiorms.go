package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// RMSFrame is one 1-second window of audio energy.
//
//	T     — pts_time of the window in seconds (0, 1, 2, …)
//	RMSdB — overall RMS level in dB (negative; quieter = more negative)
//
// Used by the freefall and landing auto-trim heuristics to find the
// wind-roar onset/drop and the touchdown impact spike.
type RMSFrame struct {
	T     float64
	RMSdB float64
}

// AudioRMS runs ffmpeg with the astats filter producing a per-1-second
// RMS_level metric, parsed from ffmpeg stderr. Mirrors the shape and
// timeout/exit-code policy of DetectSilence.
//
// Skydive thresholds (rough guide, calibrate per dropzone if needed):
//
//	freefall wind roar: > -25 dB
//	canopy / interview: -45 .. -30 dB
//	silence            : < -50 dB
//	landing impact     : sudden frame > -20 dB after a -30..-40 dB tail
//
// Returns an empty slice (not an error) when ffmpeg ran but produced no
// astats lines — typically a clip with no audio stream.
func AudioRMS(ctx context.Context, path string) ([]RMSFrame, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not found on PATH — install ffmpeg")
	}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// astats: emit overall stats every `length` seconds with reset between windows.
	// ametadata=print: dump metadata to stderr (one line per key per frame). Stick
	// to the single key we care about so the parser stays cheap.
	filter := "astats=metadata=1:reset=1:length=1," +
		"ametadata=mode=print:key=lavfi.astats.Overall.RMS_level"

	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-nostats",
		"-hide_banner",
		"-i", path,
		"-vn",          // audio-only decode
		"-af", filter,
		"-f", "null",
		"-",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	out := parseRMSLines(stderr)

	if err := cmd.Wait(); err != nil {
		// Same tolerance as DetectSilence: ffmpeg often returns non-zero on
		// /dev/null sinks; only fail if we got nothing back.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(out) > 0 {
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ametadata=print emits paired lines per frame, e.g.:
//
//	frame:25  pts:48000   pts_time:1
//	lavfi.astats.Overall.RMS_level=-23.456789
//
// We track the last seen pts_time and pair it with the next RMS_level we see.
var (
	rePtsTime  = regexp.MustCompile(`pts_time:([0-9.+-]+)`)
	reRMSLevel = regexp.MustCompile(`lavfi\.astats\.Overall\.RMS_level=([\-0-9.eE+]+)`)
)

func parseRMSLines(r io.Reader) []RMSFrame {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var out []RMSFrame
	var pendingT float64
	var havePending bool

	for scanner.Scan() {
		line := scanner.Text()
		if m := rePtsTime.FindStringSubmatch(line); m != nil {
			t, err := strconv.ParseFloat(m[1], 64)
			if err == nil {
				pendingT = t
				havePending = true
			}
			continue
		}
		if m := reRMSLevel.FindStringSubmatch(line); m != nil && havePending {
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				havePending = false
				continue
			}
			// astats emits "-inf" as a string sometimes (parsed as -Inf by Go);
			// clamp to a reasonable floor so heuristics treat it as "very quiet".
			if v < -120 {
				v = -120
			}
			out = append(out, RMSFrame{T: pendingT, RMSdB: v})
			havePending = false
		}
	}
	return out
}
