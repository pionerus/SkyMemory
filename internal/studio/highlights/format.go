package highlights

import "fmt"

// fmtSec — same shape as trim.fmtSec. Duplicated here so highlights stays
// a leaf package without circular imports back to trim.
func fmtSec(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
}
