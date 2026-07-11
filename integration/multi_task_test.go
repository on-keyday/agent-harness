//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// ---- helpers ----------------------------------------------------------------

// freePort asks the OS for an available TCP port on localhost.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// startServerAt starts a server on addr and returns its peer CID and a cancel function.
func startServerAt(t *testing.T, addr string) (objproto.ConnectionID, context.CancelFunc) {
	t.Helper()
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{
		Addr:    addr,
		DataDir: t.TempDir(),
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

	// Wait for the server to start listening.
	time.Sleep(300 * time.Millisecond)
	return peerCID, cancel
}

// startServer picks a free port and starts the server.
func startServer(t *testing.T) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	cid, _ := startServerAt(t, addr)
	return cid
}

// runnerHandle wraps a running runner goroutine so tests can close it.
type runnerHandle struct {
	cancel   context.CancelFunc
	done     chan error
	hostname string
	roots    []string
}

// Close cancels the runner's context and waits for it to exit.
func (r *runnerHandle) Close() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}

// Hostname returns the hostname this runner registered with.
func (r *runnerHandle) Hostname() string { return r.hostname }

// runnerOpts configures a test runner.
type runnerOpts struct {
	MaxTasks  int
	Roots     []string
	Hostname  string
	ClaudeBin string // defaults to fake-claude.sh
}

// startRunner starts a runner connected to serverCID with the given options.
func startRunner(t *testing.T, serverCID objproto.ConnectionID, opts runnerOpts) *runnerHandle {
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, runner.Config{
			ServerCID:    serverCID,
			AllowedRoots: roots,
			MaxTasks:     maxTasks,
			Hostname:     opts.Hostname,
			ClaudeBin:    claudeBin,
		})
	}()

	h := &runnerHandle{
		cancel:   cancel,
		done:     done,
		hostname: opts.Hostname,
		roots:    roots,
	}

	t.Cleanup(h.Close)

	// Give runner time to connect and send Hello.
	time.Sleep(500 * time.Millisecond)
	return h
}

// tempRepo creates a temp git repo and returns its absolute path.
func tempRepo(t *testing.T) string {
	t.Helper()
	return initRepo(t) // reuse initRepo defined in e2e_test.go
}

// dialClient opens a fresh cli.Client connected to serverCID.
// The client lives for the duration of the test; t.Cleanup closes it.
func dialClient(t *testing.T, serverCID objproto.ConnectionID) *cli.Client {
	t.Helper()
	// Use a background context so the connection lives beyond the dial call.
	// The test's Cancel (registered via t.Cleanup) will cancel the server,
	// which closes the connection. cli.Client.Close handles the teardown.
	c, err := cli.Dial(context.Background(), serverCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// mustSubmit submits a task and fails the test on error.
func mustSubmit(t *testing.T, c *cli.Client, repo, prompt string) string {
	t.Helper()
	id, err := c.Submit(context.Background(), repo, prompt)
	if err != nil {
		t.Fatalf("submit(%q): %v", prompt, err)
	}
	return id
}

// getTask returns the TaskInfo for taskID from a Snapshot, or the zero value if not found.
func getTask(t *testing.T, c *cli.Client, taskID string) protocol.TaskInfo {
	t.Helper()
	lr, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	wantRaw, _ := hex.DecodeString(taskID)
	for _, ti := range lr.Tasks {
		if string(ti.Id.Id[:]) == string(wantRaw) {
			return ti
		}
	}
	return protocol.TaskInfo{}
}

// eventually polls fn until it returns true or timeout elapses.
// It fails the test if the deadline is exceeded.
func eventually(t *testing.T, fn func() bool, timeout time.Duration, interval time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("eventually: timed out after %v: %s", timeout, msg)
}

// waitTaskTerminal polls until the task reaches a terminal state (Succeeded, Failed, Cancelled).
func waitTaskTerminal(t *testing.T, c *cli.Client, taskID string, timeout time.Duration) {
	t.Helper()
	eventually(t, func() bool {
		ti := getTask(t, c, taskID)
		switch ti.Status {
		case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
			return true
		}
		return false
	}, timeout, 200*time.Millisecond, fmt.Sprintf("task %s to reach terminal state", taskID[:12]))
}

// fakeClaudePath returns the absolute path to fake-claude.sh.
func fakeClaudePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude.sh: %v", err)
	}
	return abs
}

