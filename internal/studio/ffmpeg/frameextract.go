package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// ExtractFrame grabs a single high-quality JPEG at timestamp `t` (seconds)
// from `srcPath` and writes it to `dstPath`. Uses fast keyframe seek + a
// short scan for accurate frame placement (`-ss <t> -i …` is fast but
// snaps to nearest keyframe; the second `-ss` after `-i` does an exact
// seek). qScale=2 is high-quality JPEG (smaller than -qscale 1, no
// visible difference for photo-pack thumbnails or originals).
func ExtractFrame(ctx context.Context, srcPath string, t float64, dstPath string) error {
	return ExtractFrameWithWatermark(ctx, srcPath, t, dstPath, WatermarkOptions{})
}

// WatermarkOptions configures an optional logo overlay on top of the
// extracted JPEG. Zero value (empty Path) = no watermark.
type WatermarkOptions struct {
	Path       string // PNG (or any ffmpeg-readable format) on disk
	SizePct    int    // width as % of frame width (default 12)
	OpacityPct int    // 0..100 (default 80)
	Position   string // "top-left" | "top-right" | "bottom-left" | "bottom-right" (default)
}

// ExtractFrameWithWatermark is the variant that overlays a logo on the
// JPEG. Used by the photo pack so client-facing stills carry the club's
// branding the same way the rendered videos do.
func ExtractFrameWithWatermark(ctx context.Context, srcPath string, t float64, dstPath string, wm WatermarkOptions) error {
	if srcPath == "" || dstPath == "" {
		return errors.New("srcPath and dstPath required")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg not found on PATH")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tStr := strconv.FormatFloat(t, 'f', 3, 64)
	preSeek := strconv.FormatFloat(maxFloat(t-2.0, 0), 'f', 3, 64)
	innerSeek := strconv.FormatFloat(minFloat(t-(t-maxFloat(t-2.0, 0)), 2.0), 'f', 3, 64)

	args := []string{
		"-nostats",
		"-hide_banner",
		"-y",
		"-ss", preSeek,
		"-i", srcPath,
	}
	if wm.Path != "" {
		args = append(args, "-i", wm.Path)
	}
	args = append(args,
		"-ss", innerSeek,
		"-frames:v", "1",
	)
	if wm.Path != "" {
		args = append(args,
			"-filter_complex", buildWatermarkFilter(wm),
		)
	}
	args = append(args,
		"-q:v", "2",
		dstPath,
	)

	cmd := exec.CommandContext(cctx, "ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("ffmpeg extract @%ss: %w; output: %s", tStr, err, string(out))
	}
	if info, err := os.Stat(dstPath); err != nil || info.Size() == 0 {
		_ = os.Remove(dstPath)
		return fmt.Errorf("extracted frame missing or empty (@%ss)", tStr)
	}
	return nil
}

// buildWatermarkFilter mirrors the video pipeline's overlay layout so a
// photo's logo lands in the same corner as the rendered video's. Defaults
// to bottom-right at 12% width / 80% opacity if the bundle didn't set them.
//
// Watermark width is computed as a percentage of 1920px (the canonical
// frame width — same assumption runner_reel.go makes). For 4K source
// clips the logo will read smaller proportionally; that's acceptable
// fallback because we can't query main width from within scale.
func buildWatermarkFilter(wm WatermarkOptions) string {
	sizePct := wm.SizePct
	if sizePct <= 0 {
		sizePct = 12
	}
	opacity := float64(wm.OpacityPct) / 100.0
	if opacity <= 0 {
		opacity = 0.80
	}
	wmW := 1920 * sizePct / 100
	if wmW < 32 {
		wmW = 32
	}
	const margin = 24
	var posExpr string
	switch wm.Position {
	case "top-left":
		posExpr = fmt.Sprintf("x=%d:y=%d", margin, margin)
	case "top-right":
		posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=%d", margin, margin)
	case "bottom-left":
		posExpr = fmt.Sprintf("x=%d:y=main_h-overlay_h-%d", margin, margin)
	default: // bottom-right
		posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=main_h-overlay_h-%d", margin, margin)
	}
	return fmt.Sprintf(
		"[1:v]scale=%d:-1:flags=lanczos,format=rgba,colorchannelmixer=aa=%s[wm];"+
			"[0:v][wm]overlay=%s",
		wmW,
		strconv.FormatFloat(opacity, 'f', 2, 64),
		posExpr,
	)
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
