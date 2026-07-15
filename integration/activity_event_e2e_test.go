package integration

import (
	"context"
	"encoding/hex"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
)

// TestActivityEventE2E exercises the event-driven busy/idle path end-to-end:
//
//  1. Subscribe tasks.status BEFORE opening the session (the same
//     subscribe-then-act ordering the TUI uses after the SubscribedMsg
//     gap-fill change).
//  2. Open a detachable session running fake-claude-slow (one boot line,
//     then byte-quiescent) and collect TaskStatusEvents for its task id.
//  3. Assert a task_activity BUSY edge arrives (last_output_at stamped,
//     output_idle_ms under the shared threshold) followed by an IDLE edge
//     (output_idle_ms at/over the threshold) — with no client polling at all.
func TestActivityEventE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fake, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

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

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
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
			Profiles:     singleAgentProfile(fake),
		})
	}()
	time.Sleep(400 * time.Millisecond)

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Subscribe FIRST, then open the session: every event for the new task
	// must arrive, boot output included.
	evStream, err := c.Peer().JoinAndGetStream(ctx, "test", topics.TasksStatus())
	if err != nil {
		t.Fatalf("join tasks.status: %v", err)
	}
	var mu sync.Mutex
	var events []protocol.TaskStatusEvent
	go func() {
		var buf []byte
		for {
			data, eof, rerr := evStream.ReadDirect(64 * 1024)
			if rerr != nil {
				return
			}
			if len(data) > 0 {
				buf = append(buf, data...)
				for {
					var ev protocol.TaskStatusEvent
					rest, derr := ev.Decode(buf)
					if derr != nil {
						break
					}
					mu.Lock()
					events = append(events, ev)
					mu.Unlock()
					buf = rest
				}
			}
			if eof {
				return
			}
		}
	}()

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream, taskIDHex, err := c.OpenInteractiveWithSelectorAndArgs(ctx, repo, sel, nil, "")
	if err != nil {
		t.Fatalf("OpenInteractive: %v", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := stream.Stdout().Read(buf); err != nil {
				return
			}
		}
	}()

	// activityEvents returns the task_activity events seen so far for our task.
	activityEvents := func() []protocol.TaskStatusEvent {
		mu.Lock()
		defer mu.Unlock()
		var out []protocol.TaskStatusEvent
		for _, ev := range events {
			if ev.Kind == protocol.StatusEventKind_TaskActivity &&
				hex.EncodeToString(ev.TaskId.Id[:]) == taskIDHex {
				out = append(out, ev)
			}
		}
		return out
	}
	waitForActivity := func(n int, within time.Duration, what string) []protocol.TaskStatusEvent {
		deadline := time.Now().Add(within)
		for {
			evs := activityEvents()
			if len(evs) >= n {
				return evs
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d task_activity events within %v, want >= %d", what, len(evs), within, n)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Busy edge: fake-claude-slow's boot line lands, watcher tick fires.
	evs := waitForActivity(1, 10*time.Second, "busy edge")
	busyEv := evs[0]
	if busyEv.LastOutputAt == 0 {
		t.Fatal("busy edge event carries last_output_at=0")
	}
	thresholdMs := uint64(protocol.ActivityBusyThreshold / time.Millisecond)
	if busyEv.OutputIdleMs >= thresholdMs {
		t.Fatalf("busy edge output_idle_ms=%d, want < %d", busyEv.OutputIdleMs, thresholdMs)
	}
	if got := cli.ActivityStr(busyEv.OutputIdleMs); got != "busy" {
		t.Fatalf("busy edge renders %q, want busy", got)
	}
	t.Logf("busy edge: last_output_at=%d idle_ms=%d", busyEv.LastOutputAt, busyEv.OutputIdleMs)

	// Idle edge: quiescence crosses the shared threshold.
	evs = waitForActivity(2, 10*time.Second, "idle edge")
	idleEv := evs[1]
	if idleEv.LastOutputAt == 0 {
		t.Fatal("idle edge event carries last_output_at=0")
	}
	if idleEv.OutputIdleMs < thresholdMs {
		t.Fatalf("idle edge output_idle_ms=%d, want >= %d", idleEv.OutputIdleMs, thresholdMs)
	}
	if got := cli.ActivityStr(idleEv.OutputIdleMs); got == "busy" {
		t.Fatalf("idle edge renders busy (idle_ms=%d)", idleEv.OutputIdleMs)
	}
	t.Logf("idle edge: last_output_at=%d idle_ms=%d", idleEv.LastOutputAt, idleEv.OutputIdleMs)
}
