package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
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
	// stderr → tee into:
	//   • errBuf (capped, surfaced on failure)
	//   • the studio's `log` package writer so it lands in studio.log AND
	//     in os.Stderr together. Going through `log.Writer()` (the global
	//     log target, set up at studio boot to MultiWriter(stderr, file))
	//     means ffmpeg lines persist next to the rest of the pipeline
	//     messages, in order — invaluable when triaging a hang post-mortem.
	var errBuf strings.Builder
	cmd.Stderr = io.MultiWriter(&limitedWriter{b: &errBuf, max: 32 * 1024}, log.Writer())

	log.Printf("ffmpeg start: %s", summariseArgs(args))

	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg start failed: %v", err)
		return err
	}
	log.Printf("ffmpeg pid=%d, expected duration ≈ %.1fs", cmd.Process.Pid, expectedDur)

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
		log.Printf("ffmpeg exit error: %v", waitErr)
		return fmt.Errorf("ffmpeg: %v\n%s", waitErr, errBuf.String())
	}
	log.Printf("ffmpeg exit OK")
	return nil
}

// summariseArgs trims the long filter_complex / source-path strings so
// the log line stays grep-friendly. We log the full args at debug only;
// the operator's eye care is "what did we just invoke and which inputs".
func summariseArgs(args []string) string {
	const maxArgLen = 80
	out := make([]string, 0, len(args))
	for i, a := range args {
		// Always keep flags (start with `-`) and the short args. Truncate
		// the next-arg-after-filter-complex (the giant filter graph).
		if len(a) > maxArgLen {
			a = a[:maxArgLen-1] + "…"
		}
		out = append(out, a)
		if i > 40 {
			out = append(out, "… (truncated)")
			break
		}
	}
	return strings.Join(out, " ")
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
