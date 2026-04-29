package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf/wire"
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
	case agentboard.AgentMessageKind_Hello:
		s.agentHandleHello(conn, ac, msg.Hello())
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
	}
}

func (s *Server) sendAgent(conn ConnHandle, msg *agentboard.AgentMessage) {
	data, err := msg.Append([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
	if err != nil {
		slog.Warn("agent_message encode", "err", err)
		return
	}
	_, _, _ = conn.SendMessage(data)
}

func (s *Server) agentHandleHello(conn ConnHandle, ac *agentConn, h *agentboard.AgentBridgeHello) {
	if h == nil {
		return
	}
	status := s.Board.Registry().Validate(h.RunnerId, h.TaskId, h.AuthTicket)
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_HelloResponse}
	resp.SetHelloResponse(agentboard.AgentBridgeHelloResponse{Status: status})
	s.sendAgent(conn, resp)
	if status == agentboard.HelloStatusOk {
		ac.helloed = true
		ac.state = s.Board.Attach(h.RunnerId, h.TaskId, string(h.Hostname))
	}
	// On rejection we don't close the connection: ConnHandle does not
	// expose Close(), and subsequent agent messages are dropped by the
	// !ac.helloed gate in every other handler. The peer's own client
	// CLI exits after observing HelloResponse{status!=Ok}.
}

func (s *Server) agentHandleSend(conn ConnHandle, ac *agentConn, r *agentboard.SendRequest) {
	if !ac.helloed || r == nil {
		return
	}
	fromRid, fromTid, fromHost := ac.state.Identity()
	seq, err := s.Board.Send(string(r.Topic), r.Payload, fromRid, fromTid, fromHost)
	var status agentboard.SendStatus
	switch err {
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
		dm := agentboard.DeliveredMessage{
			Seq:          m.Seq,
			FromRunnerId: protoToAgentboardRunnerID(m),
			FromTaskId:   protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetPayload(m.Payload)
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
		dm := agentboard.DeliveredMessage{
			Seq:          m.Seq,
			FromRunnerId: protoToAgentboardRunnerID(m),
			FromTaskId:   protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetPayload(m.Payload)
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
