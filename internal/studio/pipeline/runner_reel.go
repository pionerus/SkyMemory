package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pionerus/freefall/internal/studio/branding"
	"github.com/pionerus/freefall/internal/studio/highlights"
)

// ReelAspect picks the output frame shape.
//
//	AspectHorizontal — 1920×1080, no reframe (WOW reel default)
//	AspectVertical   — 1080×1920, centre-cropped from 16:9 source (IG reel)
type ReelAspect string

const (
	AspectHorizontal ReelAspect = "16:9"
	AspectVertical   ReelAspect = "9:16"
)

// ReelOptions configures a multi-cut, music-only deliverable render. There
// is no source audio — operator's wind/voice is always replaced with the
// picked music track. There are no cut zones (these reels run on
// pre-curated highlights segments) and no intro/outro bumpers (the reels
// are short-form social pieces).
type ReelOptions struct {
	Segments       []highlights.Segment
	MusicTrackPath string          // empty = silent reel
	Aspect         ReelAspect      // 16:9 or 9:16
	Branding       branding.Bundle // watermark overlay; intro/outro ignored
	OutputPath     string
	CrossfadeSec   float64 // typical 0.4

	// OnProgress is called while ffmpeg encodes, with frac in [0,1]. nil =
	// no reporting. Throttled inside runFFmpeg to ~1 Hz.
	OnProgress func(frac float64)
}

// reelNormalizeChain returns the per-clip ffmpeg filter that turns any
// source resolution into the target reel frame: 1920×1080 horizontal or
// 1080×1920 vertical (centre-cropped from the source's centre 9:16 strip).
//
// For vertical: crop=ih*9/16:ih: keeps the full clip height, takes a
// vertical strip from the centre of the frame, then upscales to 1080×1920.
// This is the cleanest single-filter option without breaking aspect.
func reelNormalizeChain(aspect ReelAspect) string {
	switch aspect {
	case AspectVertical:
		return "crop=ih*9/16:ih:(iw-ih*9/16)/2:0," +
			"scale=1080:1920:force_original_aspect_ratio=decrease," +
			"pad=1080:1920:(ow-iw)/2:(oh-ih)/2:color=black," +
			"fps=30,format=yuv420p"
	default:
		// Reels always render at 1080p — they target Instagram / WhatsApp,
		// no benefit to a 4K reel. Keep the canonical 1080p chain regardless
		// of the project's main-edit resolution choice.
		return "scale=1920:1080:force_original_aspect_ratio=decrease," +
			"pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black," +
			"fps=30,format=yuv420p"
	}
}

// reelFrameSize returns "WIDTHxHEIGHT" for the given aspect — used for the
// output -s flag and watermark sizing.
func reelFrameSize(aspect ReelAspect) (w, h int) {
	if aspect == AspectVertical {
		return 1080, 1920
	}
	return 1920, 1080
}

