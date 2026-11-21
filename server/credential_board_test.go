package server

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agentboard for credential connections (credential-tasks D10): send,
// read, wait and subscribe, under the same reach as an agent, with nothing
// ever woken, and with the board state dropped whenever the credential ends or
// changes key.

// credentialBoardServer is a Server with a board. Its TaskHandler is driven
// directly over fakeConns that RecordCredentialIdentity records, as the gate's
// dispatch records a credential connection, so the board identity is the one
// the server's own OnCredentialHello wiring attaches.
func credentialBoardServer(t *testing.T) *Server {
	t.Helper()
	s := New(Config{})
	s.SetBoard(newTestBoard(t))
	return s
}

// credentialOnBoard issues a credential holding g and records one connection
// for it.
func credentialOnBoard(t *testing.T, s *Server, g authority.Grant, port int) (string, protocol.TaskID, *fakeConn) {
	t.Helper()
	_, pub := newCredentialKey(t)
	id, tid := issueCredential(t, s.Tasks(), g, pub, time.Now().Add(time.Hour))
	conn := recordCredentialConn(t, s.taskHandler, tid, pub, port)
	conn.autoSendStreams = true
	return id, tid, conn
}

func operatorPublish(t *testing.T, s *Server, topic, body string) uint64 {
	t.Helper()
	seq, _, err := s.Board.Send(topic, []byte(body), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator))
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func credentialSend(t *testing.T, s *Server, conn *fakeConn, requestID uint32, topic string) protocol.TaskControlResponse {
	t.Helper()
	r := &protocol.AgentSendRequest{}
	r.SetTopic([]byte(topic))
	return sendThroughHandler(t, s.taskHandler, conn, requestID, r)
}

func sentOK(resp protocol.TaskControlResponse) bool {
	return resp.Kind == protocol.TaskControlKind_AgentSend && resp.AgentSend().Status == protocol.SendStatus_Ok
}

// readSeq answers agent read <seq> on conn: the message, or ok=false.
func readSeq(t *testing.T, s *Server, conn *fakeConn, requestID uint32, seq uint64) (protocol.DeliveredMessage, bool) {
	t.Helper()
	s.taskHandler.handleAgentReadSeq(conn, requestID, &protocol.AgentReadSeqRequest{RequestId: requestID, Seq: seq})
	resp := lastTaskControlResponse(t, conn)
	r := resp.AgentReadSeq()
	if resp.Kind != protocol.TaskControlKind_AgentReadSeq || r == nil {
		t.Fatalf("read_seq answered %v", resp.Kind)
	}
	if r.Status != protocol.AgentReadSeqStatus_Ok {
		return protocol.DeliveredMessage{}, false
	}
	var body protocol.DeliveredBody
	conn.streamBody(t, r.StreamId, &body)
	if len(body.Msgs) != 1 {
		t.Fatalf("read_seq returned %d messages", len(body.Msgs))
	}
	return body.Msgs[0], true
}

// boardRowOf is the board's subscriber row for tid, the state a credential's
// connections share.
func boardRowOf(s *Server, tid protocol.TaskID) (agentboard.SubscriberRow, bool) {
	for _, row := range s.Board.ListSubscribers("") {
		if row.Task == tid {
			return row, true
		}
	}
	return agentboard.SubscriberRow{}, false
}

func rowHasPattern(row agentboard.SubscriberRow, pattern string) bool {
	for _, p := range row.Patterns {
		if p.Name == pattern {
			return true
		}
	}
	return false
}

// board_peer and its scope apply to a credential unchanged: it opens the
// chat.<id> of a task in its scope and refuses another with the denial an
// agent gets.
func TestCredentialBoardSendWithinPeerScope(t *testing.T) {
	s := credentialBoardServer(t)
	a := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	b := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	_, _, conn := credentialOnBoard(t, s, authority.Grant{
		Caps:  protocol.Capability_BoardPeer,
		Scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{a}},
	}, 9400)

	if resp := credentialSend(t, s, conn, 1, agentboard.SelfTopic(taskIDFromHex(a))); !sentOK(resp) {
		t.Fatalf("send to the scoped task's chat answered %v, want ok", resp.Kind)
	}
	resp := credentialSend(t, s, conn, 2, agentboard.SelfTopic(taskIDFromHex(b)))
	if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != protocol.Capability_BoardPeer {
		t.Fatalf("send outside the board_peer scope answered %v, want the board_peer denial", resp.Kind)
	}
}

