//go:build !windows

package exec

import (
	"os"
	"os/signal"
	"syscall"
)

// startWindowSizeForwarder spawns a goroutine that calls sendSize whenever
// the local terminal is resized. On Unix systems the kernel delivers
// SIGWINCH to this process when the controlling terminal changes
// dimensions, so we just listen for that signal. The returned func stops
// the goroutine and unregisters the handler — RemoteShell defers it.
func startWindowSizeForwarder(sendSize func() error) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-winch:
				_ = sendSize()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(winch)
		close(done)
	}
}
