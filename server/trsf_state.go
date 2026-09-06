package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// runnerTrsfTimeout bounds the round trip to a runner. Short: the answer is a
// snapshot of counters already in memory, so a runner that has not replied in
// this long is not slow, it is not answering.
const runnerTrsfTimeout = 3 * time.Second

// trsfConnStates is the ONE walk of this process's connections. SIGUSR1's dump
// and the trsf_state request both read it, so the two views cannot drift into
// reporting different numbers for the same connection.
//
// Visibility is not decided here and is not restated here: it is connInfoFor's
// decision, the same one `conns` publishes. A caller therefore sees the trsf
// state of exactly the connections it can already see listed — and, by that
// same rule, a confined caller sees no runner connections at all. That last
// part is load-bearing rather than incidental: a runner's connection
// multiplexes every task on that runner, so its congestion counters are the sum
// over all of them and cannot be attributed to, or filtered by, one task.
func (s *Server) trsfConnStates(allowed map[string]bool, globalView bool) []protocol.TrsfConnState {
	s.activeConnsMu.Lock()
	conns := make([]streamingConn, 0, len(s.activeConns))
	for _, c := range s.activeConns {
		conns = append(conns, c)
	}
	s.activeConnsMu.Unlock()

	out := make([]protocol.TrsfConnState, 0, len(conns))
	for _, c := range conns {
		info := s.connInfoFor(c, allowed, globalView)
		if info == nil {
			continue // not visible to this caller
		}
		st := c.trans.GetInternalState()
		if st == nil {
			continue // a connection whose transport is already gone
		}
		row := trsfRow(st)
		row.Role = info.Role
		row.PrincipalTask = info.PrincipalTask
		row.SetCid(info.Cid)
		out = append(out, row)
	}
	return out
}

// trsfRow projects trsf's own state onto the wire record. SentPackets is left
// out deliberately: it is unbounded, and no reading of a stalled transfer needs
// per-packet detail.
func trsfRow(st *trsf.InternalState) protocol.TrsfConnState {
	return protocol.TrsfConnState{
		Mtu:            uint32(st.CurrentMTU),
		Cwnd:           uint32(st.CongestionWindow),
		BytesInFlight:  uint32(st.BytesInFlight),
		SrttUs:         uint64(st.SmoothedRTT.Microseconds()),
		RttvarUs:       uint64(st.RTTVariance.Microseconds()),
		SendQueue:      uint32(st.SendQueueLength),
		RecvQueue:      uint32(st.ReceiveQueueLength),
		SendStreams:    uint32(st.ActiveSendStreams),
		RecvStreams:    uint32(st.ActiveReceiveStreams),
		LoopIterations: st.LoopIterations,
		LossEvents:     uint64(st.Loss.Events),
		LossPackets:    uint64(st.Loss.Packets),
		LossSpurious:   uint64(st.Loss.Spurious),
	}
}

// sendRunnerTrsfStateRequest asks one runner for its own transport state.
//
// Correlated by request_id rather than by the runner's connection id, which is
// what the data-plane sibling uses. The id is on the wire, so reading it is
// what keeps it from being a field nobody consults -- and it lets two callers
// poll the same runner at once, which --watch makes ordinary.
func (s *Server) sendRunnerTrsfStateRequest(ctx context.Context, entry *RunnerEntry) ([]protocol.TrsfConnState, error) {
	if entry == nil || entry.Conn == nil {
		return nil, fmt.Errorf("runner offline")
	}
	id := s.trsfReqSeq.Add(1)
	respCh := make(chan protocol.RunnerTrsfStateResponse, 1)
	s.trsfRespMu.Lock()
	if s.trsfRespCh == nil {
		s.trsfRespCh = make(map[uint32]chan protocol.RunnerTrsfStateResponse)
	}
	s.trsfRespCh[id] = respCh
	s.trsfRespMu.Unlock()
	defer func() {
		s.trsfRespMu.Lock()
		delete(s.trsfRespCh, id)
		s.trsfRespMu.Unlock()
	}()

	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_TrsfState
	rr.SetTrsfState(protocol.RunnerTrsfStateRequest{RequestId: id})
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if _, _, err := entry.Conn.SendMessage(payload); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, runnerTrsfTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp.Conns, nil
	}
}

