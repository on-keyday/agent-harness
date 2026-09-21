package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// agentConn is the per-peer state for an agent_message-bearing connection.
// Set after a successful Hello.
type agentConn struct {
	state   *agentboard.ConnState
	helloed bool
}

func (s *Server) getOrCreateAgentConn(conn ConnHandle) *agentConn {
	s.agentConnsMu.Lock()
	defer s.agentConnsMu.Unlock()
	if s.agentConns == nil {
		s.agentConns = make(map[objproto.ConnectionID]*agentConn)
	}
	cid := conn.ConnectionID()
	ac, ok := s.agentConns[cid]
	if !ok {
		ac = &agentConn{}
		s.agentConns[cid] = ac
	}
	return ac
}

// removeAgentConn is called when the peer connection closes.
// NOTE: the server's handleConnection does not have a dedicated per-conn close
// hook beyond s.registry.Remove(cid) + s.scheduler.Tick(). Since agentboard
// agents only connect from the agent side (not from the runner), there is no
// existing natural point to call this beyond the end of handleConnection. The
// leak is bounded to one map entry per disconnected agent and is acceptable for
// v1 dogfood. The call site in handleConnection is a deferred cleanup added in
// server.go.
func (s *Server) removeAgentConn(cid objproto.ConnectionID) {
	s.agentConnsMu.Lock()
	defer s.agentConnsMu.Unlock()
	if s.agentConns == nil {
		return
	}
	if ac, ok := s.agentConns[cid]; ok {
		if ac.state != nil && s.Board != nil {
			s.Board.Detach(ac.state)
		}
		delete(s.agentConns, cid)
	}
}

// establishAgentIdentity validates an agent's credential (from ClientHello) and,
// on success, attaches the per-connID agentConn used by every agentboard handler
// (ac.helloed gate + ac.state.Identity()). Reuses Registry.Validate + Board.Attach
// unchanged — the single place agent identity is established, for both
// task-control ops and agentboard messaging on the same connection.
func (s *Server) establishAgentIdentity(conn ConnHandle, info *protocol.AgentInfo) protocol.ClientHelloStatus {
	if s.Board == nil {
		return agentboard.HelloStatusOk // attribution-only degrade (test wiring)
	}
	status := s.Board.Registry().Validate(info.RunnerId, info.TaskId, info.AuthTicket)
	if status == agentboard.HelloStatusOk {
		// The agent profile is authority-side data: read it from the task
		// record, never from the agent's own hello. Empty when the store has
		// no entry for this id — a defined "not attributed", not "runner
		// default" (submit/open both resolve a concrete name before Create).
		var profile string
		if s.tasks != nil {
			if e, ok := s.tasks.Get(hex.EncodeToString(info.TaskId.Id[:])); ok {
				profile = e.AgentProfile
			}
		}
		ac := s.getOrCreateAgentConn(conn)
		ac.helloed = true
		ac.state = s.Board.Attach(info.RunnerId, info.TaskId, string(info.Hostname), profile)
	}
	return status
}

// resolveReplyTarget maps a send request's (topic, in_reply_to) to the topic
// actually published to. inReplyTo == 0 passes the requested topic through. A
// non-zero inReplyTo must resolve to a message still on the board.
//
// Three arms, in this order:
//
//   - an explicit --topic on the REPLY wins. The replier is answering and may
//     know something the asker did not.
//   - the parent's reply_to_topic, which the ASKER declared when it published.
//     This is what lets a caller keep an answer out of its own inbox without
//     the replier knowing anything: a peer sends --in-reply-to alone and the
//     destination comes off the parent, server-side.
//   - the parent sender's own chat.<short-id>, which is what every reply did
//     before reply_to_topic existed and what one still does when the asker
//     declared nothing.
//
// Every arm resolves against the RETAINED entry, so the destination is the
// server's own record and not something the requester supplied.
//
// The parent is read whole (Retained) rather than as (topic, sender)
// (LookupSeq), because the destination now lives on the message. Both are full
// ring scans, so this costs nothing extra.
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool) {
	if inReplyTo == 0 {
		return topic, true
	}
	parent, ok := b.Retained(inReplyTo)
	if !ok {
		return "", false
	}
	if topic != "" {
		return topic, true
	}
	if parent.ReplyToTopic != "" {
		return parent.ReplyToTopic, true
	}
	return agentboard.SelfTopic(parent.FromTask), true
}