// fakeClaudeSlowPath returns the absolute path to fake-claude-slow.sh.
func fakeClaudeSlowPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude-slow.sh: %v", err)
	}
	return abs
}

// ---- Task 11.2: Capacity queuing then auto-dispatch -------------------------

func TestIntegrationCapacityQueueing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	// The slow runner holds id1 while id2 stays Queued.
	slowBin := fakeClaudeSlowPath(t)
	slowRunner := startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: slowBin,
	})

	c := dialClient(t, serverCID)

	// id1 will block the slow runner (fake-claude-slow sleeps 60s).
	id1 := mustSubmit(t, c, repo, "blocking task")
	// id2 should be queued while id1 holds the slot.
	id2 := mustSubmit(t, c, repo, "queued task")

	// Wait for id1 to be Running and id2 to be Queued.
	eventually(t, func() bool {
		t1 := getTask(t, c, id1)
		t2 := getTask(t, c, id2)
		return t1.Status == protocol.TaskStatus_Running && t2.Status == protocol.TaskStatus_Queued
	}, 10*time.Second, 100*time.Millisecond, "id1 Running and id2 Queued")

	// Close the slow runner (simulates it going away). This disconnects it:
	// - id1 gets marked Failed by the disconnect handler
	// - id2 remains Queued (it was never assigned to the slow runner)
	// Then start a fast runner so id2 auto-dispatches immediately.
	slowRunner.Close()

	waitTaskTerminal(t, c, id1, 5*time.Second)

	// Start a fast runner so the scheduler can dispatch id2.
	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudePath(t),
	})

	// id2 should now auto-dispatch to the fast runner and Succeed.
	waitTaskTerminal(t, c, id2, 30*time.Second)

	t2 := getTask(t, c, id2)
	if t2.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("id2 should auto-dispatch and succeed; got %v", t2.Status)
	}
}

// ---- Task 11.3: Ambiguous runner / pin by hostname / pin not found ----------

func TestIntegrationAmbiguousRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "h1"})
	startRunner(t, serverCID, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "h2"})

	c := dialClient(t, serverCID)

	// Both runners serve the same repo. With Any selector, submit should return ambiguous_runner.
	_, err := c.SubmitWithSelector(context.Background(), repo, "echo test",
		protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any})
	if err == nil {
		t.Fatal("expected ambiguous_runner error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous_runner") {
		t.Fatalf("expected 'ambiguous_runner' in error, got: %v", err)
	}
}

func TestIntegrationPinByHostnameSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "gmkhost"})
	startRunner(t, serverCID, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "raspi"})

	c := dialClient(t, serverCID)

	// Build a ByHostname selector for "raspi".
	sel, err := cli.BuildSelector(cli.SelectorOpts{Host: "raspi"})
	if err != nil {
		t.Fatalf("build selector: %v", err)
	}

	id, err := c.SubmitWithSelector(context.Background(), repo, "echo pinned", sel)
	if err != nil {
		t.Fatalf("submit with hostname pin: %v", err)
	}

	waitTaskTerminal(t, c, id, 30*time.Second)

	ti := getTask(t, c, id)
	if ti.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("pinned task should have Succeeded; got %v", ti.Status)
	}
}

func TestIntegrationPinNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	startRunner(t, serverCID, runnerOpts{MaxTasks: 1, Roots: []string{repo}, Hostname: "gmkhost"})

	c := dialClient(t, serverCID)

	sel, err := cli.BuildSelector(cli.SelectorOpts{Host: "nowhere"})
	if err != nil {
		t.Fatalf("build selector: %v", err)
	}

	_, err = c.SubmitWithSelector(context.Background(), repo, "echo pinned", sel)
	if err == nil {
		t.Fatal("expected pinned_not_found error, got nil")
	}
	if !strings.Contains(err.Error(), "pinned_not_found") {
		t.Fatalf("expected 'pinned_not_found' in error, got: %v", err)
	}
}

