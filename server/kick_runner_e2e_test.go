package server

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func kickVia(t *testing.T, s *Server, identity protocol.RunnerID) protocol.RunnerKickStatus {
	t.Helper()
	conn := keyVerbConn()
	r := protocol.RunnerKickRequest{}
	r.SetPrefix([]byte(identity.Hex()))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerKick, RequestId: 9}}
	req.SetRunnerKick(r)
	s.taskHandler.Handle(conn, encodeTaskControlRequest(t, req))
	var st protocol.RunnerKickStatus
	eventually15(t, "a kick answer", func() bool {
		conn.sentMu.Lock()
		n := len(conn.sent)
		conn.sentMu.Unlock()
		if n == 0 {
			return false
		}
		resp := lastTaskControlResponse(t, conn)
		if k := resp.RunnerKick(); k != nil {
			st = k.Status
			return true
		}
		return false
	})
	return st
}

func eventually15(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// A dial-mode runner told to leave exits its PersistLoop and does not come back.
func TestKickDialModeRunnerExitsAndStaysGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, key, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: key.Public().(ed25519.PublicKey)}})
	s := New(Config{RunnerKeys: keys, Addr: "127.0.0.1:18684", DataDir: t.TempDir()})
	go func() { _ = s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	cands, _ := runner.ParseServerCandidates("ws:127.0.0.1:18684-*")
	rctx, rcancel := context.WithCancel(ctx)
	cfg := runner.Config{RunnerKey: key, RunnerID: runner.NewRunnerID(), ServerCandidates: cands,
		AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "kick-dial", Tasks: runner.NewTaskRegistry(),
		OnLeave: func(string) { rcancel() }}
	ep, err := runner.NewDialEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cli.PersistLoop(rctx,
			func(dctx context.Context) (cli.PersistHandle, error) { return runner.ConnectWith(dctx, ep, cfg) },
			func(rc context.Context, h cli.PersistHandle) error {
				return runner.OnConnect(rc, h.(*runner.RunHandle))
			},
			cli.PersistConfig{Enabled: true, InitialBackoff: 50 * time.Millisecond})
	}()
	f := &originFixture{s: s}
	e := f.waitRunner(t, "kick-dial")

	if st := kickVia(t, s, e.Identity); st != protocol.RunnerKickStatus_Left {
		t.Fatalf("kick: %v, want left", st)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PersistLoop returned %v, want nil (exit 0)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner did not leave")
	}
	time.Sleep(time.Second)
	for _, r := range s.registry.List() {
		if r.Hostname == "kick-dial" {
			t.Fatal("the kicked runner came back")
		}
	}
}

// A UDP-only listen-mode runner announces its close when it leaves, so the
// kick reads `left` rather than waiting out the ~1 min inactivity GC.
func TestKickListenUDPRunnerLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, key, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: key.Public().(ed25519.PublicKey)}})
	s := New(Config{RunnerKeys: keys, Addr: "127.0.0.1:18685", UDPAddr: "127.0.0.1:18686", DataDir: t.TempDir()})
	s.taskHandler.KickWait = 4 * time.Second
	go func() { _ = s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	rctx, rcancel := context.WithCancel(ctx)
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- runner.ListenAndServe(rctx, runner.ListenConfig{
			Config: runner.Config{RunnerKey: key, RunnerID: runner.NewRunnerID(), Tasks: runner.NewTaskRegistry(),
				AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "kick-udp",
				OnLeave: func(string) { rcancel() }},
			UDPListen: "127.0.0.1:18687",
		})
	}()
	time.Sleep(300 * time.Millisecond)
	cid, err := objproto.ParseConnectionID("udp:127.0.0.1:18687-*", objproto.ParseOption_AllowRandomID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.dialWithOrigin(ctx, cid, &DialOrigin{Link: protocol.RunnerLink_DialedOut}); err != nil {
		t.Fatal(err)
	}
	f := &originFixture{s: s}
	e := f.waitRunner(t, "kick-udp")
	if st := kickVia(t, s, e.Identity); st != protocol.RunnerKickStatus_Left {
		t.Fatalf("kick: %v, want left (the close must reach the server)", st)
	}
	select {
	case <-listenDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return")
	}
}
