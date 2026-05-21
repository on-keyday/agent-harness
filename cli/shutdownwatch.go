package cli

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// WatchShutdownFile polls path on a fixed interval and calls cancel
// when the file first appears, then exits the goroutine. It exists so
// daemon.py / runner.py can request a graceful shutdown on platforms
// where SIGTERM cannot reach the spawned process (notably Windows,
// where daemon.py spawns binaries with DETACHED_PROCESS so
// GenerateConsoleCtrlEvent cannot deliver CTRL_BREAK_EVENT).
//
// The Python side touches bin/.run/<slot>.shutdown right before
// falling through to the existing terminate/kill escalation; this
// goroutine sees the file within at most interval and cancels the
// passed context, which the binary's normal shutdown path (the
// signal.NotifyContext cancellation in main) already understands.
//
// If path is empty, no goroutine is started — the binary behaves as
// it did before this flag existed. If logger is nil, slog.Default()
// is used.
func WatchShutdownFile(ctx context.Context, path string, cancel context.CancelFunc, interval time.Duration, logger *slog.Logger) {
	if path == "" {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := os.Stat(path); err == nil {
					logger.Info("shutdown file detected, initiating shutdown", "path", path)
					cancel()
					return
				}
			}
		}
	}()
}
