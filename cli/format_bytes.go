package cli

import "fmt"

// FormatByteCount renders a byte total for a narrow column.
//
// It lives here rather than in the TUI, where it started, because the forward
// counters put byte sizes on all three operator surfaces at once: the CLI row,
// the TUI pane and — over the wasm bridge — the browser. A second copy written
// in JS is the shape that drifted for scope labels, and there is no reason to
// repeat it.
//
// Zero renders as "0", never "": a forward that has carried nothing is a
// measurement, and a blank cell reads as "this row does not report traffic".
func FormatByteCount(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	case n == 0:
		return "0"
	}
	return fmt.Sprintf("%dB", n)
}