// deliverRunnerTrsfStateResponse routes a runner's answer to whoever asked.
func (s *Server) deliverRunnerTrsfStateResponse(resp protocol.RunnerTrsfStateResponse) {
	s.trsfRespMu.Lock()
	ch, ok := s.trsfRespCh[resp.RequestId]
	s.trsfRespMu.Unlock()
	if !ok {
		// A late answer to a request that already timed out. Not worth a
		// warning: --watch produces these whenever a runner is slow once.
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// handleTrsfState answers a caller's request for congestion state.
//
// No capability bit is read, deliberately. Which connections the answer carries
// is a projection of task visibility, exactly as `conns` decides it, so this
// hands back the trsf state of connections the caller can already see listed
// and nothing more. Asking a RUNNER needs the global view, because every
// connection a runner holds is either its shared server connection -- which has
// no principal task to project from, and whose counters are the sum over every
// task on that runner -- or a data-plane one.
func (h *TaskHandler) handleTrsfState(conn ConnHandle, requestID uint32, cid string, req *protocol.TrsfStateRequest) {
	// The rows go on a stream and the response carries only its id, exactly as
	// handleListConns does. Inline they do not fit: ten rows is 1229 bytes
	// against udp's 1200 path MTU, objproto does not split an application
	// message, and the failed send is a dropped error the caller sees as a
	// hang. Loopback's 65536 hides all of that, which is why the first cut of
	// this shipped inline and passed every local test.
	respond := func(st protocol.TrsfStateStatus, streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_TrsfState, RequestId: requestID}
		resp.SetTrsfState(protocol.TrsfStateResponse{Status: st, StreamId: streamID})
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	send := func(rows []protocol.TrsfConnState) {
		var body protocol.TrsfStateResultBody
		body.Count = uint16(len(rows))
		body.SetConns(rows)
		bodyBytes, err := body.EncodeCopy(nil)
		if err != nil {
			slog.Error("trsf_state: encode body", "err", err)
			respond(protocol.TrsfStateStatus_Unavailable, 0)
			return
		}
		stream := conn.CreateSendStream()
		if stream == nil {
			respond(protocol.TrsfStateStatus_Unavailable, 0)
			return
		}
		// The id goes back BEFORE the bytes: the caller waits for the stream to
		// become visible by that id, so writing first would race it.
		respond(protocol.TrsfStateStatus_Ok, uint64(stream.ID()))
		if werr := stream.AppendData(false, bodyBytes); werr != nil {
			slog.Warn("trsf_state: write body", "err", werr)
			return
		}
		_ = stream.AppendData(true)
	}

	globalView, allowed := h.visibleToCaller(cid)

	if req.Target == protocol.TrsfTarget_Server {
		if h.TrsfStateFn == nil {
			respond(protocol.TrsfStateStatus_Unavailable, 0)
			return
		}
		send(h.TrsfStateFn(allowed, globalView))
		return
	}

	if !globalView {
		respond(protocol.TrsfStateStatus_NotPermitted, 0)
		return
	}
	if h.RunnerTrsfStateFn == nil {
		respond(protocol.TrsfStateStatus_Unavailable, 0)
		return
	}
	rows, err := h.RunnerTrsfStateFn(context.Background(), req.RunnerCid)
	switch {
	case errors.Is(err, errRunnerOffline):
		respond(protocol.TrsfStateStatus_RunnerOffline, 0)
	case err != nil:
		slog.Warn("trsf_state: runner did not answer", "err", err)
		respond(protocol.TrsfStateStatus_Unavailable, 0)
	default:
		send(rows)
	}
}

// errRunnerOffline separates "no such runner" from "the runner did not answer",
// because the two send the caller to different places: one is a stale id, the
// other is a runner worth looking at.
var errRunnerOffline = errors.New("runner offline")
