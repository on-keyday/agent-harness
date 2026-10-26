//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// startServerWithKey starts a server that signs with key.
func startServerWithKey(t *testing.T, key ed25519.PrivateKey) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	cid, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{RunnerKeys: testRunnerKeys(), Addr: addr, DataDir: t.TempDir(), ServerKey: key})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return cid
}

func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func clearPSKEnv(t *testing.T) {
	t.Setenv("HARNESS_PSK", "")
	t.Setenv("HARNESS_PSK_FILE", "")
	t.Setenv(cli.ServerPubKeyFileEnv, "")
}

// The transcript the server signs is the one the client verifies: a real
// handshake with the right key configured must succeed.
func TestServerAuthE2E_PinnedClientAcceptsRealServer(t *testing.T) {
	pub, priv := genKey(t)
	cid := startServerWithKey(t, priv)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("pinned client vs genuine server: %v", err)
	}
	c.Close()
}

func TestServerAuthE2E_PinnedClientRefusesImpostor(t *testing.T) {
	pub, _ := genKey(t)
	_, impostor := genKey(t)
	cid := startServerWithKey(t, impostor)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, hex.EncodeToString(pub))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err == nil {
		c.Close()
		t.Fatal("pinned client connected to an impostor")
	}
	var sae *cli.ServerAuthError
	if !errors.As(err, &sae) {
		t.Fatalf("err = %v, want *cli.ServerAuthError", err)
	}
}

func TestServerAuthE2E_UnpinnedClientConnects(t *testing.T) {
	_, priv := genKey(t)
	cid := startServerWithKey(t, priv)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("unpinned client: %v", err)
	}
	c.Close()
}

// A pinned runner that reaches an impostor never gets past the handshake —
// OnConnect, which replays a buffered AssignTask and installs the dispatcher,
// is never entered — and keeps retrying under PersistLoop, the loop
// cmd/agent-runner runs, instead of taking the fatal exit.
//
// What it does NOT assert: that the impostor's registry never lists the
// runner. A server registers a runner when ITS gate accepts, before the runner
// has checked the signature, so the row can appear for the instant until the
// runner hangs up. That is the impostor's own state, not something a task can
// reach on the runner.
func TestServerAuthE2E_PinnedRunnerRefusesImpostor(t *testing.T) {
	pub, _ := genKey(t)
	_, impostor := genKey(t)
	cid := startServerWithKey(t, impostor)
	clearPSKEnv(t)
	t.Setenv(cli.ServerPubKeyEnv, "")

	repo := initRepo(t)
	fake, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	cfg := runner.Config{RunnerKey: testRunnerKey,
		RunnerID:         runner.NewRunnerID(),
		ServerCandidates: runner.CandidatesOf(cid),
		AllowedRoots:     []string{repo},
		MaxTasks:         1,
		Profiles:         singleAgentProfile(fake),
		ServerPubKey:     pub,
	}
	var attempts, authErrs, entered atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cli.PersistLoop(ctx,
			func(dialCtx context.Context) (cli.PersistHandle, error) {
				attempts.Add(1)
				rh, err := runner.Connect(dialCtx, cfg)
				var sae *cli.ServerAuthError
				if errors.As(err, &sae) {
					authErrs.Add(1)
				}
				return rh, err
			},
			func(runCtx context.Context, ph cli.PersistHandle) error {
				entered.Add(1)
				return runner.OnConnect(runCtx, ph.(*runner.RunHandle))
			},
			cli.PersistConfig{Enabled: true, InitialBackoff: 200 * time.Millisecond, MaxBackoff: time.Second, Jitter: 0})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	// Long enough for several refused attempts at the 200ms→1s backoff.
	time.Sleep(3 * time.Second)
	if n := entered.Load(); n != 0 {
		t.Fatalf("OnConnect entered %d times against an impostor", n)
	}
	select {
	case err := <-done:
		t.Fatalf("PersistLoop exited instead of retrying: %v", err)
	default:
	}
	if a, e := attempts.Load(), authErrs.Load(); a < 2 || e < 2 {
		t.Fatalf("attempts=%d server-auth failures=%d; want each ≥ 2 (refused, then retried)", a, e)
	}
}