// retireRepliedParent applies the reply-retire rule: answering a message that
// was addressed to you withdraws it, on its author's behalf.
//
// A reply is the one moment when "this instruction is spent" is known to
// somebody who still has the context to know it. The author knows too, but the
// author has to remember — and if the author's own context is reset, nobody
// withdraws anything and the recipient re-reads the instruction forever. So the
// reply carries the retraction.
//
// Four conditions, each load-bearing:
//
//   - the parent is still live (an already-withdrawn or purged seq is nothing
//     to do, not an error);
//   - its author did not opt out with no_retire_on_reply;
//   - the parent sits on the REPLIER's own chat.<short-id>, i.e. it was
//     addressed to them specifically. A publish to a shared topic is never
//     auto-retired: one subscriber answering says nothing about whether the
//     others have read it, and retiring it there would destroy their unread
//     copy. Those senders retract explicitly instead;
//   - the replier is not the author, so a task answering itself on its own
//     topic does not erase its own message.
//
// Retraction goes through the same authorship-gated primitive an explicit
// retract uses, with the PARENT'S author as the actor — the author authorised
// it by publishing without the opt-out.
// A free function rather than a method: both frame families answer the same
// send during the migration, and a second copy of this rule is the one thing
// that must not exist — it is the difference between a peer re-reading a spent
// instruction after a context reset and not.
func retireRepliedParent(b *agentboard.Board, parentSeq uint64, replier protocol.TaskID) {
	if parentSeq == 0 || replier.Id == ([16]byte{}) || b == nil {
		return
	}
	m, ok := b.Retained(parentSeq)
	if !ok || m.NoRetireOnReply {
		return
	}
	if m.Topic != agentboard.SelfTopic(replier) || m.FromTask.Id == replier.Id {
		return
	}
	if topic, retired := b.RetractSeq(parentSeq, m.FromTask); retired {
		slog.Info("agentboard: parent retired by reply",
			"seq", parentSeq, "topic", topic,
			"author", hex.EncodeToString(m.FromTask.Id[:]),
			"replier", hex.EncodeToString(replier.Id[:]))
	}
}

// payloadReadChunk is the per-ReadDirect ceiling, and so the slack above max
// that a body can occupy before the limit is noticed.
const payloadReadChunk = 64 * 1024

// errPayloadTooLarge reports a body that exceeded the board's per-message
// limit. It is distinct from a decode/transport failure because the caller
// maps it to SendStatus_PayloadTooLarge rather than the read path's usual
// SendStatus_BadFrame — the sender can act on "too big" and cannot act on
// "bad frame".
var errPayloadTooLarge = errors.New("agent payload exceeds max")

