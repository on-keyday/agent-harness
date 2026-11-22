package cli

import (
	"io"
	"os"
)

// stderrOr is w, or os.Stderr when w is nil: a caller that names no stderr
// writer (the TUI, the WebUI) keeps the process's own.
func stderrOr(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return os.Stderr
}
