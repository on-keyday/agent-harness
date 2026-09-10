package cli

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ShutdownNoHold is the sentinel's marker for "stop for real": the server skips
// its hold sequence, so no runner is asked to keep a child alive for a server
// that is not coming back. An EMPTY sentinel — what a plain touch produces — is
// the ordinary deliberate shutdown, which holds.
//
// The sentinel carries this rather than a flag or a second signal, and the
// reason is which process needs to know. The hold is performed by the process
// being STOPPED, so a value typed at stop time cannot reach it through argv:
// `restart.py harness-server --hold-window 0` configures the NEXT server. A
// second signal would work on Linux and not on Windows, where CTRL_BREAK cannot
// reach a DETACHED_PROCESS child — which is the whole reason this sentinel
// exists. The file is already the "a human asked for this" channel, and
// daemon.py writes it BEFORE signalling, so it is on disk by the time either
// trigger fires.
const ShutdownNoHold = "nohold"

// ShutdownFileMarker reads a shutdown sentinel and returns its marker, or ""
// when there is no file, it cannot be read, or it is empty.
//
// Exported because BOTH shutdown triggers have to consult it. On Linux
// daemon.py's SIGTERM normally beats the 250 ms poll, so a signal handler that
// did not read the file itself would take the HOLDING path for a shutdown the
// operator explicitly asked not to hold — the race would decide, and it would
// usually decide wrong.
func ShutdownFileMarker(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}

// ShutdownRequestsNoHold reports whether a marker asks for a full stop.
func ShutdownRequestsNoHold(marker string) bool {
	return marker == ShutdownNoHold
}

// WatchShutdownFile polls path on a fixed interval and calls onShutdown with
// the sentinel's marker when the file first appears, then exits the goroutine.
// It exists so daemon.py / runner.py can request a graceful shutdown on
// platforms where SIGTERM cannot reach the spawned process (notably Windows,
// where daemon.py spawns binaries with DETACHED_PROCESS so
// GenerateConsoleCtrlEvent cannot deliver CTRL_BREAK_EVENT).
//
// The Python side writes bin/.run/<slot>.shutdown right before falling through
// to the existing terminate/kill escalation; this goroutine sees the file
// within at most interval and calls onShutdown, which the binary's normal
// shutdown path (the signal cancellation in main) already understands.
//
// onShutdown receives the file's CONTENT (see ShutdownNoHold), so a caller that
// distinguishes kinds of shutdown can; one that does not ignores the argument.
//
// If path is empty, no goroutine is started — the binary behaves as it did
// before this flag existed. If logger is nil, slog.Default() is used.
func WatchShutdownFile(ctx context.Context, path string, onShutdown func(marker string), interval time.Duration, logger *slog.Logger) {
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
					marker := ShutdownFileMarker(path)
					logger.Info("shutdown file detected, initiating shutdown",
						"path", path, "marker", marker)
					onShutdown(marker)
					return
				}
			}
		}
	}()
}
