//go:build windows

package exec

import (
	"os"
	"time"

	"github.com/creack/pty"
)

// windowSizePollInterval is how often the Windows fallback re-queries the
// console buffer size. There is no portable resize signal on Windows that
// a Go program can subscribe to without dropping into Console API land
// (ReadConsoleInput → WINDOW_BUFFER_SIZE_EVENT, requires
// ENABLE_WINDOW_INPUT and competes with stdin reads). 250ms keeps the
// resize feel responsive while making the polling overhead negligible.
const windowSizePollInterval = 250 * time.Millisecond

// startWindowSizeForwarder polls the local terminal dimensions every
// windowSizePollInterval and invokes sendSize when they change. This is
// the Windows fallback for the Unix SIGWINCH-driven path. The returned
// func stops the goroutine.
func startWindowSizeForwarder(sendSize func() error) func() {
	done := make(chan struct{})
	go func() {
		var last pty.Winsize
		haveLast := false
		ticker := time.NewTicker(windowSizePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur, err := pty.GetsizeFull(os.Stdin)
				if err != nil {
					continue
				}
				if haveLast && *cur == last {
					continue
				}
				last = *cur
				haveLast = true
				_ = sendSize()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
