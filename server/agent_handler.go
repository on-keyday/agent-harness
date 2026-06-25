package server

import (
	"context"
	"encoding/hex"
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
		ac := s.getOrCreateAgentConn(conn)
		ac.helloed = true
		ac.state = s.Board.Attach(rid, tid, string(info.Hostname))
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

func (s *Server) agentHandleSend(conn ConnHandle, ac *agentConn, r *agentboard.SendRequest) {
	if !ac.helloed || r == nil {
		return
	}
	// Payload arrives on a client-initiated send-stream; read it before
	// publishing. Spawn a goroutine so the receive loop stays responsive.
	go func() {
		payload, err := readAgentPayloadStream(conn, r.PayloadStreamId)
		if err != nil {
			slog.Warn("agent_handler: read payload stream failed", "request_id", r.RequestId, "err", err)
			resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SendResponse}
			resp.SetSendResponse(agentboard.SendResponse{RequestId: r.RequestId, Status: agentboard.SendStatus_BadFrame})
			s.sendAgent(conn, resp)
			return
		}
		fromRid, fromTid, fromHost := ac.state.Identity()
		seq, sendErr := s.Board.Send(string(r.Topic), payload, fromRid, fromTid, fromHost)
		var status agentboard.SendStatus
		switch sendErr {
		case nil:
			status = agentboard.SendStatus_Ok
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

// readAgentPayloadStream resolves the receive stream by id and reads the
// full body to EOF. Mirrors cli/agent/conn.go::FetchDeliveredPayload.
func readAgentPayloadStream(conn ConnHandle, id uint64) ([]byte, error) {
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
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("payload stream %d read: %w", sid, err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			return raw, nil
		}
	}
}

// writeDeliveredPayloadStream allocates a server-initiated send-stream,
// writes payload + EOF, returns the stream id (or 0 + error). Used by
// Wait/Inbox responders.
func writeDeliveredPayloadStream(conn ConnHandle, payload []byte) (uint64, error) {
	stream := conn.CreateSendStream()
	if stream == nil {
		return 0, fmt.Errorf("CreateSendStream returned nil")
	}
	if werr := stream.AppendData(false, payload); werr != nil {
		return 0, fmt.Errorf("payload stream write: %w", werr)
	}
	if werr := stream.AppendData(true); werr != nil {
		return 0, fmt.Errorf("payload stream EOF: %w", werr)
	}
	return uint64(stream.ID()), nil
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
	for _, m := range msgs {
		streamID, werr := writeDeliveredPayloadStream(conn, m.Payload)
		if werr != nil {
			slog.Warn("agent_handler: wait deliver stream", "seq", m.Seq, "err", werr)
			continue
		}
		dm := agentboard.DeliveredMessage{
			Seq:             m.Seq,
			PayloadStreamId: streamID,
			FromRunnerId:    protoToAgentboardRunnerID(m),
			FromTaskId:      protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetFromHostname([]byte(m.FromHostname))
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
}

func (s *Server) agentHandleInbox(conn ConnHandle, ac *agentConn, r *agentboard.InboxRequest) {
	if !ac.helloed || r == nil {
		return
	}
	msgs, next := s.Board.Inbox(ac.state, r.Since)
	delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
	for _, m := range msgs {
		streamID, werr := writeDeliveredPayloadStream(conn, m.Payload)
		if werr != nil {
			slog.Warn("agent_handler: inbox deliver stream", "seq", m.Seq, "err", werr)
			continue
		}
		dm := agentboard.DeliveredMessage{
			Seq:             m.Seq,
			PayloadStreamId: streamID,
			FromRunnerId:    protoToAgentboardRunnerID(m),
			FromTaskId:      protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetFromHostname([]byte(m.FromHostname))
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
				_, tid, _ := ac.state.Identity()
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
		_, tid, _ := ac.state.Identity()
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
			FromRunner:       protoToAgentboardRunnerID(m),
			FromTask:         protoToAgentboardTaskID(m),
			Size:             uint32(size),
			ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
		}
		meta.SetFromHostname([]byte(m.FromHostname))
		out.Metas = append(out.Metas, meta)
	}
	out.MetasLen = uint16(len(out.Metas))
	send()
}
