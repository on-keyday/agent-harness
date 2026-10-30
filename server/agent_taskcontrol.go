package server

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The agentboard's AGENT face, on the task-control frame family.
//
// These verbs are keyed to the caller's own subscriptions, or to a message the
// caller published, and no request KIND needs a capability. Some requests do,
// by what they name: a publish outside the caller's base set needs board_peer,
// and a read of another task's direct messages board_observe (reach spec
// 2026-10-16), refused through the same denyTaskControl as any capability.
// The two board verbs that are gated
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
		if err := h.Board.Subscribe(st, pattern); errors.Is(err, agentboard.ErrNotPermitted) {
			// The topic carries another task's messages: the ordinary
			// capability denial, rendered by every surface the same way.
			h.denyTaskControl(conn, kind, requestID, protocol.Capability_BoardObserve)
			return
		} else if err != nil {
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

// handleAgentSend publishes one message, reading its body off the
// client-initiated stream the request names.
//
// The read runs on its own goroutine, and that is load-bearing rather than
// tidy: this is driven straight from the connection's receive loop, and a peer
// that stalls part-way through a body would otherwise stall every other
// request on that connection.
func (h *TaskHandler) handleAgentSend(conn ConnHandle, requestID uint32, r *protocol.AgentSendRequest) {
	st := h.boardState(conn)
	reply := func(status protocol.SendStatus, seq uint64, deliveredTo uint16) {
		out := protocol.AgentSendResponse{
			RequestId:   requestID,
			Status:      status,
			Seq:         seq,
			DeliveredTo: deliveredTo,
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentSend, RequestId: requestID}
		resp.SetAgentSend(out)
		respondAgent(conn, resp)
	}
	if st == nil || h.Board == nil {
		// Not an agent connection: there is no authenticated sender to stamp on
		// the message, and a publish attributed to nobody is worse than a
		// refusal. bad_frame rather than a permission error, for boardState's
		// reason.
		reply(protocol.SendStatus_BadFrame, 0, 0)
		go discardPayloadStream(conn, r.PayloadStreamId)
		return
	}

	// Captured before the goroutine: the request struct is the decoded frame
	// and must not be read from another goroutine's lifetime.
	topic := string(r.Topic)
	inReplyTo := r.InReplyTo
	replyToTopic := string(r.ReplyToTopic)
	noRetire := r.NoRetireOnReply()
	noContact := r.NoContact()
	excludeSelf := r.ExcludeSelf()
	streamID := r.PayloadStreamId

	go func() {
		payload, err := readAgentPayloadStream(conn, streamID, h.Board.MaxPayload())
		if err != nil {
			slog.Warn("agent_taskcontrol: read payload stream failed", "request_id", requestID, "err", err)
			status := protocol.SendStatus_BadFrame
			if errors.Is(err, errPayloadTooLarge) {
				status = protocol.SendStatus_PayloadTooLarge
			}
			reply(status, 0, 0)
			return
		}
		fromRid, fromTid, fromHost, fromProfile := st.Identity()
		destTopic, rst := resolveReplyTarget(h.Board, topic, inReplyTo)
		if rst != protocol.SendStatus_Ok {
			reply(rst, 0, 0)
			return
		}
		// Checked where the message will land, after the reply routing: a
		// reply to a seq the sender never received resolves to a route it has
		// no contact for (reach spec D4).
		if !h.Board.Publishable(st, destTopic) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AgentSend, requestID, protocol.Capability_BoardPeer)
			return
		}
		// A declared reply route becomes the recipient's contact (§3), and the
		// recipient may be the sender itself. So it may name only a topic the
		// sender could publish to: otherwise a task with no bits sends itself
		// a message with --reply-to <anything>, reads it, and holds that topic
		// as a contact. This keeps a contact no wider than what its asker
		// could reach.
		if replyToTopic != "" && !h.Board.Publishable(st, replyToTopic) {
			h.denyTaskControl(conn, protocol.TaskControlKind_AgentSend, requestID, protocol.Capability_BoardPeer)
			return
		}
		var sendOpts []agentboard.SendOption
		if noRetire {
			sendOpts = append(sendOpts, agentboard.NoRetireOnReply())
		}
		if noContact {
			sendOpts = append(sendOpts, agentboard.NoContact())
		}
		if excludeSelf {
			sendOpts = append(sendOpts, agentboard.WithExcludeSelf())
		}
		// Where the SENDER wants replies to this message to go. Recorded on the
		// retained entry and read back by resolveReplyTarget, so the replier
		// needs no knowledge of it.
		if replyToTopic != "" {
			sendOpts = append(sendOpts, agentboard.WithReplyTo(replyToTopic))
		}
		seq, deliveredTo, sendErr := h.Board.Send(destTopic, payload, fromRid, fromTid, fromHost, fromProfile, inReplyTo, sendOpts...)
		var status protocol.SendStatus
		switch sendErr {
		case nil:
			status = protocol.SendStatus_Ok
			// Only after the reply is safely on the board: if the publish
			// failed, the acknowledgement never happened and the parent must
			// stay where the recipient can still act on it.
			if inReplyTo != 0 {
				retireRepliedParent(h.Board, inReplyTo, fromTid)
			}
		case agentboard.ErrPayloadTooLarge:
			status = protocol.SendStatus_PayloadTooLarge
		case agentboard.ErrTooManyTopics:
			status = protocol.SendStatus_TooManyTopics
		default:
			status = protocol.SendStatus_BadFrame
		}
		// deliveredTo rides along so `ok` stops covering both "everyone got it"
		// and "nobody holds this topic". Clamped like every other u16 count on
		// this wire; zero only ever means zero subscribers, never an error.
		if deliveredTo > 65535 {
			deliveredTo = 65535
		}
		reply(status, seq, uint16(deliveredTo))
	}()
}

