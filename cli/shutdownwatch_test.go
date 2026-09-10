package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchShutdownFile_TouchTriggersCancel(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "slot.shutdown")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	WatchShutdownFile(ctx, path, func(string) { cancel() }, 25*time.Millisecond, nil)

	// File doesn't exist yet — watcher should be polling.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("ctx was not canceled within 1s of touching the shutdown file")
	}
}

func TestWatchShutdownFile_EmptyPathIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	WatchShutdownFile(ctx, "", func(string) { cancel() }, 25*time.Millisecond, nil)

	// Give the (non-existent) goroutine a brief window to do anything stupid.
	select {
	case <-ctx.Done():
		t.Fatal("ctx was canceled despite empty path")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchShutdownFile_ContextCancelExitsGoroutine(t *testing.T) {
	// We can't observe the goroutine exit directly, but we can at
	// least verify that pre-canceling the context does not cause
	// any panic or leak when we then create the file (the watcher
	// should have exited via the ctx.Done branch).
	tmp := t.TempDir()
	path := filepath.Join(tmp, "slot.shutdown")

	ctx, cancel := context.WithCancel(context.Background())
	WatchShutdownFile(ctx, path, func(string) { cancel() }, 25*time.Millisecond, nil)
	cancel()
	time.Sleep(60 * time.Millisecond)

	// File appears after watcher should have exited — no observable
	// effect, but the test asserts no race / panic.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
}

// The sentinel's CONTENT reaches the callback, because that is how a full stop
// is told apart from a restart. Empty (a plain touch) must stay the ordinary
// holding shutdown: that is what every existing caller of daemon_down writes,
// so a marker read as "no hold" by accident would silently disable the feature
// for the whole fleet.
func TestWatchShutdownFile_CarriesTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		noHold  bool
	}{
		{"plain touch", "", false},
		{"full stop", "nohold", true},
		{"trailing newline", "nohold\n", true},
		{"upper case", "NOHOLD", true},
		{"something else", "please", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, "slot.shutdown")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			got := make(chan string, 1)
			WatchShutdownFile(ctx, path, func(m string) { got <- m }, 25*time.Millisecond, nil)
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-got:
				if ShutdownRequestsNoHold(m) != tc.noHold {
					t.Errorf("marker %q: no-hold=%v, want %v", m, ShutdownRequestsNoHold(m), tc.noHold)
				}
			case <-time.After(time.Second):
				t.Fatal("the watcher never fired")
			}
		})
	}
}

// Both triggers read the same file, and the signal path is the one that needs
// this: daemon.py writes the sentinel before signalling, and on Linux the
// signal wins, so the handler reads the file itself rather than waiting for the
// poll it will never see.
func TestShutdownFileMarker_MissingAndUnreadableAreEmpty(t *testing.T) {
	if got := ShutdownFileMarker(""); got != "" {
		t.Errorf("empty path = %q, want \"\"", got)
	}
	if got := ShutdownFileMarker(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("absent file = %q, want \"\" (the file only exists once somebody asks)", got)
	}
	dir := t.TempDir()
	if got := ShutdownFileMarker(dir); got != "" {
		t.Errorf("a directory = %q, want \"\"", got)
	}
}