// ---- Task 11.4: Cancel mid-execution kills claude ---------------------------

func TestIntegrationCancelKillsClaude(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	slowBin := fakeClaudeSlowPath(t)
	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  1,
		Roots:     []string{repo},
		ClaudeBin: slowBin,
	})

	c := dialClient(t, serverCID)

	id := mustSubmit(t, c, repo, "cancel me")

	// Wait for the task to be Running.
	eventually(t, func() bool {
		ti := getTask(t, c, id)
		return ti.Status == protocol.TaskStatus_Running
	}, 10*time.Second, 100*time.Millisecond, "task to be Running before cancel")

	// Cancel the task.
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The task should transition to Cancelled. The process receives SIGTERM
	// and has 5s WaitDelay before SIGKILL; then TaskFinished arrives at the
	// server, but Cancel's idempotency means the state stays Cancelled.
	eventually(t, func() bool {
		ti := getTask(t, c, id)
		return ti.Status == protocol.TaskStatus_Cancelled && ti.EndedAt != 0
	}, 15*time.Second, 200*time.Millisecond, "task to reach Cancelled with EndedAt set")

	// Capacity on the runner must be released: at least one runner should
	// have no active tasks once the cancellation propagates.
	eventually(t, func() bool {
		lr, err := c.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, ru := range lr.Runners {
			if int(ru.ActiveTasksLen) == 0 {
				return true
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "runner capacity to be released after cancel")
}

// ---- Task 11.6: Runner disconnect marks all stranded tasks Failed -----------

func TestIntegrationDisconnectMarksTasksFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)

	slowBin := fakeClaudeSlowPath(t)
	r := startRunner(t, serverCID, runnerOpts{
		MaxTasks:  4,
		Roots:     []string{repo},
		ClaudeBin: slowBin,
	})

	c := dialClient(t, serverCID)

	ids := []string{
		mustSubmit(t, c, repo, "sleep1"),
		mustSubmit(t, c, repo, "sleep2"),
		mustSubmit(t, c, repo, "sleep3"),
	}

	// Wait for all tasks to be Running.
	eventually(t, func() bool {
		for _, id := range ids {
			ti := getTask(t, c, id)
			if ti.Status != protocol.TaskStatus_Running {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond, "all tasks Running before disconnect")

	// Disconnect the runner abruptly.
	r.Close()

	// All tasks should transition to Failed. DiffInfo on the server-side entry
	// contains "runner_disconnected" but that field is not exposed in the wire
	// TaskInfo; we verify Status == Failed as the observable contract.
	for _, id := range ids {
		waitTaskTerminal(t, c, id, 10*time.Second)
		ti := getTask(t, c, id)
		if ti.Status != protocol.TaskStatus_Failed {
			t.Errorf("task %s: want Failed after runner disconnect, got %v", id[:12], ti.Status)
		}
	}
}

// ---- Task 11.1: Two tasks run concurrently ----------------------------------

func TestIntegrationTwoTasksConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	serverCID := startServer(t)
	repo := tempRepo(t)
	startRunner(t, serverCID, runnerOpts{MaxTasks: 2, Roots: []string{repo}})

	c := dialClient(t, serverCID)

	id1 := mustSubmit(t, c, repo, "echo one")
	id2 := mustSubmit(t, c, repo, "echo two")

	waitTaskTerminal(t, c, id1, 30*time.Second)
	waitTaskTerminal(t, c, id2, 30*time.Second)

	t1 := getTask(t, c, id1)
	t2 := getTask(t, c, id2)
	if t1.Status != protocol.TaskStatus_Succeeded || t2.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("expected both Succeeded; got %v and %v", t1.Status, t2.Status)
	}
}
