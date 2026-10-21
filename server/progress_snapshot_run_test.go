package server

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A deliberate stop of a RUNNING server writes the snapshot: the unit tests of
// writeProgressSnapshot cannot see whether the stop path reaches it, and a
// SIGTERM'd harness-server was measured exiting without it.
func TestRun_DeliberateStopWritesProgressSnapshot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dir := t.TempDir()
	s := New(Config{Addr: addr, DataDir: dir})
	id := s.tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, authority.Scope{}, "")
	s.tasks.SetProgress(id, "stage 9", 99)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // let the listener come up
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := os.Stat(progressSnapshotPath(dir)); err != nil {
		t.Fatalf("no progress snapshot after a deliberate stop: %v", err)
	}
}

// The path a real SIGTERM takes: the hold sequence marks the server shutting
// down and closes the listeners, serve returns through the http-server exit,
// and main returns — all BEFORE the root context is cancelled. Measured on a
// dummy instance: no snapshot, because the write was gated on ctx alone.
func TestRun_HoldPathStopWritesProgressSnapshot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	dir := t.TempDir()
	s := New(Config{Addr: addr, DataDir: dir})
	id := s.tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, authority.Scope{}, "")
	s.tasks.SetProgress(id, "stage 9", 99)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	s.shuttingDown.Store(true) // what RunHoldSequence sets first
	// Its own goroutine, as in the binary (the signal handler): closing the
	// listeners waits on the serve loop, so it cannot run on the caller.
	go s.closeListeners() // what it does last
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the listeners closed")
	}
	if _, err := os.Stat(progressSnapshotPath(dir)); err != nil {
		t.Fatalf("no progress snapshot after a hold-path stop: %v", err)
	}
}
