package server

import (
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// newAgentTaskHandler is newBoardTestHandler plus a board identity for the
// caller, which is what every agent_* verb resolves through. Returns the
// ConnState so a test can assert against the board directly rather than
// through a second request.
func newAgentTaskHandler(t *testing.T) (*TaskHandler, *fakeConn, *agentboard.ConnState) {
	t.Helper()
	h, conn := newBoardTestHandler(t)
	var rid protocol.RunnerID
	var tid protocol.TaskID
	rid.Id[0], tid.Id[0] = 0xAA, 0xBB
	st := h.Board.Attach(rid, tid, "testhost", "bash")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }
	return h, conn, st
}

// TestAgentSubscribeOverTaskControl asserts the task-control path registers the
// subscription on the BOARD, not merely that it answers ok. The response is the
// handler agreeing with itself; the board is the thing a later publish matches
// against.
func TestAgentSubscribeOverTaskControl(t *testing.T) {
	h, conn, st := newAgentTaskHandler(t)

	h.handleAgentSubscribe(conn, 7, "chat.abc", false)

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_AgentSubscribe {
		t.Fatalf("kind = %v, want agent_subscribe", resp.Kind)
	}
	got := resp.AgentSubscribe()
	if got == nil || got.Status != protocol.SubscribeStatus_Ok {
		t.Fatalf("status = %+v, want ok", got)
	}
	if !h.Board.Subscribes(st, "chat.abc") {
		t.Error("the board does not hold the subscription the request asked for")
	}
}

// TestAgentUnsubscribeOverTaskControl covers the other half of the shared
// handler.
//
// It asserts the response is read through the UNSUBSCRIBE union field. The two
// verbs share a response TYPE and do NOT share a union field: the setter and
// the accessor are keyed to the kind, so answering under the other name
// encodes a tag that does not match (MustAppend panics) and reading under the
// other name returns nil. Both halves of that were live defects the first time
// this test ran.
func TestAgentUnsubscribeOverTaskControl(t *testing.T) {
	h, conn, st := newAgentTaskHandler(t)
	h.handleAgentSubscribe(conn, 1, "chat.abc", false)

	h.handleAgentSubscribe(conn, 2, "chat.abc", true)

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_AgentUnsubscribe {
		t.Fatalf("kind = %v, want agent_unsubscribe", resp.Kind)
	}
	if got := resp.AgentUnsubscribe(); got == nil || got.Status != protocol.SubscribeStatus_Ok {
		t.Fatalf("status = %+v, want ok", got)
	}
	if resp.AgentSubscribe() != nil {
		t.Error("the unsubscribe answer is also readable as a subscribe one; the union tag is wrong")
	}
	if h.Board.Subscribes(st, "chat.abc") {
		t.Error("the subscription survived an unsubscribe the board reported ok")
	}
}

// TestAgentSubscribeWithoutBoardIdentityIsNotAPermissionError pins the choice
// boardState exists to make. A connection that never completed an agent
// ClientHello has no subscription set; that is not a missing capability, and
// reporting one would send an operator looking for a --caps to grant.
func TestAgentSubscribeWithoutBoardIdentityIsNotAPermissionError(t *testing.T) {
	h, conn, _ := newAgentTaskHandler(t)
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return nil }

	h.handleAgentSubscribe(conn, 3, "chat.abc", false)

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatal("a non-agent connection was told it lacks a capability")
	}
	if resp.Kind != protocol.TaskControlKind_AgentSubscribe {
		t.Fatalf("kind = %v, want agent_subscribe", resp.Kind)
	}
	if got := resp.AgentSubscribe(); got == nil || got.Status != protocol.SubscribeStatus_BadPattern {
		t.Fatalf("status = %+v, want bad_pattern", got)
	}
}

func TestAgentListSubscriptionsOverTaskControl(t *testing.T) {
	h, conn, st := newAgentTaskHandler(t)
	if err := h.Board.Subscribe(st, "chat.one"); err != nil {
		t.Fatalf("seed subscribe: %v", err)
	}
	if err := h.Board.Subscribe(st, "chat.two"); err != nil {
		t.Fatalf("seed subscribe: %v", err)
	}

	h.handleAgentListSubscriptions(conn, 9)

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_AgentListSubscriptions {
		t.Fatalf("kind = %v", resp.Kind)
	}
	got := resp.AgentListSubscriptions()
	if got == nil {
		t.Fatal("no agent_list_subscriptions variant")
	}
	var body protocol.SubscriptionsBody
	conn.streamBody(t, got.StreamId, &body)
	if len(body.Subscriptions) != 2 {
		t.Fatalf("subscriptions = %+v, want 2", body.Subscriptions)
	}
	seen := map[string]bool{}
	for _, s := range body.Subscriptions {
		seen[string(s.Pattern)] = true
	}
	if !seen["chat.one"] || !seen["chat.two"] {
		t.Errorf("patterns = %v, want both seeded topics", seen)
	}
}

