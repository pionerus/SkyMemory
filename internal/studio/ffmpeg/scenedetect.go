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

// SceneChange is one frame at which the scene-change detector flagged a
// significant visual cut. Score is the underlying scdet score (0..100);
// higher = bigger change. Operators reading this for highlights typically
// only care that score > some floor.
type SceneChange struct {
	T     float64
	Score float64
}

// SceneChanges runs the ffmpeg `scdet` filter on the given video file and
// returns all detected scene transitions sorted by time. Threshold tunes
// sensitivity — 10 is a sane default for skydive footage where the cabin
// → sky boundary is dramatic but in-air angle changes are subtle.
//
// We send video through `scdet` with `s=1` (output even non-changed frames'
// score) and `t=<th>` (threshold for IS_SCENE_CHANGE flag). Then parse the
// per-frame metadata in stderr — same pattern as DetectSilence/AudioRMS.
func SceneChanges(ctx context.Context, path string, threshold float64) ([]SceneChange, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not found on PATH — install ffmpeg")
	}

	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// scdet only operates on video; we discard audio. The filter prints to
	// stderr a metadata line per detected scene change of the form:
	//
	//   [Parsed_scdet_0 @ 0xabc] lavfi.scd.score: 28.314
	//   [Parsed_scdet_0 @ 0xabc] lavfi.scd.time: 12.456
	//
	// Only frames that exceed the threshold get the time line; we pair
	// score+time as they arrive.
	filter := "scdet=threshold=" + strconv.FormatFloat(threshold, 'f', 2, 64)

	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-nostats",
		"-hide_banner",
		"-i", path,
		"-an",          // ignore audio
		"-vf", filter,
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

	out := parseSceneLines(stderr)

	if err := cmd.Wait(); err != nil {
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

// scdet emits these as separate lines; we track the most recent score and
// pair it with the next time line we see.
var (
	reScdetScore = regexp.MustCompile(`lavfi\.scd\.score:\s*([\-0-9.eE+]+)`)
	reScdetTime  = regexp.MustCompile(`lavfi\.scd\.time:\s*([0-9.+-]+)`)
)

func parseSceneLines(r io.Reader) []SceneChange {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var out []SceneChange
	var pendingScore float64
	var havePending bool

	for scanner.Scan() {
		line := scanner.Text()
		if m := reScdetScore.FindStringSubmatch(line); m != nil {
			s, err := strconv.ParseFloat(m[1], 64)
			if err == nil {
				pendingScore = s
				havePending = true
			}
			continue
		}
		if m := reScdetTime.FindStringSubmatch(line); m != nil && havePending {
			t, err := strconv.ParseFloat(m[1], 64)
			if err == nil {
				out = append(out, SceneChange{T: t, Score: pendingScore})
			}
			havePending = false
		}
	}
	return out
}
