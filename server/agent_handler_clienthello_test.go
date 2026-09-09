package server

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// newTestBoard returns a minimal agentboard.Board suitable for unit tests.
func newTestBoard(t *testing.T) *agentboard.Board {
	t.Helper()
	b := agentboard.New(agentboard.Config{
		RingN:      8,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 1024,
	})
	t.Cleanup(func() { b.Close() })
	return b
}

// makeProtoRunnerID derives a stable runner identity from a connection-id
// string, so a test can build an AgentInfo.RunnerId matching whatever it
// registered on the board without going through the wire.
//
// It used to mirror runnerIDFromConnID, converting the string field by field —
// which is exactly the conflation that is gone: an identity is no longer
// derivable from an address. The hash here is a TEST convenience for keeping
// two call sites in agreement, not a rule the server follows.
func makeProtoRunnerID(t *testing.T, connIDStr string) protocol.RunnerID {
	t.Helper()
	sum := sha256.Sum256([]byte(connIDStr))
	var rid protocol.RunnerID
	copy(rid.Id[:], sum[:])
	return rid
}

// TestEstablishAgentIdentity_ValidTicket verifies that establishAgentIdentity
// returns HelloStatusOk and sets ac.helloed when the ticket is correct.
func TestEstablishAgentIdentity_ValidTicket(t *testing.T) {
	board := newTestBoard(t)

	connIDStr := "ws:127.0.0.1:8539-77"
	protoRID := makeProtoRunnerID(t, connIDStr)
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0xAB, 0xCD}
	var ticket [16]byte
	for i := range ticket {
		ticket[i] = byte(i + 1)
	}
	board.Registry().Register(protoRID, protoTID, ticket)

	s := &Server{Board: board}

	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}
	info := &protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: ticket,
	}
	info.SetHostname([]byte("testhost"))

	status := s.establishAgentIdentity(conn, info)
	if status != agentboard.HelloStatusOk {
		t.Fatalf("expected HelloStatusOk, got %v", status)
	}

	// The agentConn must be helloed after a successful establish.
	s.agentConnsMu.Lock()
	ac, ok := s.agentConns[conn.id]
	s.agentConnsMu.Unlock()
	if !ok {
		t.Fatal("expected agentConn to exist after establishAgentIdentity")
	}
	if !ac.helloed {
		t.Fatal("expected ac.helloed = true after Ok status")
	}
	if ac.state == nil {
		t.Fatal("expected ac.state to be non-nil after Ok status")
	}
}

// TestEstablishAgentIdentity_WrongTicket verifies that a wrong ticket returns
// HelloStatusBadTicket and does not set ac.helloed.
func TestEstablishAgentIdentity_WrongTicket(t *testing.T) {
	board := newTestBoard(t)

	connIDStr := "ws:127.0.0.1:8539-78"
	protoRID := makeProtoRunnerID(t, connIDStr)
	var protoTID protocol.TaskID
	protoTID.Id = [16]byte{0x11, 0x22}
	var goodTicket [16]byte
	goodTicket[0] = 0xFF
	board.Registry().Register(protoRID, protoTID, goodTicket)

	s := &Server{Board: board}

	conn := &fakeConn{id: objproto.MustParseConnectionID(connIDStr)}
	var badTicket [16]byte
	badTicket[0] = 0x00 // wrong
	info := &protocol.AgentInfo{
		RunnerId:   protoRID,
		TaskId:     protoTID,
		AuthTicket: badTicket,
	}
	info.SetHostname([]byte("testhost"))

	status := s.establishAgentIdentity(conn, info)
	if status != agentboard.HelloStatusBadTicket {
		t.Fatalf("expected HelloStatusBadTicket, got %v", status)
	}

	// The agentConn must NOT be helloed.
	s.agentConnsMu.Lock()
	ac := s.agentConns[conn.id]
	s.agentConnsMu.Unlock()
	if ac != nil && ac.helloed {
		t.Fatal("expected ac.helloed = false after bad ticket")
	}
}

// TestEstablishAgentIdentity_NilBoard verifies that nil Board degrades to Ok
// without panic (test-wiring path).
func TestEstablishAgentIdentity_NilBoard(t *testing.T) {
	s := &Server{}
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-79")}
	info := &protocol.AgentInfo{}
	info.RunnerId = makeProtoRunnerID(t, "ws:127.0.0.1:8539-79")
	info.SetHostname([]byte("x"))

	status := s.establishAgentIdentity(conn, info)
	if status != agentboard.HelloStatusOk {
		t.Fatalf("expected HelloStatusOk with nil Board, got %v", status)
	}
}

// TestClientHelloStatusFromBoard verifies the four-way mapping.
func TestClientHelloStatusFromBoard(t *testing.T) {
	cases := []struct {
		in  agentboard.HelloStatus
		out protocol.ClientHelloStatus
	}{
		{agentboard.HelloStatusOk, protocol.ClientHelloStatus_Ok},
		{agentboard.HelloStatusBadTicket, protocol.ClientHelloStatus_BadTicket},
		{agentboard.HelloStatusUnknownTask, protocol.ClientHelloStatus_UnknownTask},
		{agentboard.HelloStatusRunnerMismatch, protocol.ClientHelloStatus_RunnerMismatch},
	}
	for _, c := range cases {
		got := clientHelloStatusFromBoard(c.in)
		if got != c.out {
			t.Errorf("clientHelloStatusFromBoard(%v) = %v, want %v", c.in, got, c.out)
		}
	}
}
