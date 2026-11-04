//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// P2 of docs/superpowers/specs/2026-11-04-persistent-reverse-dial-and-runner-kick-design.md:
// a deliberate server restart keeps a LISTEN-mode runner's task. Nothing but a
// dial record brings that runner back, and the record must be dialed only
// after the new server has replayed its WAL — before it, the runner would be
// told none of its held children survive.
func TestDialRecordReadoptsHeldTaskAcrossDeliberateRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash")
	}
	serverAddr, runnerAddr := freePort(t), freePort(t)
	dataDir := t.TempDir()
	dialsPath := filepath.Join(dataDir, "runner-dials.json")
	repo := tempRepo(t)

	dials, err := server.LoadRunnerDialStore(dialsPath)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dials.Add(server.RunnerDialRecord{Candidates: []string{"ws:" + runnerAddr + "-*"},
		Key: testRunnerKey.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}

	startServer := func() (*server.Server, context.CancelFunc, chan error) {
		st, err := server.LoadRunnerDialStore(dialsPath) // what a restarted server reads
		if err != nil {
			t.Fatal(err)
		}
		s := server.New(server.Config{RunnerKeys: testRunnerKeys(), RunnerDials: st,
			Addr: serverAddr, DataDir: dataDir, HoldWindow: 30 * time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		return s, cancel, done
	}
	s1, cancel1, done1 := startServer()
	time.Sleep(300 * time.Millisecond)

	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go func() {
		_ = runner.ListenAndServe(rctx, runner.ListenConfig{
			Config: runner.Config{RunnerKey: testRunnerKey, RunnerID: runner.NewRunnerID(),
				Tasks: runner.NewTaskRegistry(), ProcessCtx: rctx,
				AllowedRoots: []string{repo}, MaxTasks: 1, Hostname: "held-listen",
				Profiles: singleAgentProfile(fakeClaudeSlowPath(t))},
			WSListen: runnerAddr,
		})
	}()
	serverCID, err := objproto.ParseConnectionID("ws:"+serverAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}
	registered := func(s *server.Server) bool {
		for _, e := range s.RegisteredRunners() {
			if e.Hostname == "held-listen" && e.Link == protocol.RunnerLink_DialRecord && e.LinkRecord == rec.ID {
				return true
			}
		}
		return false
	}
	eventually(t, func() bool { return registered(s1) }, 15*time.Second, 100*time.Millisecond, "the record connects the runner")

	c1, err := cli.Dial(context.Background(), serverCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	stream, taskID, err := c1.OpenInteractive(context.Background(), repo,
		cli.SessionOpts{Selector: protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}})
	if err != nil {
		t.Fatalf("open interactive: %v", err)
	}
	stream.Close()
	eventually(t, func() bool {
		return getTask(t, c1, taskID).Status == protocol.TaskStatus_Detached
	}, 10*time.Second, 100*time.Millisecond, "the session runs, detached")
	c1.Close()

	// Deliberate stop: hold first, then cancel — the order harness-server's
	// signal path uses.
	if held := s1.RunHoldSequence(); held != 1 {
		t.Fatalf("held %d task(s), want 1", held)
	}
	cancel1()
	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Fatal("first server did not stop")
	}

	s2, cancel2, done2 := startServer()
	defer func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(5 * time.Second):
		}
	}()
	eventually(t, func() bool { return registered(s2) }, 15*time.Second, 100*time.Millisecond,
		"the restarted server dials the record")
	c2, err := cli.Dial(context.Background(), serverCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	eventually(t, func() bool {
		return getTask(t, c2, taskID).Status == protocol.TaskStatus_Detached
	}, 10*time.Second, 100*time.Millisecond, "the held task is re-adopted, not Failed")
}
