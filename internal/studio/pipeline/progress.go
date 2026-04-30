package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// progressFn is called periodically while ffmpeg is encoding, with frac in
// [0,1] reflecting how far through the encoding pass we are. nil = no
// reporting (helper just drains stdout).
type progressFn func(frac float64)

// runFFmpeg launches ffmpeg with `-progress pipe:1 -nostats` prepended so
// progress lines arrive on stdout. The parser pulls `out_time_us=` lines and
// invokes onProgress (throttled to ~1 Hz) with the fraction of expectedDur
// completed. stderr is captured (capped at 32 KB) and surfaced on non-zero
// exit so callers get useful diagnostics.
//
// Caller-supplied args should still include their own `-hide_banner -y`;
// this helper does not add them. Duplicate `-nostats` is harmless.
func (r *Runner) runFFmpeg(ctx context.Context, args []string, expectedDur float64, onProgress progressFn) error {
	full := append([]string{"-progress", "pipe:1", "-nostats"}, args...)
	cmd := exec.CommandContext(ctx, r.FFmpegPath, full...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// stderr → tee both into errBuf (capped, surfaced on failure) AND into the
	// studio process's stderr live, so a hang or weird ffmpeg warning is
	// visible in the studio console window immediately rather than only after
	// ffmpeg exits non-zero.
	var errBuf strings.Builder
	cmd.Stderr = io.MultiWriter(&limitedWriter{b: &errBuf, max: 32 * 1024}, os.Stderr)

	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if onProgress == nil || expectedDur <= 0 {
			// Caller doesn't want progress, but we still need to drain stdout
			// or the pipe back-pressures ffmpeg.
			_, _ = io.Copy(io.Discard, stdout)
			return
		}
		scan := bufio.NewScanner(stdout)
		// out_time_us lines are short; default 64 KB buffer is more than enough.
		var last time.Time
		for scan.Scan() {
			line := scan.Text()
			if !strings.HasPrefix(line, "out_time_us=") {
				continue
			}
			us, err := strconv.ParseInt(strings.TrimPrefix(line, "out_time_us="), 10, 64)
			if err != nil || us <= 0 {
				continue
			}
			now := time.Now()
			if now.Sub(last) < 800*time.Millisecond {
				continue
			}
			last = now
			frac := float64(us) / 1e6 / expectedDur
			if frac > 1 {
				frac = 1
			}
			onProgress(frac)
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr != nil {
		return fmt.Errorf("ffmpeg: %v\n%s", waitErr, errBuf.String())
	}
	return nil
}

// limitedWriter caps captured stderr so a chatty ffmpeg build can't blow up
// studio memory when something goes wrong in a tight loop.
type limitedWriter struct {
	b   *strings.Builder
	max int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	rem := lw.max - lw.b.Len()
	if rem <= 0 {
		return len(p), nil
	}
	if rem < len(p) {
		lw.b.Write(p[:rem])
	} else {
		lw.b.Write(p)
	}
	return len(p), nil
}

// pctRange maps a [0,1] fraction onto an integer percentage range. Used to
// turn per-stage ffmpeg progress into a global progress-bar value.
type pctRange struct{ lo, hi int }

func (pr pctRange) at(frac float64) int {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return pr.lo + int(frac*float64(pr.hi-pr.lo))
}
