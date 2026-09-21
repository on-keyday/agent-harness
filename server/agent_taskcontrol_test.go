package server

import (
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
	// Attach still takes the board's OWN id copies; Send takes protocol's.
	// That split is what this migration removes in its last task.
	var rid agentboard.RunnerID
	var tid agentboard.TaskID
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
	if got == nil || got.SubscriptionsLen != 2 {
		t.Fatalf("subscriptions = %+v, want 2", got)
	}
	seen := map[string]bool{}
	for _, s := range got.Subscriptions {
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
	if got := resp.AgentListSubscriptions(); got == nil || got.SubscriptionsLen != 0 {
		t.Fatalf("subscriptions = %+v, want an empty list", got)
	}
}
