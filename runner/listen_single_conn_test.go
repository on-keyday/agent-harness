package runner

import (
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

func listenTestServer(t *testing.T, ctx context.Context, addr string, key ed25519.PrivateKey) *server.Server {
	t.Helper()
	keys, err := server.LoadRunnerKeyStore("", []server.RunnerKeyRecord{{Key: key.Public().(ed25519.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Config{RunnerKeys: keys, Addr: addr, DataDir: t.TempDir()})
	go func() { _ = s.Run(ctx) }()
	return s
}

func hasRunner(s *server.Server, host string) bool {
	for _, e := range s.RegisteredRunners() {
		if e.Hostname == host {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func dialFrom(t *testing.T, ctx context.Context, serverAddr, runnerAddr string) {
	t.Helper()
	parse := func(s string) objproto.ConnectionID {
		cid, err := objproto.ParseConnectionID(s, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			t.Fatal(err)
		}
		return cid
	}
	resp, err := cli.ServerDialRunner(ctx, parse("ws:"+serverAddr+"-*"), parse("ws:"+runnerAddr+"-*"), protocol.RunnerID{})
	if err != nil || resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner from %s: %v %v", serverAddr, resp.Status, err)
	}
}

// D13 (spec 2026-11-04): a listen-mode runner keeps ONE server connection. A
// second one the server accepted replaces the first, whose teardown — which
// cancels every task in the process-wide registry — runs before anything the
// new connection assigns, never after.
func TestListenSecondConnRetiresFirstBeforeAssign(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, key, _ := ed25519.GenerateKey(nil)
	const aAddr, bAddr, runnerAddr = "127.0.0.1:18680", "127.0.0.1:18681", "127.0.0.1:18682"
	sA := listenTestServer(t, ctx, aAddr, key)
	sB := listenTestServer(t, ctx, bAddr, key)
	time.Sleep(300 * time.Millisecond)
	tasks := NewTaskRegistry()
	go func() {
		_ = ListenAndServe(ctx, ListenConfig{
			Config: Config{RunnerKey: key, RunnerID: NewRunnerID(), Tasks: tasks, ProcessCtx: ctx,
				AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "single-conn"},
			WSListen: runnerAddr,
		})
	}()
	time.Sleep(300 * time.Millisecond)

	dialFrom(t, ctx, aAddr, runnerAddr)
	waitFor(t, "registered on A", 5*time.Second, func() bool { return hasRunner(sA, "single-conn") })
	var t1Cancelled, t2Cancelled atomic.Bool
	tasks.put("11111111111111111111111111111111", &taskEntry{cancel: func() { t1Cancelled.Store(true) }})

	dialFrom(t, ctx, bAddr, runnerAddr)
	waitFor(t, "registered on B", 5*time.Second, func() bool { return hasRunner(sB, "single-conn") })
	waitFor(t, "the runner let go of A", 3*time.Second, func() bool { return !hasRunner(sA, "single-conn") })
	if !t1Cancelled.Load() {
		t.Fatal("the first connection's task survived its retirement (D2: it ends)")
	}

	// Assigned after B took over: nothing left of A may cancel it.
	tasks.put("22222222222222222222222222222222", &taskEntry{cancel: func() { t2Cancelled.Store(true) }})
	time.Sleep(500 * time.Millisecond)
	if t2Cancelled.Load() {
		t.Fatal("a task on the new connection was cancelled by the old connection's teardown")
	}
	if !hasRunner(sB, "single-conn") {
		t.Fatal("B lost the runner")
	}
}

// ListenAndServe returns only once its server connection is torn down: the
// tasks cancelled and the Close sent. Returning first lets agent-runner exit
// before either happens (spec 2026-11-04 §2, the drain).
func TestListenReturnsAfterServerConnTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, key, _ := ed25519.GenerateKey(nil)
	const sAddr, runnerAddr = "127.0.0.1:18683", "127.0.0.1:18688"
	s := listenTestServer(t, ctx, sAddr, key)
	time.Sleep(300 * time.Millisecond)
	tasks := NewTaskRegistry()
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ListenAndServe(rctx, ListenConfig{
			Config: Config{RunnerKey: key, RunnerID: NewRunnerID(), Tasks: tasks, ProcessCtx: ctx,
				AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "drain"},
			WSListen: runnerAddr,
		})
	}()
	time.Sleep(300 * time.Millisecond)
	dialFrom(t, ctx, sAddr, runnerAddr)
	waitFor(t, "registered", 5*time.Second, func() bool { return hasRunner(s, "drain") })
	var torn atomic.Bool
	tasks.put("33333333333333333333333333333333", &taskEntry{cancel: func() {
		time.Sleep(200 * time.Millisecond) // a child that takes a moment to go
		torn.Store(true)
	}})
	rcancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return")
	}
	if !torn.Load() {
		t.Fatal("ListenAndServe returned before its server connection's teardown finished")
	}
}
