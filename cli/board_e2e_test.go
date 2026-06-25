//go:build !js

package cli_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/cliopts"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// operatorE2E wraps an in-process server and its Board for operator-plane tests.
type operatorE2E struct {
	board  *agentboard.Board
	cancel context.CancelFunc
}

// Board returns the agentboard.Board seeded by the test.
func (e *operatorE2E) Board() *agentboard.Board { return e.board }

// startOperatorServerE2E starts an in-process server with a Board on a free
// port. It returns (wrapper, dialableCID) where the CID is suitable for
// cli.Dial(ctx, peerCID, protocol.ClientKind_Cli). Both the server and board
// are stopped via t.Cleanup.
//
// This helper also clears the harness agent env vars (HARNESS_RUNNER_ID,
// HARNESS_TASK_ID, HARNESS_AUTH_TICKET) for the duration of the test via
// t.Setenv. Inside a harness-spawned task those vars are set, which causes
// cli.buildMergedClientHello to upgrade the connection kind to Agent instead
// of Cli — breaking the operator path this test exercises.
func startOperatorServerE2E(t *testing.T) (*operatorE2E, objproto.ConnectionID) {
	t.Helper()

	// Suppress agent-context env so buildMergedClientHello sends ClientKind_Cli.
	t.Setenv("HARNESS_RUNNER_ID", "")
	t.Setenv("HARNESS_TASK_ID", "")
	t.Setenv("HARNESS_AUTH_TICKET", "")

	// Pick a free port on loopback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startOperatorServerE2E: listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	board := agentboard.New(agentboard.Config{
		RingN:      64,
		TopicTTL:   time.Hour,
		MaxTopics:  32,
		MaxPayload: 4096,
	})

	s := server.New(server.Config{Addr: addr})
	s.SetBoard(board)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()

	// Poll until the HTTP server is ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	peerCID, err := cliopts.ResolveServerCID("ws:" + addr + "-*")
	if err != nil {
		cancel()
		board.Close()
		t.Fatalf("startOperatorServerE2E: parse CID: %v", err)
	}

	e := &operatorE2E{board: board, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		board.Close()
	})
	return e, peerCID
}

// TestClientBoard_TopicsReadPurge exercises BoardTopics, BoardRead, and
// BoardPurge over a live in-process server, asserting that payload content
// ("hello" / "world") round-trips correctly through the server-initiated
// send-stream.
func TestClientBoard_TopicsReadPurge(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t)
	srv.Board().Send("chat.x", []byte("hello"), protocol.RunnerID{}, protocol.TaskID{}, "h") //nolint:errcheck
	srv.Board().Send("chat.x", []byte("world"), protocol.RunnerID{}, protocol.TaskID{}, "h") //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	topics, err := c.BoardTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0].Name != "chat.x" || topics[0].MsgCount != 2 {
		t.Fatalf("topics = %+v", topics)
	}

	msgs, found, err := c.BoardRead(ctx, "chat.x")
	if err != nil || !found || len(msgs) != 2 {
		t.Fatalf("read = (%d msgs, found=%v, err=%v)", len(msgs), found, err)
	}
	if string(msgs[0].Payload) != "hello" || string(msgs[1].Payload) != "world" {
		t.Fatalf("payloads = %q,%q", msgs[0].Payload, msgs[1].Payload)
	}

	purged, found, err := c.BoardPurge(ctx, "chat.x", msgs[0].Seq)
	if err != nil || !found || purged != 1 {
		t.Fatalf("seq purge = (%d, found=%v, err=%v)", purged, found, err)
	}
	purged, found, _ = c.BoardPurge(ctx, "chat.x", 0)
	if !found || purged != 1 {
		t.Fatalf("whole purge = (%d, found=%v)", purged, found)
	}
}
