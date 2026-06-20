////go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// syncBuf is a concurrency-safe io.Writer wrapping a bytes.Buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// pollUntilContains polls fn (up to timeout, sleeping step between attempts)
// until the returned string contains want, then returns true. Returns false on
// timeout, and sets t.Errorf with the last-seen value.
func pollUntilContains(t *testing.T, want string, timeout, step time.Duration, fn func() string) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = fn()
		if strings.Contains(last, want) {
			return true
		}
		time.Sleep(step)
	}
	t.Errorf("timed out waiting for %q; last value:\n%s", want, last)
	return false
}

// TestNotifyEgressHookE2E verifies that sending a notification causes the
// configured NotifyHook to be executed with the JSON payload on stdin.
func TestNotifyEgressHookE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	// Create a hook script that captures its stdin to outFile.
	hookDir := t.TempDir()
	outFile := hookDir + "/hook-output.json"
	hookScript := hookDir + "/hook.sh"
	if err := os.WriteFile(hookScript, []byte("#!/bin/sh\ncat > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18552"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := server.New(server.Config{
		Addr:       addr,
		DataDir:    t.TempDir(),
		NotifyHook: hookScript,
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	// Give the server a moment to start listening.
	time.Sleep(300 * time.Millisecond)

	if err := cli.Notify(ctx, peerCID, "info", "title", "hello-egress"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// The hook runs asynchronously: poll until the output file is non-empty.
	if !pollUntilContains(t, "hello-egress", 3*time.Second, 50*time.Millisecond, func() string {
		b, _ := os.ReadFile(outFile)
		return string(b)
	}) {
		t.FailNow()
	}
	got, _ := os.ReadFile(outFile)
	if !strings.Contains(string(got), `"level":"info"`) {
		t.Errorf("hook output missing \"level\":\"info\"; got:\n%s", got)
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel — leaking goroutine")
	}
}

// TestNotifyLiveReplayE2E verifies two properties of the notifications topic:
//  1. Ring backlog replay: a late subscriber receives events that were sent before it joined.
//  2. Live fan-out: the subscriber receives events sent after it joined.
func TestNotifyLiveReplayE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	addr := "127.0.0.1:18553"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := server.New(server.Config{
		Addr:    addr,
		DataDir: t.TempDir(),
		// No NotifyHook — live-only recording.
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	// Give the server a moment to start listening.
	time.Sleep(300 * time.Millisecond)

	// Client A: send a notification before client B subscribes (backlog test).
	clientA, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer clientA.Close()

	if err := clientA.Notify(ctx, "info", "", "backlog-1"); err != nil {
		t.Fatalf("Notify backlog-1: %v", err)
	}
	// Give the server a moment to record backlog-1 to the ring.
	time.Sleep(100 * time.Millisecond)

	// Client B: subscribe to notifications. Ring backlog should arrive first.
	clientB, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer clientB.Close()

	var safeBuf syncBuf
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	go func() {
		if err := clientB.WatchNotifications(watchCtx, &safeBuf); err != nil && watchCtx.Err() == nil {
			t.Logf("WatchNotifications returned: %v", err)
		}
	}()

	// Phase 1: verify ring backlog replay delivers backlog-1.
	if !pollUntilContains(t, "backlog-1", 3*time.Second, 50*time.Millisecond, safeBuf.String) {
		t.Fatalf("ring replay did not deliver backlog-1; buf:\n%s", safeBuf.String())
	}
	t.Log("ring replay verified: backlog-1 received by late subscriber")

	// Phase 2: send a live event; verify fan-out delivers it to the subscriber.
	if err := clientA.Notify(ctx, "warn", "", "live-2"); err != nil {
		t.Fatalf("Notify live-2: %v", err)
	}
	if !pollUntilContains(t, "live-2", 3*time.Second, 50*time.Millisecond, safeBuf.String) {
		t.Fatalf("live fan-out did not deliver live-2; buf:\n%s", safeBuf.String())
	}
	t.Log("live fan-out verified: live-2 received by subscriber")

	watchCancel()
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel — leaking goroutine")
	}
}