// Messages to a credential stay on the board: agent read returns one on its
// own chat.<id>, and agent wait is ended by one.
func TestCredentialBoardReadAndWait(t *testing.T) {
	s := credentialBoardServer(t)
	_, tid, conn := credentialOnBoard(t, s, authority.Grant{}, 9410)
	inbox := agentboard.SelfTopic(tid)

	seq := operatorPublish(t, s, inbox, "first")
	m, ok := readSeq(t, s, conn, 1, seq)
	if !ok || string(m.Payload) != "first" {
		t.Fatalf("agent read of a message on the credential's own chat: ok=%v payload=%q", ok, m.Payload)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w := &protocol.AgentWaitRequest{RequestId: 2, Since: seq, TimeoutMs: 3000}
		w.SetPattern([]byte(inbox))
		s.taskHandler.handleAgentWait(conn, 2, w)
	}()
	second := operatorPublish(t, s, inbox, "second")
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("agent wait did not return")
	}
	resp := lastTaskControlResponse(t, conn)
	r := resp.AgentWait()
	if resp.Kind != protocol.TaskControlKind_AgentWait || r == nil || r.TimedOut != 0 {
		t.Fatalf("agent wait answered %v (timed out: %v), want the message", resp.Kind, r != nil && r.TimedOut != 0)
	}
	var body protocol.DeliveredBody
	conn.streamBody(t, r.StreamId, &body)
	if len(body.Msgs) != 1 || body.Msgs[0].Seq != second {
		t.Fatalf("agent wait returned %d messages, want seq %d", len(body.Msgs), second)
	}
}

// A credential has no process: a publish to it is delivered (it stays on the
// board for read / wait) and wakes nothing — no runner lookup, nothing logged.
func TestCredentialBoardPublishWakesNothing(t *testing.T) {
	s := credentialBoardServer(t)
	_, tid, _ := credentialOnBoard(t, s, authority.Grant{}, 9420)

	logbuf := &syncLogBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	_, delivered, err := s.Board.Send(agentboard.SelfTopic(tid), []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator))
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("delivered_to %d, want 1: the credential holds its own chat", delivered)
	}
	if n := s.Board.Wake(agentboard.SelfTopic(tid)); n != 0 {
		t.Errorf("board wake reports %d woken for a credential's chat, want 0", n)
	}
	out := logbuf.String()
	if strings.Contains(out, "task wake") {
		t.Errorf("a wake was attempted for a credential:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Errorf("publishing to a credential logged a failure:\n%s", out)
	}
}

// D10: the board state is dropped on revoke, expiry and re-key, so a new key
// holder does not inherit the subscriptions and contacts the old one built.
func TestCredentialBoardStateDroppedOnRevokeAndRekey(t *testing.T) {
	ends := map[string]func(t *testing.T, s *Server, id string, tid protocol.TaskID){
		"revoke": func(t *testing.T, s *Server, id string, _ protocol.TaskID) {
			s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
		},
		"expiry": func(t *testing.T, s *Server, id string, _ protocol.TaskID) {
			if !s.Tasks().ExpireCredential(id, time.Now().Add(2*time.Hour)) {
				t.Fatal("ExpireCredential ended nothing")
			}
		},
		"rekey": func(t *testing.T, s *Server, _ string, tid protocol.TaskID) {
			_, newPub := newCredentialKey(t)
			// The operator re-keys: no principal on this connection id.
			r := s.taskHandler.credentialRekey("ws:127.0.0.1:9439-1",
				&protocol.CredentialRekeyRequest{TaskId: tid, Pubkey: newPub, TtlMs: uint64(time.Hour / time.Millisecond)}, time.Now())
			if r.Status != protocol.CredentialStatus_Ok {
				t.Fatalf("re-key: %v", r.Status)
			}
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			s := credentialBoardServer(t)
			id, tid, conn := credentialOnBoard(t, s, authority.Grant{}, 9430)
			s.taskHandler.handleAgentSubscribe(conn, 1, "ci.events", false)
			if resp := lastTaskControlResponse(t, conn); resp.AgentSubscribe() == nil || resp.AgentSubscribe().Status != protocol.SubscribeStatus_Ok {
				t.Fatalf("subscribe answered %v", resp.Kind)
			}
			row, ok := boardRowOf(s, tid)
			if !ok || !rowHasPattern(row, "ci.events") || !rowHasPattern(row, agentboard.SelfTopic(tid)) {
				t.Fatalf("before the %s the board holds %+v (ok=%v), want its chat and ci.events", name, row, ok)
			}
			end(t, s, id, tid)
			if row, ok := boardRowOf(s, tid); ok {
				t.Fatalf("the board state survived the %s: %+v", name, row)
			}
		})
	}
}