// RunReel renders a multi-cut, music-only short-form deliverable. Single
// ffmpeg pass — trim each segment, xfade them together, optionally overlay
// watermark, mux music as the sole audio track.
//
// Returns the absolute path of the output file (== opts.OutputPath on
// success). Errors are wrapped with the failing stage so callers can log.
func (r *Runner) RunReel(ctx context.Context, opts ReelOptions) (string, error) {
	if r.FFmpegPath == "" {
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return "", errors.New("ffmpeg not on PATH")
		}
		r.FFmpegPath = p
	}
	if len(opts.Segments) < 2 {
		return "", fmt.Errorf("need at least 2 segments, got %d", len(opts.Segments))
	}
	if opts.OutputPath == "" {
		return "", errors.New("OutputPath required")
	}
	if opts.CrossfadeSec <= 0 {
		opts.CrossfadeSec = 0.4
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir output dir: %w", err)
	}

	// === Build inputs ===
	args := []string{"-hide_banner", "-y"}
	for _, s := range opts.Segments {
		args = append(args, "-i", s.SourcePath)
	}
	wmIdx := -1
	if opts.Branding.HasWatermark() {
		wmIdx = len(opts.Segments)
		args = append(args, "-i", opts.Branding.WatermarkPath)
	}
	musicIdx := -1
	if opts.MusicTrackPath != "" {
		musicIdx = len(args)/2 - 1 // (number of -i flags so far) - 1
		args = append(args, "-i", opts.MusicTrackPath)
		musicIdx = (len(args) - 2) / 2 // recompute properly: each -i adds 2 args
	}

	// musicIdx should be the input index, not args index. Inputs are 1 per
	// "-i path" pair; len(opts.Segments) clips, then optional watermark, then
	// optional music. Recompute cleanly:
	wmIdx = -1
	musicIdx = -1
	idx := len(opts.Segments)
	if opts.Branding.HasWatermark() {
		wmIdx = idx
		idx++
	}
	if opts.MusicTrackPath != "" {
		musicIdx = idx
		idx++
	}

	// === Per-segment normalise chain ===
	var fc strings.Builder
	normChain := reelNormalizeChain(opts.Aspect)
	segDurs := make([]float64, len(opts.Segments))
	for i, s := range opts.Segments {
		segDurs[i] = s.End - s.Start
		fc.WriteString(fmt.Sprintf(
			"[%d:v]trim=start=%s:end=%s,setpts=PTS-STARTPTS,%s[v%d];",
			i, floatStr(s.Start), floatStr(s.End), normChain, i,
		))
	}

	// === xfade chain ===
	// First pair: [v0][v1]xfade=offset=dur0-cf  → [xf1]
	// Then [xfN-1][vN]xfade=offset=cumDur-cf*N  → [xfN]
	prev := "[v0]"
	cumDur := segDurs[0]
	for i := 1; i < len(opts.Segments); i++ {
		cf := opts.CrossfadeSec
		if cf > segDurs[i]/3 {
			cf = segDurs[i] / 3
		}
		offset := cumDur - cf
		next := fmt.Sprintf("[xf%d]", i)
		fc.WriteString(fmt.Sprintf(
			"%s[v%d]xfade=transition=fade:duration=%s:offset=%s%s;",
			prev, i, floatStr(cf), floatStr(offset), next,
		))
		prev = next
		cumDur += segDurs[i] - cf
	}
	mainV := prev

	// === Watermark overlay (optional) ===
	if wmIdx >= 0 {
		fW, _ := reelFrameSize(opts.Aspect)
		wmW := fW * opts.Branding.WatermarkSizePct / 100
		if wmW < 32 {
			wmW = 32
		}
		opacity := float64(opts.Branding.WatermarkOpacityPct) / 100.0
		const margin = 24
		var posExpr string
		switch opts.Branding.WatermarkPosition {
		case "bottom-left":
			posExpr = fmt.Sprintf("x=%d:y=main_h-overlay_h-%d", margin, margin)
		case "top-right":
			posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=%d", margin, margin)
		case "top-left":
			posExpr = fmt.Sprintf("x=%d:y=%d", margin, margin)
		default:
			posExpr = fmt.Sprintf("x=main_w-overlay_w-%d:y=main_h-overlay_h-%d", margin, margin)
		}
		fc.WriteString(fmt.Sprintf(
			"[%d:v]scale=%d:-1,format=rgba,colorchannelmixer=aa=%s[wmready];",
			wmIdx, wmW, floatStr(opacity),
		))
		fc.WriteString(fmt.Sprintf("%s[wmready]overlay=%s:format=auto[mainVwm];", mainV, posExpr))
		mainV = "[mainVwm]"
	}

	// === Audio: music only ===
	mainA := ""
	if musicIdx >= 0 {
		fc.WriteString(fmt.Sprintf(
			"[%d:a]aloop=loop=-1:size=2e9,afade=t=in:st=0:d=0.6,"+
				"atrim=duration=%s,afade=t=out:st=%s:d=0.8,"+
				"aformat=channel_layouts=stereo:sample_rates=48000[reelA];",
			musicIdx,
			floatStr(cumDur),
			floatStr(cumDur-0.8),
		))
		mainA = "[reelA]"
	} else {
		// Silent track sized to total duration so muxer is happy.
		fc.WriteString(fmt.Sprintf(
			"anullsrc=channel_layout=stereo:sample_rate=48000,atrim=duration=%s,aformat=channel_layouts=stereo:sample_rates=48000[reelA];",
			floatStr(cumDur),
		))
		mainA = "[reelA]"
	}

	// === Output ===
	args = append(args,
		"-filter_complex", strings.TrimRight(fc.String(), ";"),
		"-map", mainV,
		"-map", mainA,
		"-fps_mode", "cfr",
	)
	if r.useQSV {
		args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20")
	}
	args = append(args,
		"-c:a", "aac",
		"-b:a", "192k",
		"-ar", "48000",
		"-movflags", "+faststart",
		opts.OutputPath,
	)

	// Drop the args that runFFmpeg adds itself (-progress / -nostats), then
	// hand off to the shared helper so reel + main pipeline progress flow
	// through the same code path. cumDur is the expected output length.
	log.Printf("reel ffmpeg start: aspect=%s out=%s expected=%.1fs", opts.Aspect, opts.OutputPath, cumDur)
	if err := r.runFFmpeg(ctx, args, cumDur, opts.OnProgress); err != nil {
		return "", fmt.Errorf("ffmpeg reel: %w", err)
	}
	log.Printf("reel ffmpeg exit OK: %s", opts.OutputPath)
	return opts.OutputPath, nil
}
