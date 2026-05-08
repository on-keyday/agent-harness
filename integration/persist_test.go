//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
)

// persistRunnerHandle wraps a PersistLoop goroutine and exposes a way to
// close the current connection (triggering a reconnect) without stopping
// the loop entirely.
type persistRunnerHandle struct {
	cancel     context.CancelFunc
	done       <-chan error
	currentRun atomic.Pointer[runner.RunHandle] // set by the dialer on each iteration
}

// Close stops the PersistLoop by cancelling the context and waits for exit.
func (h *persistRunnerHandle) Close(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Log("runner persist loop did not exit within 3s")
	}
}

// DropCurrentConn closes the current connection so PersistLoop triggers a
// reconnect. Noop if no connection is active.
func (h *persistRunnerHandle) DropCurrentConn() {
	if rh := h.currentRun.Load(); rh != nil {
		rh.Close()
	}
}

// startPersistentRunnerHandle starts a runner under cli.PersistLoop in a
// goroutine. The returned handle can be used to close the current connection
// (triggering a reconnect) or to stop the loop entirely.
func startPersistentRunnerHandle(t *testing.T, serverCID objproto.ConnectionID, opts runnerOpts) *persistRunnerHandle {
	t.Helper()

	claudeBin := opts.ClaudeBin
	if claudeBin == "" {
		abs, err := filepath.Abs("../testdata/fake-claude.sh")
		if err != nil {
			t.Fatalf("resolve fake-claude.sh: %v", err)
		}
		claudeBin = abs
	}

	roots := opts.Roots
	maxTasks := opts.MaxTasks
	if maxTasks < 1 {
		maxTasks = 1
	}

	cfg := runner.Config{
		ServerCID:    serverCID,
		AllowedRoots: roots,
		MaxTasks:     maxTasks,
		Hostname:     opts.Hostname,
		ClaudeBin:    claudeBin,
		PingInterval: 2 * time.Second,
	}

	h := &persistRunnerHandle{}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	doneCh := make(chan error, 1)
	h.done = doneCh

	go func() {
		doneCh <- cli.PersistLoop(ctx,
			func(dialCtx context.Context) (cli.PersistHandle, error) {
				rh, err := runner.Connect(dialCtx, cfg)
				if err == nil {
					h.currentRun.Store(rh)
				}
				return rh, err
			},
			func(runCtx context.Context, ph cli.PersistHandle) error {
				return runner.OnConnect(runCtx, ph.(*runner.RunHandle))
			},
			cli.PersistConfig{
				Enabled:        true,
				InitialBackoff: 200 * time.Millisecond,
				MaxBackoff:     2 * time.Second,
				// Jitter=0 for deterministic backoff in tests.
				Jitter: 0,
			})
	}()

	t.Cleanup(func() { h.Close(t) })
	return h
}

// startPersistentRunner is a simpler wrapper used by tests that don't need
// direct connection access. Returns a cancel func and error channel.
func startPersistentRunner(t *testing.T, serverCID objproto.ConnectionID, opts runnerOpts) (context.CancelFunc, <-chan error) {
	t.Helper()
	h := startPersistentRunnerHandle(t, serverCID, opts)
	// unwrap: t.Cleanup already registered via startPersistentRunnerHandle.
	return h.cancel, h.done
}