// deliveredBody turns retained messages into the body wait / inbox /
// inbox_advance / read_seq all return, payloads in their rows. It is the one
// place all four hand messages to a task, so it is also where their reply
// routes become the task's contacts (reach spec §3).
func deliveredBody(b *agentboard.Board, st *agentboard.ConnState, msgs []agentboard.RetainedMessage) *protocol.DeliveredBody {
	if b != nil {
		b.NoteDelivered(st, msgs)
	}
	rows := make([]protocol.DeliveredMessage, 0, len(msgs))
	for _, m := range msgs {
		dm := protocol.DeliveredMessage{
			Seq:       m.Seq,
			InReplyTo: m.InReplyTo,
			// No conversion any more: RetainedMessage already holds protocol's
			// ids, and the board's hand-copied types — which the old path
			// converted to on every row — are gone.
			FromRunnerId: m.FromRunner,
			FromTaskId:   m.FromTask,
			SenderKind:   m.SenderKind,
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetPayload(m.Payload)
		dm.SetFromHostname([]byte(m.FromHostname))
		dm.SetFromAgentProfile([]byte(m.FromAgentProfile))
		dm.SetReplyToTopic([]byte(m.ReplyToTopic))
		rows = append(rows, dm)
	}
	body := &protocol.DeliveredBody{}
	body.SetMsgs(rows)
	return body
}

// handleAgentWait long-polls one topic.
//
// Dispatched on its own goroutine by the caller, because it blocks for the
// requester's whole timeout and the connection's receive loop must stay
// responsive. The delayed-response shape is ordinary on this family —
// handleAwaitIdle answers the same way.
func (h *TaskHandler) handleAgentWait(conn ConnHandle, requestID uint32, r *protocol.AgentWaitRequest) {
	out := protocol.AgentWaitResponse{RequestId: requestID, NextCursor: r.Since}
	st := h.boardState(conn)
	respond := func(msgs []agentboard.RetainedMessage) {
		respondWithBody(conn, "wait", deliveredBody(h.Board, st, msgs), func(streamID uint64) {
			out.StreamId = streamID
			resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentWait, RequestId: requestID}
			resp.SetAgentWait(out)
			respondAgent(conn, resp)
		})
	}

	if st == nil || h.Board == nil {
		out.TimedOut = 1
		respond(nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.TimeoutMs)*time.Millisecond)
	defer cancel()
	msgs, timedOut, err := h.Board.Wait(ctx, st, string(r.Pattern), r.Since, r.InReplyTo)
	if errors.Is(err, agentboard.ErrNotPermitted) {
		h.denyTaskControl(conn, protocol.TaskControlKind_AgentWait, requestID, protocol.Capability_BoardObserve)
		return
	}

	if timedOut {
		out.TimedOut = 1
	}
	for _, m := range msgs {
		if m.Seq > out.NextCursor {
			out.NextCursor = m.Seq
		}
	}
	respond(msgs)
}