// readAgentPayloadStream resolves the receive stream by id and reads the body,
// giving up once it exceeds max. Mirrors cli/agent/conn.go::FetchDeliveredPayload.
func readAgentPayloadStream(conn ConnHandle, id uint64, max int) ([]byte, error) {
	if id == 0 {
		return nil, fmt.Errorf("payload stream id is 0")
	}
	sid := trsf.StreamID(id)
	st := conn.GetReceiveStream(sid)
	if st == nil {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
	wait:
		for st == nil {
			select {
			case <-deadline.C:
				return nil, fmt.Errorf("payload stream %d not visible after 2s", sid)
			case <-tick.C:
				st = conn.GetReceiveStream(sid)
				if st != nil {
					break wait
				}
			}
		}
	}
	var raw []byte
	for {
		data, eof, err := st.ReadDirect(payloadReadChunk)
		if err != nil {
			return nil, fmt.Errorf("payload stream %d read: %w", sid, err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
			if len(raw) > max {
				// Cancel rather than drain: every ReadDirect returns receive
				// window to the peer, so draining an over-long body is an
				// invitation to send more. agent send is reachable with no
				// capability, which makes this the cheapest allocation
				// primitive on the server if it is left unbounded.
				st.Cancel()
				return nil, errPayloadTooLarge
			}
		}
		if eof {
			return raw, nil
		}
	}
}

// pendingPayload is a delivery whose stream id has been announced but whose
// body has not been written yet.
type pendingPayload struct {
	stream  trsf.SendStream
	payload []byte
}

// openDeliveredPayloadStream allocates a server-initiated send-stream and
// reports its id, writing nothing. Splitting allocation from the write is what
// lets the Wait/Inbox responders announce every stream id first: the agent
// cannot read a stream it has not been told about, so a body written ahead of
// the response is a body nobody is draining, and it only lands at all because
// the peer's receive window absorbs it. Past that window the write never
// completes and the message is undeliverable — a ceiling on the board's
// per-message limit that has no reason to exist.
func openDeliveredPayloadStream(conn ConnHandle) (trsf.SendStream, uint64, error) {
	stream := conn.CreateSendStream()
	if stream == nil {
		return nil, 0, fmt.Errorf("CreateSendStream returned nil")
	}
	return stream, uint64(stream.ID()), nil
}

// flushDeliveredPayloads writes each announced body + EOF. Call it only after
// the response carrying the stream ids has been sent. Runs on its own
// goroutine at the call sites: agentHandleInbox is driven straight from the
// connection's receive loop, and a peer that stops reading must not stall it.
func flushDeliveredPayloads(pending []pendingPayload) {
	for _, p := range pending {
		if werr := p.stream.AppendData(false, p.payload); werr != nil {
			slog.Warn("agent_handler: delivered payload write", "stream", p.stream.ID(), "err", werr)
			continue
		}
		if werr := p.stream.AppendData(true); werr != nil {
			slog.Warn("agent_handler: delivered payload EOF", "stream", p.stream.ID(), "err", werr)
		}
	}
}

// agentHandleReadSeq answers a request for one retained message by seq.
//
// Board.Retained searches every ring, so the subscription check below is the
// whole of this op's scoping — without it, one request per integer reads the
// entire board, and seqs are global and consecutive. It also merges "gone"
// with "not yours" into one NotFound: a distinguishable refusal would still
// answer "does seq N exist?" for every seq.

// agentHandleInboxAdvance serves the read that moves the task's delivery mark.
// Only the runner-injected UserPromptSubmit hook sends it.
//
// The body is agentHandleInbox's, minus the client-supplied cursor and the
// next_cursor in the reply: the position is the server's (taskState.shown, per
// topic), so the client neither asserts one nor is told one. Board.InboxAdvance
// collects and marks under a single acquisition of the task's lock, so nothing
// is returned here without also having been recorded as delivered.

// agentHandlePurge destroys a topic's retained-message ring. Gated by
// Capability_Purge (distinct from Prune): purge drops live retained messages on
// a possibly-shared topic, so a confined task must be granted it explicitly.

// agentHandleRetract withdraws one message the CALLER published. The withdrawn
// message leaves every agent-facing path (deliver / inbox / wait / read_seq /
// list_retained) and stays only on the operator surfaces, so a task can drop a
// spent instruction at agent speed without shrinking the window a human has to
// audit what was said.
//
// There is NO capability gate here, and that is the design rather than an
// omission. Purge needs Capability_Purge because it can destroy a topic full
// of other agents' unread messages; retract reaches exactly the bytes the
// caller wrote and nothing else, so the authority argument is authorship, not
// a grantable bit. The gate lives in Board.RetractSeq, which matches FromTask
// against the caller's authenticated task id.
//
// Both failure modes answer not_found — see RetractStatus in agentboard.bgn
// for why "not yours" must not be distinguishable.

// agentHandleListRetained returns a topic's retained ring as metadata only (no
// payload bytes). It is the content-blind targeting step for a seq-scoped
// purge: the caller picks a seq by sender / size / time without ingesting a
// payload that might itself trip a moderation gate.
//
// No capability gate (helloed only), like inbox/wait/send/subscribe. It is a
// KEYED read of a topic the caller must already name — not a discovery sweep
// (that is list_topics, which board_observe gates). Everything it surfaces (seq /
// sender task id / size / time) is already obtainable uncapped by subscribing
// and reading inbox/wait — metadata is a strict subset of that content — so a
// cap here would gate a read more tightly than the content it summarizes, for
// no gain. Destruction (purge) still needs Capability_Purge; reading does not.
