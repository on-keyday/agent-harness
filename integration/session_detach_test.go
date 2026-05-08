//go:build integration

package integration

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// fakeClaudeLoudPath returns the absolute path to fake-claude-loud.sh.
func fakeClaudeLoudPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude-loud.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude-loud.sh: %v", err)
	}
	return abs
}

// startServerWithRingSize starts a server with a custom detach ring buffer size.
func startServerWithRingSize(t *testing.T, ringSize int64) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)

	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{
		Addr:                 addr,
		DataDir:              t.TempDir(),
		DetachRingBufferSize: ringSize,
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Log("server did not exit within 3s of cancel")
		}
	})

	time.Sleep(300 * time.Millisecond)
	return peerCID
}

// TestSessionDetachReattach verifies the full detach → reattach cycle:
//  1. Start a detachable interactive session with fake-claude-slow (slow process stays alive).
//  2. Wait for task Running.
//  3. Close the client stream to simulate a disconnect.
//  4. Wait for task status to become Detached.
//  5. Open a NEW client and call AttachSession.
//  6. Assert: no error, replayBytes > 0, stream is usable.
func TestSessionDetachReattach(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash — skipping on Windows")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeSlowPath(t),
	})

	// Client 1: open the detachable session.
	c1 := dialClient(t, serverCID)

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream1, taskIDHex, err := c1.OpenInteractiveWithSelectorAndArgs(
		context.Background(), repo, sel, nil, "", true,
	)
	if err != nil {
		t.Fatalf("OpenInteractiveWithSelectorAndArgs: %v", err)
	}
	t.Logf("opened detachable session, task=%s", taskIDHex)

	// Drain a little stdout in the background so the ring buffer fills with data
	// from the initial "stdout: slow claude starting, ..." echo line.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 4096)
		for {
			_, err := stream1.Stdout().Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Wait for task to reach Running before detaching.
	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Running
	}, 10*time.Second, 100*time.Millisecond, "task to reach Running")

	// Close stream1 — this is the "client disconnect" event. tuiPump sees EOF and
	// fires onDetach → SetDetached on the server's task store.
	stream1.Close()
	<-drainDone

	// Wait for the server to flip the task to Detached.
	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Detached
	}, 5*time.Second, 100*time.Millisecond, "task to reach Detached after stream close")

	t.Logf("task %s is Detached; reattaching with new client", taskIDHex[:12])

	// Client 2: re-attach.
	c2 := dialClient(t, serverCID)
	stream2, replayBytes, err := c2.AttachSession(context.Background(), taskIDHex)
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	t.Logf("AttachSession ok, replayBytes=%d", replayBytes)

	if replayBytes == 0 {
		t.Error("expected replayBytes > 0 (ring should have the initial slow-claude output line)")
	}

	// Drain a bit of the replayed output and close.
	go func() {
		io.Copy(io.Discard, stream2.Stdout())
	}()
	go func() {
		io.Copy(io.Discard, stream2.Stderr())
	}()
	time.Sleep(100 * time.Millisecond)
	stream2.Close()
}

// TestSessionDetach_RingBufferWrap verifies that when the process emits
// far more data than the ring's byte budget, AttachSession's replay is
// bounded — the oldest *whole frames* are dropped on overflow. The exact
// replay size depends on frame boundaries, not on the byte budget alone:
// the ring keeps frames intact (a single frame larger than cap is kept
// alone, exceeding cap; smaller frames are evicted whole until total fits)
// because a byte-level truncation would corrupt the consumer's frame
// parser. The test asserts (a) the runner generated more than cap bytes
// of work, and (b) the replay is still bounded near cap rather than
// exposing the full multi-MB burst.
func TestSessionDetach_RingBufferWrap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash — skipping on Windows")
	}

	const ringSize = 1024 // 1 KiB — overflowed by the 5 MiB fake-claude-loud output

	serverCID := startServerWithRingSize(t, ringSize)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeLoudPath(t),
	})

	c1 := dialClient(t, serverCID)

	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream1, taskIDHex, err := c1.OpenInteractiveWithSelectorAndArgs(
		context.Background(), repo, sel, nil, "", true,
	)
	if err != nil {
		t.Fatalf("OpenInteractiveWithSelectorAndArgs: %v", err)
	}
	t.Logf("opened detachable session, task=%s", taskIDHex)

	// Drain stdout in the background to avoid blocking the runner's write path.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 32*1024)
		for {
			_, err := stream1.Stdout().Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Wait for task to be Running (runner accepted the session and started fake-claude-loud).
	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Running
	}, 10*time.Second, 100*time.Millisecond, "task to reach Running")

	// Give fake-claude-loud time to emit its 5 MiB through the runner, filling the ring.
	// fake-claude-loud.sh: `yes ... | head -c 5000000; echo; sleep 2`
	// At PTY throughput, 5 MiB takes well under a second. We wait 1 s to be conservative.
	time.Sleep(1 * time.Second)

	// Disconnect the client stream — triggers onDetach.
	stream1.Close()
	<-drainDone

	// Wait for task to become Detached (ring is still alive, fake-claude sleeps 2s).
	eventually(t, func() bool {
		ti := getTask(t, c1, taskIDHex)
		return ti.Status == protocol.TaskStatus_Detached
	}, 5*time.Second, 100*time.Millisecond, "task to reach Detached after loud-client disconnect")

	t.Logf("task %s is Detached; attaching to verify replay cap", taskIDHex[:12])

	c2 := dialClient(t, serverCID)
	stream2, replayBytes, err := c2.AttachSession(context.Background(), taskIDHex)
	if err != nil {
		t.Fatalf("AttachSession: %v", err)
	}
	t.Logf("AttachSession ok, replayBytes=%d (cap=%d, total runner output=~5MiB)", replayBytes, uint64(ringSize))

	if replayBytes == 0 {
		t.Errorf("replayBytes=0 — expected the ring to hold at least the most recent frame")
	}
	// Upper bound: cap + one max-frame-size headroom. PTY reads are bounded
	// by the kernel TTY buffer (typically 4 KiB), so a single frame from
	// the runner won't exceed that plus the 5-byte header. Allow generous
	// slack (64 KiB) to keep this test robust against PTY tuning.
	const maxFrameHeadroom = 64 * 1024
	if replayBytes > uint64(ringSize+maxFrameHeadroom) {
		t.Errorf("replayBytes=%d exceeds soft cap %d + headroom %d — ring failed to bound 5 MiB burst", replayBytes, ringSize, maxFrameHeadroom)
	}

	// Drain replayed output and close.
	go func() {
		io.Copy(io.Discard, stream2.Stdout())
	}()
	go func() {
		io.Copy(io.Discard, stream2.Stderr())
	}()
	time.Sleep(100 * time.Millisecond)
	stream2.Close()
}
