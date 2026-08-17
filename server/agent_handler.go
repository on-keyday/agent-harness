package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
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

func (s *Server) handleAgentMessage(conn ConnHandle, payload []byte) {
	if s.Board == nil {
		return // agentboard not configured; ignore.
	}
	msg := &agentboard.AgentMessage{}
	if _, err := msg.Decode(payload); err != nil {
		slog.Warn("agent_message decode", "err", err)
		return
	}
	ac := s.getOrCreateAgentConn(conn)
	switch msg.Kind {
	case agentboard.AgentMessageKind_Send:
		s.agentHandleSend(conn, ac, msg.Send())
	case agentboard.AgentMessageKind_Subscribe:
		s.agentHandleSubscribe(conn, ac, msg.Subscribe())
	case agentboard.AgentMessageKind_Unsubscribe:
		s.agentHandleUnsubscribe(conn, ac, msg.Unsubscribe())
	case agentboard.AgentMessageKind_Wait:
		go s.agentHandleWait(conn, ac, msg.Wait())
	case agentboard.AgentMessageKind_Inbox:
		s.agentHandleInbox(conn, ac, msg.Inbox())
	case agentboard.AgentMessageKind_ListTopics:
		s.agentHandleListTopics(conn, ac, msg.ListTopics())
	case agentboard.AgentMessageKind_ListSubscriptions:
		s.agentHandleListSubscriptions(conn, ac, msg.ListSubscriptions())
	case agentboard.AgentMessageKind_Purge:
		s.agentHandlePurge(conn, ac, msg.Purge())
	case agentboard.AgentMessageKind_ListRetained:
		s.agentHandleListRetained(conn, ac, msg.ListRetained())
	case agentboard.AgentMessageKind_ReadSeq:
		s.agentHandleReadSeq(conn, ac, msg.ReadSeq())
	case agentboard.AgentMessageKind_Retract:
		s.agentHandleRetract(conn, ac, msg.Retract())
	}
}

func (s *Server) sendAgent(conn ConnHandle, msg *agentboard.AgentMessage) {
	data, err := msg.Append([]byte{byte(appwire.AppKind_AgentMessage)})
	if err != nil {
		slog.Warn("agent_message encode", "err", err)
		return
	}
	_, _, _ = conn.SendMessage(data)
}

// establishAgentIdentity validates an agent's credential (from ClientHello) and,
// on success, attaches the per-connID agentConn used by every agentboard handler
// (ac.helloed gate + ac.state.Identity()). Reuses Registry.Validate + Board.Attach
// unchanged — the single place agent identity is established, for both
// task-control ops and agentboard messaging on the same connection.
func (s *Server) establishAgentIdentity(conn ConnHandle, info *protocol.AgentInfo) agentboard.HelloStatus {
	if s.Board == nil {
		return agentboard.HelloStatusOk // attribution-only degrade (test wiring)
	}
	rid := boardRunnerIDFromProto(info.RunnerId)
	tid := boardTaskIDFromProto(info.TaskId)
	status := s.Board.Registry().Validate(rid, tid, info.AuthTicket)
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
		ac.state = s.Board.Attach(rid, tid, string(info.Hostname), profile)
	}
	return status
}

func clientHelloStatusFromBoard(s agentboard.HelloStatus) protocol.ClientHelloStatus {
	switch s {
	case agentboard.HelloStatusBadTicket:
		return protocol.ClientHelloStatus_BadTicket
	case agentboard.HelloStatusUnknownTask:
		return protocol.ClientHelloStatus_UnknownTask
	case agentboard.HelloStatusRunnerMismatch:
		return protocol.ClientHelloStatus_RunnerMismatch
	default:
		return protocol.ClientHelloStatus_Ok
	}
}

