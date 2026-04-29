////go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// TestSubmitWakeE2E asserts that publishing to an agentboard topic causes the
// agent process running under agent-runner to receive the wake marker on its
// stdin within ~2s.
//
// Architecture of the positive path:
//
//  1. An in-process server + board are started, and a runner is dialled in.
//  2. A task is submitted via cli.Submit; the runner spawns fake-claude-wake.sh.
//  3. fake-claude-wake.sh writes WAKE_OUT (empty sentinel) at startup, then
//     blocks on stdin, appending every line it reads to that file.
//  4. Once WAKE_OUT appears the test synthesises a board subscriber for the
//     real task's (anyRid, realTid), subscribes it to "topic/wake-smoke", and
//     calls board.Send.  The board's onDeliver hook calls emitTaskWake(realTid),
//     which resolves the live runner via the server's task store + registry and
//     sends a RunnerRequest{TaskWake} to it.
//  5. The runner handles TaskWake → session.WakeStdin(taskIDHex) → writes the
//     wakeMarker into the fake-claude pipe.  The script appends that line to
//     WAKE_OUT.
//  6. The test asserts WAKE_OUT contains "<harness:agentboard-wake>" within 2s.
//
// The synthetic RunnerID passed to board.Attach is intentionally arbitrary:
// server.wireAgentBoardWake ignores the RunnerID from the onDeliver callback
// (it is _ in the closure) and resolves the runner exclusively from the TaskID
// via the server's task store.  So the rid passed to board.Attach need not
// match the real runner's RunnerID.
func TestSubmitWakeE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E skipped in -short mode")
	}

	repo := initRepo(t)
	fake, err := filepath.Abs("../testdata/fake-claude-wake.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fake); err != nil {
		t.Fatalf("fake-claude-wake.sh not found: %v", err)
	}

	wakeOut := filepath.Join(t.TempDir(), "wake.out")
	t.Setenv("WAKE_OUT", wakeOut)

	// Pick a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// In-process board so the test can inject subscriptions directly.
	board := agentboard.New(agentboard.Config{
		RingN:      32,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 4096,
	})
	defer board.Close()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	s.SetBoard(board)

	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	// Poll until the server is ready.
	{
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
			if err == nil {
				c.Close()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			ClaudeBin:    fake,
		})
	}()
	// Give the runner time to connect and become Idle.
	time.Sleep(400 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "wake-test-prompt")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted %s", taskID)

	// Wait for fake-claude to start: it writes an empty WAKE_OUT at startup.
	if !waitForFile(t, wakeOut, 10*time.Second) {
		t.Fatalf("fake-claude-wake.sh did not start (WAKE_OUT not created within 10s)")
	}
	t.Log("fake-claude running, WAKE_OUT created")

	// --- Negative invariant: no wake without a publish --------------------
	// Give the system 200ms to settle; there should be no spurious wake.
	time.Sleep(200 * time.Millisecond)
	if data, err := os.ReadFile(wakeOut); err == nil {
		if strings.Contains(string(data), "<harness:agentboard-wake>") {
			t.Errorf("spurious wake before any publish: %q", string(data))
		}
	}

	// --- Positive path: synthesise a subscriber and publish ---------------
	//
	// Build the real TaskID from the hex string returned by cli.Submit.
	var realTid agentboard.TaskID
	rawTid, err := hex.DecodeString(taskID)
	if err != nil || len(rawTid) != 16 {
		t.Fatalf("decode task id %q: %v (len=%d)", taskID, err, len(rawTid))
	}
	copy(realTid.Id[:], rawTid)

	// Synthesise a RunnerID.  The value is arbitrary because emitTaskWake (the
	// server's onDeliver hook) ignores the RunnerID from the callback and
	// resolves the runner exclusively via the TaskID.
	var fakeRid agentboard.RunnerID
	fakeRid.SetTransport([]byte("ws"))
	fakeRid.SetIpAddr([]byte{127, 0, 0, 1})
	fakeRid.Port = 9999
	fakeRid.UniqueNumber = 0xBEEF

	// Attach the synthetic identity to create a taskState in the board.
	// board.Attach does NOT validate against the ticket registry — validation
	// is the server's agent_handler's job.  We call it directly here to inject
	// a subscriber without going through the agentboard wire protocol.
	conn := board.Attach(fakeRid, realTid, "integration-test")
	defer board.Detach(conn)
	if err := board.Subscribe(conn, "topic/wake-smoke"); err != nil {
		t.Fatalf("board.Subscribe: %v", err)
	}
	t.Log("synthetic subscriber attached and subscribed to topic/wake-smoke")

	// Build a sender identity (for Send's from_* attribution; the values are
	// only used for message provenance — they do not affect the wake path).
	var fromRid protocol.RunnerID
	fromRid.SetTransport([]byte("ws"))
	fromRid.SetIpAddr([]byte{127, 0, 0, 1})
	fromRid.Port = 9998
	fromRid.UniqueNumber = 0xCAFE
	var fromTid protocol.TaskID
	copy(fromTid.Id[:], rawTid) // use the same task as sender for simplicity

	seq, err := board.Send("topic/wake-smoke", []byte("ping"), fromRid, fromTid, "integration-test")
	if err != nil {
		t.Fatalf("board.Send: %v", err)
	}
	t.Logf("board.Send ok (seq=%d), waiting for wake marker in WAKE_OUT", seq)

	// Assert the wake marker arrives within 2s.
	const wakeMarker = "<harness:agentboard-wake>"
	deadline := time.Now().Add(2 * time.Second)
	var lastContent string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(wakeOut)
		if err == nil {
			lastContent = string(data)
			if strings.Contains(lastContent, wakeMarker) {
				t.Logf("wake marker received: %q", lastContent)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(lastContent, wakeMarker) {
		t.Errorf("wake marker %q not found in WAKE_OUT within 2s; content: %q", wakeMarker, lastContent)
	}

	// Tear down.
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel — leaking goroutine")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel — leaking goroutine")
	}
}
