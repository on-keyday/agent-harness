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

	WatchShutdownFile(ctx, path, cancel, 25*time.Millisecond, nil)

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

	WatchShutdownFile(ctx, "", cancel, 25*time.Millisecond, nil)

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
	WatchShutdownFile(ctx, path, cancel, 25*time.Millisecond, nil)
	cancel()
	time.Sleep(60 * time.Millisecond)

	// File appears after watcher should have exited — no observable
	// effect, but the test asserts no race / panic.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
}