// resolveReplyTarget maps a send request's (topic, in_reply_to) to the topic
// actually published to. inReplyTo == 0 passes the requested topic through. A
// non-zero inReplyTo must resolve to a message still on the board; when it does
// and the request named no topic, the destination is the parent sender's own
// inbound topic — taken from the retained entry, so it is the server's attested
// sender and not something the requester supplied.
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool) {
	if inReplyTo == 0 {
		return topic, true
	}
	_, parentTid, ok := b.LookupSeq(inReplyTo)
	if !ok {
		return "", false
	}
	if topic == "" {
		return agentboard.SelfTopic(parentTid), true
	}
	return topic, true
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
func (s *Server) retireRepliedParent(parentSeq uint64, replier protocol.TaskID) {
	if parentSeq == 0 || replier.Id == ([16]byte{}) {
		return
	}
	m, ok := s.Board.Retained(parentSeq)
	if !ok || m.NoRetireOnReply {
		return
	}
	if m.Topic != agentboard.SelfTopic(replier) || m.FromTask.Id == replier.Id {
		return
	}
	if topic, retired := s.Board.RetractSeq(parentSeq, m.FromTask); retired {
		slog.Info("agentboard: parent retired by reply",
			"seq", parentSeq, "topic", topic,
			"author", hex.EncodeToString(m.FromTask.Id[:]),
			"replier", hex.EncodeToString(replier.Id[:]))
	}
}

func (s *Server) agentHandleSend(conn ConnHandle, ac *agentConn, r *agentboard.SendRequest) {
	if !ac.helloed || r == nil {
		return
	}
	// Payload arrives on a client-initiated send-stream; read it before
	// publishing. Spawn a goroutine so the receive loop stays responsive.
	go func() {
		payload, err := readAgentPayloadStream(conn, r.PayloadStreamId, s.Board.MaxPayload())
		if err != nil {
			slog.Warn("agent_handler: read payload stream failed", "request_id", r.RequestId, "err", err)
			status := agentboard.SendStatus_BadFrame
			if errors.Is(err, errPayloadTooLarge) {
				status = agentboard.SendStatus_PayloadTooLarge
			}
			resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SendResponse}
			resp.SetSendResponse(agentboard.SendResponse{RequestId: r.RequestId, Status: status})
			s.sendAgent(conn, resp)
			return
		}
		fromRid, fromTid, fromHost, fromProfile := ac.state.Identity()
		destTopic, ok := resolveReplyTarget(s.Board, string(r.Topic), r.InReplyTo)
		if !ok {
			resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SendResponse}
			resp.SetSendResponse(agentboard.SendResponse{
				RequestId: r.RequestId,
				Status:    agentboard.SendStatus_UnknownInReplyTo,
				Seq:       0,
			})
			s.sendAgent(conn, resp)
			return
		}
		var sendOpts []agentboard.SendOption
		if r.NoRetireOnReply() {
			sendOpts = append(sendOpts, agentboard.NoRetireOnReply())
		}
		seq, sendErr := s.Board.Send(destTopic, payload, fromRid, fromTid, fromHost, fromProfile, r.InReplyTo, sendOpts...)
		var status agentboard.SendStatus
		switch sendErr {
		case nil:
			status = agentboard.SendStatus_Ok
			// Only after the reply is safely on the board: if the publish
			// failed, the acknowledgement never happened and the parent must
			// stay where the recipient can still act on it.
			if r.InReplyTo != 0 {
				s.retireRepliedParent(r.InReplyTo, fromTid)
			}
		case agentboard.ErrPayloadTooLarge:
			status = agentboard.SendStatus_PayloadTooLarge
		case agentboard.ErrTooManyTopics:
			status = agentboard.SendStatus_TooManyTopics
		default:
			status = agentboard.SendStatus_BadFrame
		}
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SendResponse}
		resp.SetSendResponse(agentboard.SendResponse{RequestId: r.RequestId, Status: status, Seq: seq})
		s.sendAgent(conn, resp)
	}()
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
func (s *Server) agentHandleReadSeq(conn ConnHandle, ac *agentConn, r *agentboard.ReadSeqRequest) {
	if !ac.helloed || r == nil {
		return
	}
	notFound := func() {
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ReadSeqResponse}
		resp.SetReadSeqResponse(agentboard.ReadSeqResponse{
			RequestId: r.RequestId,
			Status:    agentboard.ReadSeqStatus_NotFound,
		})
		s.sendAgent(conn, resp)
	}

	m, ok := s.Board.Retained(r.Seq)
	if !ok || !s.Board.Subscribes(ac.state, m.Topic) {
		notFound()
		return
	}

	stream, streamID, werr := openDeliveredPayloadStream(conn)
	if werr != nil {
		slog.Warn("agent_handler: read deliver stream", "seq", m.Seq, "err", werr)
		notFound()
		return
	}
	dm := agentboard.DeliveredMessage{
		Seq:             m.Seq,
		InReplyTo:       m.InReplyTo,
		PayloadStreamId: streamID,
		FromRunnerId:    protoToAgentboardRunnerID(m),
		FromTaskId:      protoToAgentboardTaskID(m),
	}
	dm.SetTopic([]byte(m.Topic))
	dm.SetFromHostname([]byte(m.FromHostname))
	dm.SetFromAgentProfile([]byte(m.FromAgentProfile))

	rr := agentboard.ReadSeqResponse{RequestId: r.RequestId, Status: agentboard.ReadSeqStatus_Ok}
	rr.SetMsgs([]agentboard.DeliveredMessage{dm})
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ReadSeqResponse}
	resp.SetReadSeqResponse(rr)
	s.sendAgent(conn, resp)
	go flushDeliveredPayloads([]pendingPayload{{stream: stream, payload: m.Payload}})
}