// handleAgentInbox reads every subscribed topic above the caller's cursor and
// moves NOTHING: the cursor is the caller's own, so running it twice returns
// the same batch twice and it can never take a message away from the hook.
func (h *TaskHandler) handleAgentInbox(conn ConnHandle, requestID uint32, r *protocol.AgentInboxRequest) {
	out := protocol.AgentInboxResponse{RequestId: requestID, NextCursor: r.Since}
	var msgs []agentboard.RetainedMessage
	st := h.boardState(conn)
	if st != nil && h.Board != nil {
		msgs, out.NextCursor = h.Board.Inbox(st, r.Since)
	}
	respondWithBody(conn, "inbox", deliveredBody(h.Board, st, msgs), func(streamID uint64) {
		out.StreamId = streamID
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentInbox, RequestId: requestID}
		resp.SetAgentInbox(out)
		respondAgent(conn, resp)
	})
}

// handleAgentInboxAdvance is handleAgentInbox's body minus the caller's cursor
// and minus the next_cursor in the reply: the position is the SERVER's
// (topic.shown, per task), so the client neither asserts one nor is told
// one. Board.InboxAdvance collects and marks under a single acquisition of each
// topic's lock, so nothing is returned without also having been recorded as
// delivered.
func (h *TaskHandler) handleAgentInboxAdvance(conn ConnHandle, requestID uint32) {
	out := protocol.AgentInboxAdvanceResponse{RequestId: requestID}
	var msgs []agentboard.RetainedMessage
	st := h.boardState(conn)
	if st != nil && h.Board != nil {
		msgs = h.Board.InboxAdvance(st)
	}
	respondWithBody(conn, "inbox_advance", deliveredBody(h.Board, st, msgs), func(streamID uint64) {
		out.StreamId = streamID
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentInboxAdvance, RequestId: requestID}
		resp.SetAgentInboxAdvance(out)
		respondAgent(conn, resp)
	})
}

// handleAgentReadSeq answers a request for ONE retained message by seq.
//
// Board.Retained searches every ring, so the subscription check is the whole of
// this op's scoping — without it, one request per integer reads the entire
// board, and seqs are global and consecutive. It also merges "gone" with "not
// yours" into one not_found: a distinguishable refusal would still answer "does
// seq N exist?" for every seq.
func (h *TaskHandler) handleAgentReadSeq(conn ConnHandle, requestID uint32, r *protocol.AgentReadSeqRequest) {
	out := protocol.AgentReadSeqResponse{RequestId: requestID, Status: protocol.AgentReadSeqStatus_NotFound}
	respond := func(streamID uint64) {
		out.StreamId = streamID
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentReadSeq, RequestId: requestID}
		resp.SetAgentReadSeq(out)
		respondAgent(conn, resp)
	}

	if st := h.boardState(conn); st != nil && h.Board != nil {
		if m, ok := h.Board.Retained(r.Seq); ok && h.Board.Subscribes(st, m.Topic) {
			out.Status = protocol.AgentReadSeqStatus_Ok
			respondWithBody(conn, "read_seq", deliveredBody(h.Board, st, []agentboard.RetainedMessage{m}), respond)
			return
		}
	}
	respond(0)
}

// handleAgentListRetained returns a topic's retained ring as METADATA ONLY.
//
// No capability gate: it is a KEYED read of a topic the caller must already
// name — not a discovery sweep, which is board_topics and which board_observe
// gates. What it surfaces is never more than the caller could read anyway. On
// its own and group topics that is the whole ring, as subscribe + inbox would
// return it. On a topic it cannot read (another task's chat.<id>,
// chat.operator, a tree.<id> it is not in, without board_observe) it is only
// the rows the caller published, flagged own_only — which lets a sender check
// its own message on a peer's topic and exposes nothing else (reach spec D15).
// Destruction (board_purge) still needs the bit.
func (h *TaskHandler) handleAgentListRetained(conn ConnHandle, requestID uint32, r *protocol.AgentListRetainedRequest) {
	out := protocol.AgentListRetainedResponse{RequestId: requestID, Status: protocol.BoardStatus_NotFound}
	respond := func(streamID uint64) {
		out.StreamId = streamID
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentListRetained, RequestId: requestID}
		resp.SetAgentListRetained(out)
		respondAgent(conn, resp)
	}

	if h.Board != nil {
		st := h.boardState(conn)
		ownOnly := !h.Board.Readable(st, string(r.Topic))
		// Zero for a connection with no board identity, whose "own" rows
		// would otherwise be every operator and server publish.
		_, self, _, _ := st.Identity()
		if msgs, found := h.Board.ListRetained(string(r.Topic)); found {
			out.Status = protocol.BoardStatus_Ok
			out.SetOwnOnly(ownOnly)
			metas := make([]protocol.RetainedMeta, 0, len(msgs))
			for _, m := range msgs {
				if ownOnly && (st == nil || m.FromTask != self) {
					continue
				}
				size := len(m.Payload)
				if size > 0xffffffff {
					size = 0xffffffff
				}
				meta := protocol.RetainedMeta{
					Seq:              m.Seq,
					InReplyTo:        m.InReplyTo,
					FromRunner:       m.FromRunner,
					FromTask:         m.FromTask,
					Size:             uint32(size),
					ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
					SenderKind:       m.SenderKind,
				}
				meta.SetFromHostname([]byte(m.FromHostname))
				meta.SetFromAgentProfile([]byte(m.FromAgentProfile))
				meta.SetReplyToTopic([]byte(m.ReplyToTopic))
				metas = append(metas, meta)
			}
			body := &protocol.RetainedBody{}
			body.SetMetas(metas)
			respondWithBody(conn, "list_retained", body, respond)
			return
		}
	}
	respond(0)
}

