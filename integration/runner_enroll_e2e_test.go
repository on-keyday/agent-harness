//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

func startServerWithRunnerKeys(t *testing.T, keys *server.RunnerKeyStore) (objproto.ConnectionID, *server.Server) {
	t.Helper()
	addr := freePort(t)
	cid, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(server.Config{Addr: addr, DataDir: t.TempDir(), RunnerKeys: keys})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return cid, srv
}

func enrollTestConfig(serverCID objproto.ConnectionID, key ed25519.PrivateKey, host string) runner.Config {
	return runner.Config{
		RunnerKey:        key,
		RunnerID:         runner.NewRunnerID(),
		ServerCandidates: runner.CandidatesOf(serverCID),
		AllowedRoots:     []string{"/tmp"},
		MaxTasks:         1,
		Hostname:         host,
		Profiles:         singleAgentProfile("/bin/true"),
	}
}

// The negative control: before runner keys, any host that reached the server
// registered as a runner.
func TestRunnerWithoutEnrolledKeyIsRefused(t *testing.T) {
	keys, _ := server.LoadRunnerKeyStore("", nil)
	cid, srv := startServerWithRunnerKeys(t, keys)
	_, key, _ := ed25519.GenerateKey(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := runner.Connect(ctx, enrollTestConfig(cid, key, "stranger"))
	if err == nil {
		h.Close()
		t.Fatal("a runner with an unenrolled key connected")
	}
	var rej *cli.PskRejectedError
	if !errors.As(err, &rej) || rej.Code != protocol.PskAuthStatus_NotEnrolled || !rej.Retryable() {
		t.Fatalf("err = %v, want a retryable not_enrolled rejection", err)
	}
	if n := len(srv.RegisteredRunners()); n != 0 {
		t.Fatalf("%d runners registered, want 0", n)
	}
}

func TestNotEnrolledRunnerConnectsAfterEnroll(t *testing.T) {
	keys, _ := server.LoadRunnerKeyStore("", nil)
	cid, srv := startServerWithRunnerKeys(t, keys)
	pub, key, _ := ed25519.GenerateKey(nil)
	cfg := enrollTestConfig(cid, key, "late")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if h, err := runner.Connect(ctx, cfg); err == nil {
		h.Close()
		t.Fatal("connected before enrollment")
	}
	if _, err := keys.Enroll(server.RunnerKeyRecord{Key: pub, Label: "late", EnrolledBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	h, err := runner.Connect(ctx, cfg) // what the persist loop's next attempt does
	if err != nil {
		t.Fatalf("after enrollment: %v", err)
	}
	defer h.Close()
	go runner.OnConnect(ctx, h) //nolint:errcheck
	e := waitForRunnerByHostname(t, srv, "late", 5*time.Second)
	if !bytes.Equal(e.Key, pub) || e.LegacyAuth {
		t.Fatalf("registry entry key=%x legacy=%v, want the enrolled key and not legacy", e.Key, e.LegacyAuth)
	}
}

func TestEnrolledRunnerRegistersWithKey(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	keys, _ := server.LoadRunnerKeyStore("", []server.RunnerKeyRecord{{Key: pub, Label: "lbl", EnrolledBy: "operator"}})
	cid, srv := startServerWithRunnerKeys(t, keys)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go runner.Run(ctx, enrollTestConfig(cid, key, "enrolled")) //nolint:errcheck
	waitForRunnerByHostname(t, srv, "enrolled", 5*time.Second)

	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Runners {
		if string(r.Hostname) != "enrolled" {
			continue
		}
		if !bytes.Equal(r.Key[:], pub) || string(r.KeyLabel) != "lbl" || r.LegacyAuth() {
			t.Fatalf("runner row key=%x label=%q legacy=%v", r.Key, r.KeyLabel, r.LegacyAuth())
		}
		return
	}
	t.Fatal("the enrolled runner is not in the snapshot")
}

// A connection that claims a live runner identity with a different key is
// refused after the gate said ok, so the refusal has to reach the runner as a
// close it notices now — a local close alone left it to the runner's GC, about
// 70s (measured by the Tasks 4–7 review). The holder keeps its registration.
func TestRefusedTakeoverIsNoticedPromptly(t *testing.T) {
	p1, k1, _ := ed25519.GenerateKey(nil)
	p2, k2, _ := ed25519.GenerateKey(nil)
	keys, _ := server.LoadRunnerKeyStore("", []server.RunnerKeyRecord{
		{Key: p1, Label: "a", EnrolledBy: "operator"}, {Key: p2, Label: "b", EnrolledBy: "operator"}})
	cid, srv := startServerWithRunnerKeys(t, keys)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := enrollTestConfig(cid, k1, "holder")
	go runner.Run(ctx, a) //nolint:errcheck
	waitForRunnerByHostname(t, srv, "holder", 5*time.Second)

	b := enrollTestConfig(cid, k2, "intruder")
	b.RunnerID = a.RunnerID
	h, err := runner.Connect(ctx, b)
	if err != nil {
		t.Fatalf("the gate refused B: %v (the refusal is the registry's, after ok)", err)
	}
	done := make(chan struct{})
	go func() { runner.OnConnect(ctx, h); close(done) }() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the refused runner was still connected after 10s")
	}
	if e := waitForRunnerByHostname(t, srv, "holder", time.Second); !bytes.Equal(e.Key, p1) {
		t.Fatalf("the holder lost its registration: %+v", e.Key)
	}
}