func (s *Server) agentHandleSubscribe(conn ConnHandle, ac *agentConn, r *agentboard.SubscribeRequest) {
	if !ac.helloed || r == nil {
		return
	}
	err := s.Board.Subscribe(ac.state, string(r.Pattern))
	status := agentboard.SubscribeStatus_Ok
	if err != nil {
		status = agentboard.SubscribeStatus_BadPattern
	}
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SubscribeResponse}
	resp.SetSubscribeResponse(agentboard.SubscribeResponse{RequestId: r.RequestId, Status: status})
	s.sendAgent(conn, resp)
}

func (s *Server) agentHandleUnsubscribe(conn ConnHandle, ac *agentConn, r *agentboard.UnsubscribeRequest) {
	if !ac.helloed || r == nil {
		return
	}
	s.Board.Unsubscribe(ac.state, string(r.Pattern))
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SubscribeResponse}
	resp.SetSubscribeResponse(agentboard.SubscribeResponse{RequestId: r.RequestId, Status: agentboard.SubscribeStatus_Ok})
	s.sendAgent(conn, resp)
}

// protoToAgentboardRunnerID converts a protocol.RunnerID (stored in RetainedMessage)
// to agentboard.RunnerID (the type carried in DeliveredMessage). The two types are
// distinct Go types with identical field shapes. If IpAddr is empty (zero sender),
// a placeholder IPv4 {0,0,0,0} is used to satisfy the hard IpAddrLen == 4|16 assertion
// in the encoder.
func protoToAgentboardRunnerID(r agentboard.RetainedMessage) agentboard.RunnerID {
	var out agentboard.RunnerID
	out.SetTransport(r.FromRunner.Transport)
	ip := r.FromRunner.IpAddr
	if len(ip) != 4 && len(ip) != 16 {
		ip = []byte{0, 0, 0, 0}
	}
	out.SetIpAddr(ip)
	out.Port = r.FromRunner.Port
	out.UniqueNumber = r.FromRunner.UniqueNumber
	return out
}

// protoToAgentboardTaskID converts a protocol.TaskID to agentboard.TaskID.
func protoToAgentboardTaskID(r agentboard.RetainedMessage) agentboard.TaskID {
	var out agentboard.TaskID
	copy(out.Id[:], r.FromTask.Id[:])
	return out
}

func (s *Server) agentHandleWait(conn ConnHandle, ac *agentConn, r *agentboard.WaitRequest) {
	if !ac.helloed || r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.TimeoutMs)*time.Millisecond)
	defer cancel()
	msgs, timedOut, _ := s.Board.Wait(ctx, ac.state, string(r.Pattern), r.Since)
	delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
	pending := make([]pendingPayload, 0, len(msgs))
	for _, m := range msgs {
		stream, streamID, werr := openDeliveredPayloadStream(conn)
		if werr != nil {
			slog.Warn("agent_handler: wait deliver stream", "seq", m.Seq, "err", werr)
			continue
		}
		pending = append(pending, pendingPayload{stream: stream, payload: m.Payload})
		dm := agentboard.DeliveredMessage{
			Seq:             m.Seq,
			InReplyTo:       m.InReplyTo,
			PayloadStreamId: streamID,
			FromRunnerId:    protoToAgentboardRunnerID(m),
			FromTaskId:      protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetFromHostname([]byte(m.FromHostname))
		dm.SetFromAgentProfile([]byte(m.FromAgentProfile))
		delivered = append(delivered, dm)
	}
	var to uint8
	if timedOut {
		to = 1
	}
	next := r.Since
	for _, m := range msgs {
		if m.Seq > next {
			next = m.Seq
		}
	}
	wr := agentboard.WaitResponse{
		RequestId:  r.RequestId,
		TimedOut:   to,
		NextCursor: next,
	}
	wr.SetMsgs(delivered)
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_WaitResponse}
	resp.SetWaitResponse(wr)
	s.sendAgent(conn, resp)
	go flushDeliveredPayloads(pending)
}

