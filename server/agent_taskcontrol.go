package server

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agentboard's AGENT face, on the task-control frame family.
//
// These verbs are keyed to the caller's own subscriptions, or to a message the
// caller published, and take no capability. The two board verbs that are gated
// — board_topics and board_purge — live in board_handler.go beside the rest of
// the operator face; the agent CLI reaches them by name rather than through a
// duplicate of its own.
//
// Everything here needs the caller's BOARD identity rather than its task
// principal, and that is what boardState answers.

// boardState resolves the calling connection's agentboard identity, or nil when
// the connection never completed an agent ClientHello.
//
// Every handler in this file goes through it, so "the hook is not wired" and
// "this caller is not an agent" have ONE answer rather than ten. Both are the
// same thing to a verb: there is no subscription set to read and no authorship
// to check, which is not a capability failure and must not be reported as one.
func (h *TaskHandler) boardState(conn ConnHandle) *agentboard.ConnState {
	if h.BoardConnState == nil {
		return nil
	}
	return h.BoardConnState(conn)
}

// respondAgent encodes one task-control response of the given kind. Factored
// because every handler below ends the same way and a hand-written tail per
// verb is a tail per verb to keep in step.
func respondAgent(conn ConnHandle, resp protocol.TaskControlResponse) {
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleAgentSubscribe registers or removes one exact-match subscription for
// the calling agent. remove picks which; the two verbs differ in nothing else
// and share a response format.
//
// A caller with no board identity answers bad_pattern rather than a permission
// error: it holds no wrong capability, it simply has no subscription set for
// the pattern to join.
func (h *TaskHandler) handleAgentSubscribe(conn ConnHandle, requestID uint32, pattern string, remove bool) {
	kind := protocol.TaskControlKind_AgentSubscribe
	if remove {
		kind = protocol.TaskControlKind_AgentUnsubscribe
	}
	out := protocol.AgentSubscribeResponse{RequestId: requestID, Status: protocol.SubscribeStatus_Ok}

	st := h.boardState(conn)
	switch {
	case st == nil || h.Board == nil:
		out.Status = protocol.SubscribeStatus_BadPattern
	case remove:
		h.Board.Unsubscribe(st, pattern)
	default:
		if err := h.Board.Subscribe(st, pattern); err != nil {
			out.Status = protocol.SubscribeStatus_BadPattern
		}
	}

	// The two kinds share a response TYPE but not a union field: the setter is
	// keyed to the kind, and calling the wrong one encodes a tag that does not
	// match and panics at MustAppend. Sharing the format is a decision about
	// what the wire carries, not permission to answer under the other name.
	resp := protocol.TaskControlResponse{Kind: kind, RequestId: requestID}
	if remove {
		resp.SetAgentUnsubscribe(out)
	} else {
		resp.SetAgentSubscribe(out)
	}
	respondAgent(conn, resp)
}

// handleAgentListSubscriptions reports the patterns this agent receives on.
//
// An unknown caller gets an empty list, not a refusal: "you are subscribed to
// nothing" is the true answer for a connection with no board identity, and it
// discloses nothing about anyone else.
func (h *TaskHandler) handleAgentListSubscriptions(conn ConnHandle, requestID uint32) {
	out := protocol.AgentListSubscriptionsResponse{RequestId: requestID}
	if st := h.boardState(conn); st != nil && h.Board != nil {
		for _, p := range h.Board.ListSubscriptions(st) {
			ss := protocol.AgentSubscriptionSummary{}
			ss.SetPattern([]byte(p))
			out.Subscriptions = append(out.Subscriptions, ss)
		}
	}
	out.SubscriptionsLen = uint16(len(out.Subscriptions))

	resp := protocol.TaskControlResponse{
		Kind:      protocol.TaskControlKind_AgentListSubscriptions,
		RequestId: requestID,
	}
	resp.SetAgentListSubscriptions(out)
	respondAgent(conn, resp)
}
