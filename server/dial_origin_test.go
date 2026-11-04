package server

import (
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// originFixture is an in-process server with two enrolled runner keys, and a
// listen-mode runner signing with the first. Ports are distinct from the
// integration package's.
type originFixture struct {
	s          *Server
	keyA, keyB ed25519.PrivateKey
	runnerCID  objproto.ConnectionID
	serverCID  objproto.ConnectionID
}

func newOriginFixture(t *testing.T, ctx context.Context, serverPort, runnerPort string) *originFixture {
	t.Helper()
	_, ka, _ := ed25519.GenerateKey(nil)
	_, kb, _ := ed25519.GenerateKey(nil)
	keys, err := LoadRunnerKeyStore("", []RunnerKeyRecord{
		{Key: ka.Public().(ed25519.PublicKey), Label: "a"},
		{Key: kb.Public().(ed25519.PublicKey), Label: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newOriginFixtureWith(t, ctx, serverPort, runnerPort, ka, Config{RunnerKeys: keys, DataDir: t.TempDir()})
	f.keyB = kb
	return f
}

// newOriginFixtureWith runs a server with cfg (Addr filled in) and a
// listen-mode runner signing with runnerKey.
func newOriginFixtureWith(t *testing.T, ctx context.Context, serverPort, runnerPort string, runnerKey ed25519.PrivateKey, cfg Config) *originFixture {
	t.Helper()
	cfg.Addr = "127.0.0.1:" + serverPort
	s := New(cfg)
	go func() { _ = s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	go func() {
		_ = runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{RunnerKey: runnerKey, RunnerID: runner.NewRunnerID(),
				AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "origin-runner"},
			WSListen: "127.0.0.1:" + runnerPort,
		})
	}()
	time.Sleep(300 * time.Millisecond)
	parse := func(s string) objproto.ConnectionID {
		cid, err := objproto.ParseConnectionID(s, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			t.Fatal(err)
		}
		return cid
	}
	return &originFixture{s: s, keyA: runnerKey,
		runnerCID: parse("ws:127.0.0.1:" + runnerPort + "-*"),
		serverCID: parse("ws:127.0.0.1:" + serverPort + "-*")}
}

func (f *originFixture) waitRunner(t *testing.T, host string) RunnerEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range f.s.registry.List() {
			if e.Hostname == host {
				return e
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("runner %q never registered", host)
	return RunnerEntry{}
}

func TestAcceptedRunnerLinkIsDialedIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ka, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: ka.Public().(ed25519.PublicKey)}})
	s := New(Config{RunnerKeys: keys, Addr: "127.0.0.1:18660", DataDir: t.TempDir()})
	go func() { _ = s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cands, err := runner.ParseServerCandidates("ws:127.0.0.1:18660-*")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = runner.Run(ctx, runner.Config{RunnerKey: ka, RunnerID: runner.NewRunnerID(), ServerCandidates: cands,
			AllowedRoots: []string{t.TempDir()}, MaxTasks: 1, Hostname: "dialed-in-runner"})
	}()
	f := &originFixture{s: s}
	e := f.waitRunner(t, "dialed-in-runner")
	if info := toRunnerInfo(e); info.Link != protocol.RunnerLink_DialedIn {
		t.Fatalf("link = %v, want dialed_in", info.Link)
	}
}

func TestOneShotDialLinkIsDialedOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newOriginFixture(t, ctx, "18661", "18662")
	resp, err := cli.ServerDialRunner(ctx, f.serverCID, f.runnerCID, protocol.RunnerID{})
	if err != nil || resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner: %v %v", resp.Status, err)
	}
	e := f.waitRunner(t, "origin-runner")
	if info := toRunnerInfo(e); info.Link != protocol.RunnerLink_DialedOut || info.LinkRecord != 0 {
		t.Fatalf("link = %v record %d, want dialed_out 0", info.Link, info.LinkRecord)
	}
}

type originProbe struct {
	mu         sync.Mutex
	registered []protocol.RunnerID
	refused    []string
	closed     chan struct{}
}

func newOriginProbe() *originProbe { return &originProbe{closed: make(chan struct{})} }

func (p *originProbe) origin(record uint32, key ed25519.PublicKey) *DialOrigin {
	var once sync.Once
	return &DialOrigin{
		Link: protocol.RunnerLink_DialRecord, RecordID: record, ExpectKey: key,
		OnRegistered: func(id protocol.RunnerID) { p.mu.Lock(); p.registered = append(p.registered, id); p.mu.Unlock() },
		OnRefused:    func(reason string) { p.mu.Lock(); p.refused = append(p.refused, reason); p.mu.Unlock() },
		OnClosed:     func() { once.Do(func() { close(p.closed) }) },
	}
}

func TestRecordOriginRightKeyCallsOnRegistered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newOriginFixture(t, ctx, "18663", "18664")
	p := newOriginProbe()
	if err := f.s.dialWithOrigin(ctx, f.runnerCID, p.origin(42, f.keyA.Public().(ed25519.PublicKey))); err != nil {
		t.Fatalf("dial: %v", err)
	}
	e := f.waitRunner(t, "origin-runner")
	info := toRunnerInfo(e)
	if info.Link != protocol.RunnerLink_DialRecord || info.LinkRecord != 42 {
		t.Fatalf("link = %v record %d, want dial_record 42", info.Link, info.LinkRecord)
	}
	p.mu.Lock()
	reg := append([]protocol.RunnerID(nil), p.registered...)
	p.mu.Unlock()
	if len(reg) != 1 || reg[0] != e.Identity {
		t.Fatalf("OnRegistered got %v, want [%s]", reg, e.Identity.Hex())
	}
	f.s.kickConn(e.ID)
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClosed did not fire after the connection was closed")
	}
}

func TestRecordOriginWrongKeyRefusedBeforeRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newOriginFixture(t, ctx, "18665", "18666")
	// The runner is already registered over a one-shot dial.
	if resp, err := cli.ServerDialRunner(ctx, f.serverCID, f.runnerCID, protocol.RunnerID{}); err != nil || resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner: %v %v", resp.Status, err)
	}
	first := f.waitRunner(t, "origin-runner")

	// A record bound to the OTHER key dials the same runner. A fresh parse:
	// the `-*` id is drawn once per parse, and a second connection under the
	// same id is refused by objproto (the dialer re-parses every attempt).
	again, err := objproto.ParseConnectionID("ws:"+f.runnerCID.Addr.String()+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}
	p := newOriginProbe()
	if err := f.s.dialWithOrigin(ctx, again, p.origin(7, f.keyB.Public().(ed25519.PublicKey))); err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the mismatched connection was not closed")
	}
	p.mu.Lock()
	refused, registered := append([]string(nil), p.refused...), len(p.registered)
	p.mu.Unlock()
	if len(refused) != 1 || !strings.HasPrefix(refused[0], "key mismatch: ") {
		t.Fatalf("OnRefused got %q, want one \"key mismatch: …\"", refused)
	}
	if registered != 0 {
		t.Fatal("a refused connection must not count as registered")
	}
	// The registry still holds the first connection, untouched.
	list := f.s.registry.List()
	if len(list) != 1 || list[0].ID != first.ID {
		t.Fatalf("registry = %+v, want only the first connection %v", list, first.ID)
	}
}