// waitForRunnerWithRoot polls the server for a runner advertising root until
// it appears or timeout elapses. Returns true if the runner was found.
func waitForRunnerWithRoot(t *testing.T, serverCID objproto.ConnectionID, root string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := cli.Dial(context.Background(), serverCID)
		if err == nil {
			snap, lerr := c.Snapshot(context.Background())
			c.Close()
			if lerr == nil {
				for _, r := range snap.Runners {
					for _, ar := range r.AllowedRoots {
						if string(ar.Path) == root {
							return true
						}
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// TestRunnerPersistsAcrossServerRestart verifies that cli.PersistLoop
// automatically reconnects the runner after its connection is severed.
//
// Server restart flow:
//  1. Start server #1, runner connects and registers.
//  2. Drop the runner's current connection (simulates network disconnect /
//     server going away from the runner's perspective). Server #1 is also
//     cancelled so the port is reusable.
//  3. Start server #2 on the same port.
//  4. PersistLoop reconnects; verify runner appears on server #2.
//
// Note: we close the runner connection from the runner side (DropCurrentConn)
// rather than relying on http.Server.Shutdown to propagate the close, because
// golang.org/x/net/websocket keeps hijacked connections alive past Shutdown.
func TestRunnerPersistsAcrossServerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	t.Parallel()

	addr := freePort(t)
	repo := tempRepo(t)

	serverCID1, srv1Cancel := startServerAt(t, addr)

	rh := startPersistentRunnerHandle(t, serverCID1, runnerOpts{
		Roots:    []string{repo},
		Hostname: "persist-test",
	})

	if !waitForRunnerWithRoot(t, serverCID1, repo, 5*time.Second) {
		t.Fatalf("runner did not register on server #1")
	}

	// Simulate disconnect: close the runner's current connection.
	// This causes OnConnect to return, and PersistLoop to enter the
	// backoff-reconnect cycle.
	rh.DropCurrentConn()

	// Also cancel server #1 so the port is freed for server #2.
	srv1Cancel()
	time.Sleep(500 * time.Millisecond)

	// Start server #2 on the same port.
	serverCID2, _ := startServerAt(t, addr)

	if !waitForRunnerWithRoot(t, serverCID2, repo, 15*time.Second) {
		t.Fatalf("runner did not reconnect to server #2 within 15s")
	}
}

// TestRunnerNoPersistExitsOnDisconnect verifies that runner.Run (single-shot,
// no PersistLoop) exits and does NOT reconnect after the caller cancels the
// context. This contrasts with the PersistLoop behaviour tested in
// TestRunnerPersistsAcrossServerRestart.
//
// Note: detecting exit via server-side TCP close is unreliable because
// golang.org/x/net/websocket keeps hijacked connections alive past
// http.Server.Shutdown. Instead we cancel the runner context directly and
// verify (a) runner.Run returns quickly and (b) the runner does not re-dial
// (ensured by the single-shot contract: no reconnect loop in runner.Run).
func TestRunnerNoPersistExitsOnDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	t.Parallel()

	addr := freePort(t)
	repo := tempRepo(t)

	serverCID, _ := startServerAt(t, addr)

	claudeBin, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatalf("fake-claude path: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, runner.Config{
			ServerCID:    serverCID,
			AllowedRoots: []string{repo},
			MaxTasks:     1,
			Hostname:     "no-persist-test",
			ClaudeBin:    claudeBin,
		})
	}()

	if !waitForRunnerWithRoot(t, serverCID, repo, 5*time.Second) {
		cancel()
		t.Fatalf("runner did not register")
	}

	// Cancel the runner's context; runner.Run must return promptly.
	cancel()

	select {
	case err := <-done:
		// runner.Run returns nil on ctx cancel — acceptable; the key property
		// is that it exits without re-dialing.
		t.Logf("runner.Run exited with: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("runner.Run did not exit within 5s of context cancel")
	}

	// After cancellation, confirm the runner is deregistered from the server
	// (or at least that it does NOT re-appear under a fresh dial).
	time.Sleep(300 * time.Millisecond)
	c, cerr := cli.Dial(context.Background(), serverCID)
	if cerr != nil {
		t.Logf("dial after runner exit: %v (server may have cleaned up)", cerr)
		return
	}
	defer c.Close()
	snap, serr := c.Snapshot(context.Background())
	if serr != nil {
		t.Logf("snapshot after runner exit: %v", serr)
		return
	}
	for _, r := range snap.Runners {
		for _, ar := range r.AllowedRoots {
			if string(ar.Path) == repo {
				t.Errorf("runner still registered after context cancel — should not have reconnected")
			}
		}
	}
}
