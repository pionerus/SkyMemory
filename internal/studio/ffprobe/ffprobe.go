// Package ffprobe wraps the system `ffprobe` binary to extract metadata
// (codec, duration, fps, resolution) from a video file. Studio uses this to
// validate uploads before storing them.
//
// We expect ffprobe on PATH. Production builds will bundle it; for now studio
// logs a warning at boot if it's missing and uploads are accepted but unprobed.
package ffprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Metadata is the slice of ffprobe's output that studio actually uses.
type Metadata struct {
	DurationSeconds float64
	Codec           string  // primary video codec, e.g. "h264", "hevc"
	Width           int
	Height          int
	FPS             float64 // numeric form of r_frame_rate (e.g. 30000/1001 -> 29.97)
	HasAudio        bool
	AudioCodec      string
}

// IsAvailable returns true if ffprobe is on PATH.
// Studio calls this once at boot to decide whether to surface a warning to the operator.
func IsAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// Probe runs `ffprobe -print_format json -show_streams -show_format <path>`
// and returns the parsed metadata. Times out after 15s.
func Probe(ctx context.Context, path string) (*Metadata, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, errors.New("ffprobe not found on PATH — install ffmpeg or set PATH")
	}

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ffprobe exit %d: %s", ee.ExitCode(), string(ee.Stderr))
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var raw rawProbe
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse ffprobe json: %w", err)
	}

	m := &Metadata{}
	if d, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
		m.DurationSeconds = d
	}

	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if m.Codec == "" {
				m.Codec = s.CodecName
				m.Width = s.Width
				m.Height = s.Height
				m.FPS = parseFraction(s.RFrameRate)
			}
		case "audio":
			if !m.HasAudio {
				m.HasAudio = true
				m.AudioCodec = s.CodecName
			}
		}
	}

	if m.Codec == "" {
		return nil, errors.New("no video stream found")
	}
	return m, nil
}

// parseFraction converts ffprobe's "30000/1001" -> 29.97 (or "30/1" -> 30).
// Returns 0 on parse failure (caller should treat 0 as unknown).
func parseFraction(f string) float64 {
	parts := strings.SplitN(f, "/", 2)
	if len(parts) != 2 {
		v, _ := strconv.ParseFloat(f, 64)
		return v
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}
	return num / den
}

// rawProbe matches ffprobe -print_format json. Only fields we care about.
type rawProbe struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []rawStream `json:"streams"`
}

type rawStream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	RFrameRate string `json:"r_frame_rate"`
}
