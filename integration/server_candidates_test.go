package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/server"
)

// candidateRunnerConfig is the smallest Config that can complete a hello.
func candidateRunnerConfig(t *testing.T, cands runner.ServerCandidates) runner.Config {
	t.Helper()
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	return runner.Config{
		RunnerID:         runner.NewRunnerID(),
		ServerCandidates: cands,
		AllowedRoots:     []string{initRepo(t)},
		Profiles:         singleAgentProfile(fakeClaude),
	}
}

// TestServerCandidateRotation is the falsifier for the whole feature: a runner
// whose FIRST --server-cid candidate is dead must reach the server on the
// second one.
//
// Connecting at all proves which candidate was taken — only the second address
// has a listener — and that is also what makes the winner the address agents
// get, since driveAfterConn reads HARNESS_SERVER_CID off the live connection
// (`serverCID := pc.Connection().ConnectionID()`) rather than off the config.
func TestServerCandidateRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	const liveAddr = "127.0.0.1:18860"
	// Nothing listens here. Loopback answers a closed port with RST, so this
	// candidate fails fast rather than sitting out objproto's 10s handshake
	// timeout — which is the timing the roaming case actually wants.
	const deadCID = "ws:127.0.0.1:18861-*"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := server.New(server.Config{Addr: liveAddr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	cands, err := runner.ParseServerCandidates(deadCID + ",ws:" + liveAddr + "-*")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	h, err := runner.Connect(ctx, candidateRunnerConfig(t, cands))
	if err != nil {
		t.Fatalf("connect with a dead first candidate: %v", err)
	}
	h.Close()
}

// The negative control for the test above: with no reachable candidate the
// rotation must still FAIL. Without this, a rotation that silently connected
// to something else — or a Connect that returned a nil error on exhaustion —
// would read as a pass.
func TestServerCandidateRotationExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cands, err := runner.ParseServerCandidates("ws:127.0.0.1:18861-*,ws:127.0.0.1:18862-*")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	h, err := runner.Connect(ctx, candidateRunnerConfig(t, cands))
	if err == nil {
		h.Close()
		t.Fatal("connect succeeded with every candidate dead")
	}
}