// The carried liveness item: the board path is under the same key-bound
// liveness gate as TaskControl (principalRecord). A connection whose key was
// rotated away — with no drop to close it — keeps nothing on the board: not
// the agent face, which needs no bit, and not the board face, which needs one.
func TestCredentialBoardRefusedAfterRotation(t *testing.T) {
	s := credentialBoardServer(t)
	id, tid, conn := credentialOnBoard(t, s, authority.Grant{Caps: protocol.Capability_BoardObserve}, 9440)
	inbox := agentboard.SelfTopic(tid)
	seq := operatorPublish(t, s, inbox, "before")

	boardRead := func(requestID uint32) protocol.TaskControlResponse {
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_BoardRead, RequestId: requestID}}
		br := protocol.BoardReadRequest{RequestId: requestID}
		br.SetTopic([]byte(inbox))
		req.SetBoardRead(br)
		s.taskHandler.Handle(conn, encodeTaskControlRequest(t, req))
		return lastTaskControlResponse(t, conn)
	}

	if resp := credentialSend(t, s, conn, 1, inbox); !sentOK(resp) {
		t.Fatalf("send before the rotation answered %v", resp.Kind)
	}
	if _, ok := readSeq(t, s, conn, 2, seq); !ok {
		t.Fatal("read before the rotation was refused")
	}
	if resp := boardRead(3); resp.Kind != protocol.TaskControlKind_BoardRead {
		t.Fatalf("board read before the rotation answered %v", resp.Kind)
	}

	_, newPub := newCredentialKey(t)
	if _, err := s.Tasks().RekeyCredential(id, newPub, time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != nil {
		t.Fatal(err)
	}

	if resp := credentialSend(t, s, conn, 4, inbox); sentOK(resp) {
		t.Error("the replaced key's connection published after the rotation")
	}
	if _, ok := readSeq(t, s, conn, 5, seq); ok {
		t.Error("the replaced key's connection read a message after the rotation")
	}
	s.taskHandler.handleAgentSubscribe(conn, 6, "ci.after", false)
	if resp := lastTaskControlResponse(t, conn); resp.AgentSubscribe() != nil && resp.AgentSubscribe().Status == protocol.SubscribeStatus_Ok {
		t.Error("the replaced key's connection subscribed after the rotation")
	}
	w := &protocol.AgentWaitRequest{RequestId: 7, TimeoutMs: 50}
	w.SetPattern([]byte(inbox))
	s.taskHandler.handleAgentWait(conn, 7, w)
	if resp := lastTaskControlResponse(t, conn); resp.AgentWait() != nil && resp.AgentWait().TimedOut == 0 {
		t.Error("the replaced key's connection was handed messages by a wait after the rotation")
	}
	if resp := boardRead(8); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Errorf("board read after the rotation answered %v, want permission_denied", resp.Kind)
	}
	if caps := s.agentCallerCaps(s.getOrCreateAgentConn(conn)); caps != protocol.Capability_None {
		t.Errorf("agentCallerCaps after the rotation = %v, want none", caps)
	}

	// The new key's connection has the board.
	newConn := recordCredentialConn(t, s.taskHandler, tid, newPub, 9441)
	newConn.autoSendStreams = true
	if resp := credentialSend(t, s, newConn, 9, inbox); !sentOK(resp) {
		t.Errorf("the new key's connection could not publish: %v", resp.Kind)
	}
}