func (s *Server) agentHandleInbox(conn ConnHandle, ac *agentConn, r *agentboard.InboxRequest) {
	if !ac.helloed || r == nil {
		return
	}
	msgs, next := s.Board.Inbox(ac.state, r.Since)
	delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
	pending := make([]pendingPayload, 0, len(msgs))
	for _, m := range msgs {
		stream, streamID, werr := openDeliveredPayloadStream(conn)
		if werr != nil {
			slog.Warn("agent_handler: inbox deliver stream", "seq", m.Seq, "err", werr)
			continue
		}
		pending = append(pending, pendingPayload{stream: stream, payload: m.Payload})
		dm := agentboard.DeliveredMessage{
			Seq:             m.Seq,
			InReplyTo:       m.InReplyTo,
			PayloadStreamId: streamID,
			FromRunnerId:    protoToAgentboardRunnerID(m),
			FromTaskId:      protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetFromHostname([]byte(m.FromHostname))
		dm.SetFromAgentProfile([]byte(m.FromAgentProfile))
		delivered = append(delivered, dm)
	}
	ir := agentboard.InboxResponse{
		RequestId:  r.RequestId,
		NextCursor: next,
	}
	ir.SetMsgs(delivered)
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_InboxResponse}
	resp.SetInboxResponse(ir)
	s.sendAgent(conn, resp)
	go flushDeliveredPayloads(pending)
}

func (s *Server) agentHandleListTopics(conn ConnHandle, ac *agentConn, req *agentboard.ListTopicsRequest) {
	if !ac.helloed || req == nil {
		return
	}

	// Gate: callers without Capability_InfoGlobal receive an empty topic list.
	// This prevents agents from enumerating all board topics (visibility scope).
	if !hasCap(s.agentCallerCaps(ac), protocol.Capability_InfoGlobal) {
		slog.Warn("agentHandleListTopics: caller lacks InfoGlobal; returning empty list",
			"task_id", func() string {
				_, tid, _, _ := ac.state.Identity()
				return hex.EncodeToString(tid.Id[:])
			}())
		out := agentboard.ListTopicsResponse{RequestId: req.RequestId}
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListTopicsResponse}
		resp.SetListTopicsResponse(out)
		s.sendAgent(conn, resp)
		return
	}

	rows := s.Board.ListTopics()

	out := agentboard.ListTopicsResponse{RequestId: req.RequestId}
	for _, r := range rows {
		ts := agentboard.TopicSummary{
			LastSeq:               r.LastSeq,
			LastPublishedAtUnixMs: uint64(r.LastPublishedAt.UnixMilli()),
		}
		ts.SetName([]byte(r.Name))
		// MsgCount: clamp to u16
		if r.MsgCount > 65535 {
			ts.MsgCount = 65535
		} else {
			ts.MsgCount = uint16(r.MsgCount)
		}
		out.Topics = append(out.Topics, ts)
	}
	out.TopicsLen = uint16(len(out.Topics))
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListTopicsResponse}
	resp.SetListTopicsResponse(out)
	s.sendAgent(conn, resp)
}

func (s *Server) agentHandleListSubscriptions(conn ConnHandle, ac *agentConn, req *agentboard.ListSubscriptionsRequest) {
	if !ac.helloed || req == nil {
		return
	}
	patterns := s.Board.ListSubscriptions(ac.state)
	out := agentboard.ListSubscriptionsResponse{RequestId: req.RequestId}
	for _, p := range patterns {
		ss := agentboard.SubscriptionSummary{}
		ss.SetPattern([]byte(p))
		out.Subscriptions = append(out.Subscriptions, ss)
	}
	out.SubscriptionsLen = uint16(len(out.Subscriptions))
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListSubscriptionsResponse}
	resp.SetListSubscriptionsResponse(out)
	s.sendAgent(conn, resp)
}

