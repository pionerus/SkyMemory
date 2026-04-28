// Package ffmpeg wraps the system ffmpeg binary for audio-analysis tasks
// (silencedetect, RMS) used by the auto-trim heuristics.
package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// SilenceWindow is one detected stretch of below-threshold audio in seconds.
// End == Start when the parser saw silence_start but never the matching
// silence_end (clip ended in silence). Caller should treat End=clipDuration
// in that case.
type SilenceWindow struct {
	Start float64
	End   float64
}

// IsAvailable returns true if ffmpeg is on PATH.
func IsAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// DetectSilence runs `ffmpeg -i path -af silencedetect=noise=<th>:d=<min>` and
// returns the parsed silence windows. Times out at 60s — long enough for ~10
// minute 4K clips on consumer hardware (silencedetect only decodes audio).
//
//   thresholdDB: dB level below which audio is considered silent. Typical: -30 (sensitive),
//                -50 (only true silence). Skydive interviews work well at -30.
//   minDuration: shortest gap to report (seconds). Use 0.5 to avoid catching
//                normal speech pauses.
func DetectSilence(ctx context.Context, path string, thresholdDB float64, minDuration float64) ([]SilenceWindow, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not found on PATH — install ffmpeg")
	}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	filter := "silencedetect=noise=" + formatDB(thresholdDB) + ":d=" + strconv.FormatFloat(minDuration, 'f', 3, 64)

	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-nostats",
		"-hide_banner",
		"-i", path,
		"-af", filter,
		"-vn",         // disable video — silencedetect only needs audio
		"-f", "null",  // discard output
		"-",
	)
	// silencedetect prints to stderr.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	out := parseSilenceLines(stderr)

	if err := cmd.Wait(); err != nil {
		// ffmpeg often returns non-zero even on success when output is /dev/null —
		// only error out if we got nothing back AND it was a real error.
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

// formatDB renders -30 -> "-30dB", -7.5 -> "-7.5dB". ffmpeg's silencedetect accepts both.
func formatDB(db float64) string {
	return strconv.FormatFloat(db, 'f', -1, 64) + "dB"
}

// silenceStart line example:  [silencedetect @ 0xabc] silence_start: 0.123
// silenceEnd   line example:  [silencedetect @ 0xabc] silence_end: 1.456 | silence_duration: 1.333
var (
	reSilStart = regexp.MustCompile(`silence_start:\s*([0-9.+-]+)`)
	reSilEnd   = regexp.MustCompile(`silence_end:\s*([0-9.+-]+)`)
)

// parseSilenceLines reads the stderr stream line by line. Pairs each
// silence_start with the next silence_end. A trailing silence_start without a
// matching end is emitted as Start=End so callers can fix it up using clip duration.
func parseSilenceLines(r interface {
	Read([]byte) (int, error)
}) []SilenceWindow {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20) // up to 1MB lines just in case

	var out []SilenceWindow
	var pending *SilenceWindow

	for scanner.Scan() {
		line := scanner.Text()
		if m := reSilStart.FindStringSubmatch(line); m != nil {
			t, _ := strconv.ParseFloat(m[1], 64)
			pending = &SilenceWindow{Start: t, End: t}
			out = append(out, *pending)
			continue
		}
		if m := reSilEnd.FindStringSubmatch(line); m != nil && pending != nil {
			t, _ := strconv.ParseFloat(m[1], 64)
			out[len(out)-1].End = t
			pending = nil
		}
	}
	return out
}
