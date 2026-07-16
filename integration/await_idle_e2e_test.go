package integration

import (
	"context"
	"encoding/hex"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// TestAwaitIdleE2E exercises the idle-detection feature end-to-end:
//
//  1. Open a detachable interactive session running fake-claude-slow
//     (echoes one boot line, then sleeps — i.e. produces output then goes
//     byte-quiescent, the same shape as a claude turn ending).
//  2. Layer 1: poll cli.Snapshot until the task's LastOutputAt is stamped,
//     then assert OutputIdleMs grows between two snapshots (server-clock
//     idle age, the field all three UIs derive the busy/idle badge from).
//  3. Layer 2: AwaitIdle long-poll (sink=reply) fires with status=fired
//     once the boot output has been quiescent for the threshold.
func TestAwaitIdleE2E(t *testing.T) {
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

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream, taskIDHex, err := c.OpenInteractive(ctx, repo, cli.SessionOpts{Selector: sel})
	if err != nil {
		t.Fatalf("OpenInteractive: %v", err)
	}
	// Drain client-side output so nothing backpressures.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := stream.Stdout().Read(buf); err != nil {
				return
			}
		}
	}()

	// --- Layer 1: last_output_at + output_idle_ms in the List response ----
	findTask := func() (protocol.TaskInfo, bool) {
		lr, err := c.Snapshot(ctx)
		if err != nil {
			return protocol.TaskInfo{}, false
		}
		for _, ti := range lr.Tasks {
			if hex.EncodeToString(ti.Id.Id[:]) == taskIDHex {
				return ti, true
			}
		}
		return protocol.TaskInfo{}, false
	}

	var first protocol.TaskInfo
	{
		deadline := time.Now().Add(10 * time.Second)
		for {
			ti, ok := findTask()
			if ok && ti.LastOutputAt > 0 {
				first = ti
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("LastOutputAt never stamped within 10s of opening the session")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Logf("Layer1: last_output_at=%d output_idle_ms=%d", first.LastOutputAt, first.OutputIdleMs)

	// fake-claude-slow prints its one boot line then sleeps: the idle age
	// must grow across snapshots (server-clock derivation).
	time.Sleep(1 * time.Second)
	second, ok := findTask()
	if !ok {
		t.Fatal("task vanished from snapshot")
	}
	if second.OutputIdleMs <= first.OutputIdleMs {
		t.Fatalf("OutputIdleMs did not grow: first=%d second=%d", first.OutputIdleMs, second.OutputIdleMs)
	}

	// --- Layer 2: await-idle long-poll fires -------------------------------
	start := time.Now()
	resp, err := c.AwaitIdle(ctx, taskIDHex, 1000, protocol.AwaitIdleSink_Reply, "")
	if err != nil {
		t.Fatalf("AwaitIdle: %v", err)
	}
	if resp.Status != protocol.AwaitIdleStatus_Fired {
		t.Fatalf("AwaitIdle status = %v, want Fired", resp.Status)
	}
	if resp.LastOutputAt == 0 {
		t.Fatal("AwaitIdle fired with LastOutputAt=0")
	}
	t.Logf("Layer2: await-idle fired after %v", time.Since(start))

	// Unknown task id → not_found (no live mux).
	respNF, err := c.AwaitIdle(ctx, "ffffffffffffffffffffffffffffffff", 0, protocol.AwaitIdleSink_Reply, "")
	if err != nil {
		t.Fatalf("AwaitIdle(unknown): %v", err)
	}
	if respNF.Status != protocol.AwaitIdleStatus_NotFound {
		t.Fatalf("AwaitIdle(unknown) status = %v, want NotFound", respNF.Status)
	}
}