// agentHandlePurge destroys a topic's retained-message ring. Gated by
// Capability_Purge (distinct from Prune): purge drops live retained messages on
// a possibly-shared topic, so a confined task must be granted it explicitly.
func (s *Server) agentHandlePurge(conn ConnHandle, ac *agentConn, r *agentboard.PurgeRequest) {
	if !ac.helloed || r == nil {
		return
	}
	reply := func(status agentboard.PurgeStatus, purged uint16) {
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_PurgeResponse}
		resp.SetPurgeResponse(agentboard.PurgeResponse{RequestId: r.RequestId, Status: status, Purged: purged})
		s.sendAgent(conn, resp)
	}

	if !hasCap(s.agentCallerCaps(ac), protocol.Capability_Purge) {
		_, tid, _, _ := ac.state.Identity()
		slog.Warn("agentHandlePurge: caller lacks Purge cap; denying",
			"task_id", hex.EncodeToString(tid.Id[:]), "topic", string(r.Topic))
		reply(agentboard.PurgeStatus_Denied, 0)
		return
	}

	// seq == 0 → whole topic; seq > 0 → drop just that one retained message.
	if r.Seq == 0 {
		purged, found := s.Board.PurgeTopic(string(r.Topic))
		if !found {
			reply(agentboard.PurgeStatus_NotFound, 0)
			return
		}
		n := purged
		if n > 65535 {
			n = 65535
		}
		reply(agentboard.PurgeStatus_Ok, uint16(n))
		return
	}

	removed, found := s.Board.PurgeSeq(string(r.Topic), r.Seq)
	if !found || !removed {
		// Topic gone, or no retained message carried that seq.
		reply(agentboard.PurgeStatus_NotFound, 0)
		return
	}
	reply(agentboard.PurgeStatus_Ok, 1)
}

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
func (s *Server) agentHandleRetract(conn ConnHandle, ac *agentConn, r *agentboard.RetractRequest) {
	if !ac.helloed || r == nil {
		return
	}
	reply := func(status agentboard.RetractStatus) {
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_RetractResponse}
		resp.SetRetractResponse(agentboard.RetractResponse{RequestId: r.RequestId, Status: status})
		s.sendAgent(conn, resp)
	}

	_, tid, _, _ := ac.state.Identity()
	topic, ok := s.Board.RetractSeq(r.Seq, tid)
	if !ok {
		reply(agentboard.RetractStatus_NotFound)
		return
	}
	slog.Info("agent retract", "task_id", hex.EncodeToString(tid.Id[:]), "seq", r.Seq, "topic", topic)
	reply(agentboard.RetractStatus_Ok)
}

// agentHandleListRetained returns a topic's retained ring as metadata only (no
// payload bytes). It is the content-blind targeting step for a seq-scoped
// purge: the caller picks a seq by sender / size / time without ingesting a
// payload that might itself trip a moderation gate.
//
// No capability gate (helloed only), like inbox/wait/send/subscribe. It is a
// KEYED read of a topic the caller must already name — not a discovery sweep
// (that is list_topics, which info_global gates). Everything it surfaces (seq /
// sender task id / size / time) is already obtainable uncapped by subscribing
// and reading inbox/wait — metadata is a strict subset of that content — so a
// cap here would gate a read more tightly than the content it summarizes, for
// no gain. Destruction (purge) still needs Capability_Purge; reading does not.
func (s *Server) agentHandleListRetained(conn ConnHandle, ac *agentConn, req *agentboard.ListRetainedRequest) {
	if !ac.helloed || req == nil {
		return
	}
	out := agentboard.ListRetainedResponse{RequestId: req.RequestId}
	send := func() {
		resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_ListRetainedResponse}
		resp.SetListRetainedResponse(out)
		s.sendAgent(conn, resp)
	}

	msgs, found := s.Board.ListRetained(string(req.Topic))
	if !found {
		out.Status = agentboard.PurgeStatus_NotFound
		send()
		return
	}
	out.Status = agentboard.PurgeStatus_Ok
	for _, m := range msgs {
		size := len(m.Payload)
		if size > 0xffffffff {
			size = 0xffffffff
		}
		meta := agentboard.RetainedMeta{
			Seq:              m.Seq,
			InReplyTo:        m.InReplyTo,
			FromRunner:       protoToAgentboardRunnerID(m),
			FromTask:         protoToAgentboardTaskID(m),
			Size:             uint32(size),
			ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
		}
		meta.SetFromHostname([]byte(m.FromHostname))
		meta.SetFromAgentProfile([]byte(m.FromAgentProfile))
		out.Metas = append(out.Metas, meta)
	}
	out.MetasLen = uint16(len(out.Metas))
	send()
}