// A connection with no board identity is subscribed to nothing, which is the
// true answer and discloses nothing about anyone else. Asserted separately
// because an empty list and a refusal are the pair this verb must not confuse.
func TestAgentListSubscriptionsWithoutIdentityIsEmptyNotDenied(t *testing.T) {
	h, conn, _ := newAgentTaskHandler(t)
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return nil }

	h.handleAgentListSubscriptions(conn, 4)

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_AgentListSubscriptions {
		t.Fatalf("kind = %v, want agent_list_subscriptions", resp.Kind)
	}
	got := resp.AgentListSubscriptions()
	if got == nil {
		t.Fatal("no agent_list_subscriptions variant")
	}
	// An empty list still arrives as a body; stream id 0 would mean the server
	// failed, and the client reports that as an error.
	var body protocol.SubscriptionsBody
	conn.streamBody(t, got.StreamId, &body)
	if len(body.Subscriptions) != 0 {
		t.Fatalf("subscriptions = %+v, want an empty list", body.Subscriptions)
	}
}

// inbox_advance marks what it collects as delivered before the response leaves,
// so what it marks and what the body carries must be the same rows. When the
// rows rode inline, a response too large for one UDP datagram was dropped after
// the marks moved, and those messages left the automatic path for good.
func TestAgentInboxAdvanceBodyCarriesEveryMarkedRow(t *testing.T) {
	h, conn, st := newAgentTaskHandler(t)
	// Past the ~13 rows that used to fill a datagram. The test board keeps 8
	// per topic, so they are spread over five topics, which is also how an
	// inbox gets that large in practice.
	const topics, perTopic = 5, 8
	const n = topics * perTopic
	for i := 0; i < topics; i++ {
		topic := fmt.Sprintf("chat.many%d", i)
		if err := h.Board.Subscribe(st, topic); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < perTopic; j++ {
			if _, _, err := h.Board.Send(topic, []byte{byte(i*perTopic + j)}, protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0); err != nil {
				t.Fatal(err)
			}
		}
	}

	h.handleAgentInboxAdvance(conn, 1)

	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentInboxAdvance()
	if got == nil {
		t.Fatal("no agent_inbox_advance variant")
	}
	if b := resp.MustAppend([]byte{0}); len(b) > 1170 {
		t.Errorf("response is %d bytes; a task-control message must fit one ~1170-byte datagram", len(b))
	}
	var body protocol.DeliveredBody
	conn.streamBody(t, got.StreamId, &body)
	if len(body.Msgs) != n {
		t.Fatalf("body carries %d rows, want %d", len(body.Msgs), n)
	}
	// Topics are visited in map order, so check the set, not the sequence.
	seen := map[byte]bool{}
	for _, m := range body.Msgs {
		if len(m.Payload) != 1 || seen[m.Payload[0]] {
			t.Fatalf("payload %v is malformed or repeated", m.Payload)
		}
		seen[m.Payload[0]] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct payloads = %d, want %d", len(seen), n)
	}
	// And they were marked: a second advance has nothing left to give.
	h.handleAgentInboxAdvance(conn, 2)
	resp = lastTaskControlResponse(t, conn)
	var again protocol.DeliveredBody
	conn.streamBody(t, resp.AgentInboxAdvance().StreamId, &again)
	if len(again.Msgs) != 0 {
		t.Errorf("second advance returned %d rows, want 0", len(again.Msgs))
	}
}

// read_seq answers not_found with no stream: the client must not wait for one.
func TestAgentReadSeqNotFoundHasNoStream(t *testing.T) {
	h, conn, _ := newAgentTaskHandler(t)

	h.handleAgentReadSeq(conn, 1, &protocol.AgentReadSeqRequest{Seq: 12345})

	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentReadSeq()
	if got == nil || got.Status != protocol.AgentReadSeqStatus_NotFound || got.StreamId != 0 {
		t.Fatalf("read_seq = %+v, want not_found with stream 0", got)
	}
}