// handleAgentRetract withdraws one message the CALLER published.
//
// No capability gate, and that is the design rather than an omission: purge
// needs Capability_Purge because it can destroy a topic full of other agents'
// unread messages, while this reaches exactly the bytes the caller wrote. The
// authority is AUTHORSHIP, enforced in Board.RetractSeq, which matches the
// stored FromTask against the caller's authenticated id.
//
// Both failure modes answer not_found — see AgentRetractStatus in the schema
// for why "not yours" must not be distinguishable.
func (h *TaskHandler) handleAgentRetract(conn ConnHandle, requestID uint32, r *protocol.AgentRetractRequest) {
	out := protocol.AgentRetractResponse{RequestId: requestID, Status: protocol.AgentRetractStatus_NotFound}

	if st := h.boardState(conn); st != nil && h.Board != nil {
		_, tid, _, _ := st.Identity()
		if topic, ok := h.Board.RetractSeq(r.Seq, tid); ok {
			slog.Info("agent retract", "task_id", hex.EncodeToString(tid.Id[:]), "seq", r.Seq, "topic", topic)
			out.Status = protocol.AgentRetractStatus_Ok
		}
	}

	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentRetract, RequestId: requestID}
	resp.SetAgentRetract(out)
	respondAgent(conn, resp)
}

// handleAgentListContacts reports this agent's contacts, newest first: the
// reply routes it may publish to without board_peer. Like list_subscriptions,
// an unknown caller gets an empty list rather than a refusal.
func (h *TaskHandler) handleAgentListContacts(conn ConnHandle, requestID uint32) {
	var rows []protocol.BoardContact
	if st := h.boardState(conn); st != nil && h.Board != nil {
		rows = boardContacts(h.Board.Contacts(st))
	}
	body := &protocol.ContactsBody{}
	body.SetContacts(rows)
	respondWithBody(conn, "list_contacts", body, func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AgentListContacts, RequestId: requestID}
		resp.SetAgentListContacts(protocol.AgentListContactsResponse{RequestId: requestID, StreamId: streamID})
		respondAgent(conn, resp)
	})
}

// boardContacts is the wire form of a contact list, shared by the agent's own
// view and the operator's subscriber rows so the two cannot disagree.
func boardContacts(topics []string) []protocol.BoardContact {
	out := make([]protocol.BoardContact, 0, len(topics))
	for _, t := range topics {
		var c protocol.BoardContact
		c.SetTopic([]byte(t))
		out = append(out, c)
	}
	return out
}

// handleAgentListSubscriptions reports the patterns this agent receives on.
//
// An unknown caller gets an empty list, not a refusal: "you are subscribed to
// nothing" is the true answer for a connection with no board identity, and it
// discloses nothing about anyone else.
func (h *TaskHandler) handleAgentListSubscriptions(conn ConnHandle, requestID uint32) {
	var subs []protocol.AgentSubscriptionSummary
	if st := h.boardState(conn); st != nil && h.Board != nil {
		for _, p := range h.Board.ListSubscriptions(st) {
			ss := protocol.AgentSubscriptionSummary{}
			ss.SetPattern([]byte(p))
			subs = append(subs, ss)
		}
	}
	body := &protocol.SubscriptionsBody{}
	body.SetSubscriptions(subs)
	respondWithBody(conn, "list_subscriptions", body, func(streamID uint64) {
		resp := protocol.TaskControlResponse{
			Kind:      protocol.TaskControlKind_AgentListSubscriptions,
			RequestId: requestID,
		}
		resp.SetAgentListSubscriptions(protocol.AgentListSubscriptionsResponse{RequestId: requestID, StreamId: streamID})
		respondAgent(conn, resp)
	})
}
