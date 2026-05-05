package highlights

import (
	"image"
	"image/color"
	_ "image/jpeg"
	"math"
	"os"
)

// SharpnessScore returns a Laplacian-variance estimate of how sharp a JPEG
// is. Higher = sharper. Pure Go (image/jpeg + manual 3×3 kernel) so no
// external deps. Calibrated empirically:
//
//	  >  300 — clearly sharp (good photo candidate)
//	  100..300 — passable (most freefall handheld footage)
//	  <  100 — likely motion-blurred; reject if a sharper alternative exists
//
// Returns 0 on read/decode error. Caller should compare relative scores
// across candidates rather than rely on absolute thresholds.
func SharpnessScore(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return 0
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w < 8 || h < 8 {
		return 0
	}

	// Downsample to ≤ 480px on the longest side to keep the cost flat for
	// huge 4K source frames. Decimation by integer factor; nearest-neighbour
	// is fine for sharpness statistics.
	targetMax := 480
	step := 1
	for max(w, h)/step > targetMax {
		step++
	}
	dw, dh := w/step, h/step
	if dw < 8 || dh < 8 {
		return 0
	}

	// Build a luma plane via ITU-R BT.601: 0.299 R + 0.587 G + 0.114 B.
	luma := make([]float64, dw*dh)
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			r, g, b, _ := img.At(bounds.Min.X+x*step, bounds.Min.Y+y*step).RGBA()
			// RGBA returns pre-multiplied 0..65535. Convert to 0..255.
			rN := float64(r >> 8)
			gN := float64(g >> 8)
			bN := float64(b >> 8)
			luma[y*dw+x] = 0.299*rN + 0.587*gN + 0.114*bN
		}
	}

	// 3×3 Laplacian kernel: [0,1,0; 1,-4,1; 0,1,0]. Variance of the response
	// is the standard sharpness proxy (OpenCV's `cv2.Laplacian().var()`).
	mean := 0.0
	resp := make([]float64, 0, (dw-2)*(dh-2))
	for y := 1; y < dh-1; y++ {
		for x := 1; x < dw-1; x++ {
			c := luma[y*dw+x]
			n := luma[(y-1)*dw+x]
			s := luma[(y+1)*dw+x]
			e := luma[y*dw+(x+1)]
			w := luma[y*dw+(x-1)]
			v := n + s + e + w - 4*c
			resp = append(resp, v)
			mean += v
		}
	}
	if len(resp) == 0 {
		return 0
	}
	mean /= float64(len(resp))
	variance := 0.0
	for _, v := range resp {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(resp))
	return variance
}

// luma is exposed only so tests can sanity-check the helper; not used
// outside this file in production.
func _luma(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	rn := float64(r >> 8)
	gn := float64(g >> 8)
	bn := float64(b >> 8)
	return 0.299*rn + 0.587*gn + 0.114*bn
}

// _ avoid "imported and not used" if image/color stays as a single ref.
var _ = math.Sqrt
