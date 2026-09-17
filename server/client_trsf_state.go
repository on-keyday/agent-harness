package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// clientTrsfPending correlates a client's answer with the caller waiting for
// it, exactly as trsfRespCh does for a runner. A separate table rather than a
// shared one because the two carry different response types, and widening one
// map to hold both would cost a type switch at every delivery to save a field.
type clientTrsfPending struct {
	mu sync.Mutex
	ch map[uint32]chan protocol.ClientTrsfStateBody
}

func (p *clientTrsfPending) register(id uint32) chan protocol.ClientTrsfStateBody {
	respCh := make(chan protocol.ClientTrsfStateBody, 1)
	p.mu.Lock()
	if p.ch == nil {
		p.ch = map[uint32]chan protocol.ClientTrsfStateBody{}
	}
	p.ch[id] = respCh
	p.mu.Unlock()
	return respCh
}

func (p *clientTrsfPending) forget(id uint32) {
	p.mu.Lock()
	delete(p.ch, id)
	p.mu.Unlock()
}

func (p *clientTrsfPending) deliver(id uint32, body protocol.ClientTrsfStateBody) {
	p.mu.Lock()
	ch, ok := p.ch[id]
	p.mu.Unlock()
	if !ok {
		// A late answer to a request that already timed out. Not worth a
		// warning: --watch produces these whenever a client is slow once.
		return
	}
	select {
	case ch <- body:
	default:
	}
}

// sendClientTrsfStateRequest asks one client for its own transport state.
//
// This is the server making a REQUEST of a client, which nothing else does.
// It exists because a client's transport state is reachable from nowhere else:
// the client running a port forward is neither the server nor a runner, and the
// drops that matter most to it -- a datagram refused at the congestion gate
// inside trsf's run loop, after SendDatagram already returned nil -- never
// reach the client's own application code as an error either.
func (s *Server) sendClientTrsfStateRequest(ctx context.Context, conn ConnHandle) ([]protocol.TrsfConnState, int64, error) {
	if conn == nil {
		return nil, 0, errClientOffline
	}
	id := s.trsfReqSeq.Add(1)
	respCh := s.clientTrsf.register(id)
	defer s.clientTrsf.forget(id)

	var req protocol.ClientControlRequest
	req.Kind = protocol.ClientControlKind_TrsfState
	req.RequestId = id
	payload, err := req.Append([]byte{byte(appwire.AppKind_ClientControl)})
	if err != nil {
		return nil, 0, fmt.Errorf("encode: %w", err)
	}
	if _, _, err := conn.SendMessage(payload); err != nil {
		return nil, 0, fmt.Errorf("send: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, runnerTrsfTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case body := <-respCh:
		// The CLIENT's stamp. Same reason as the runner's: the counters
		// advanced on its clock, and a round trip separates the two.
		return body.Conns, int64(body.SampledUnixNs), nil
	}
}

// connByID finds a live wrapped connection by its id, which is how a client is
// named on the wire.
func (s *Server) connByID(cid protocol.ConnID) ConnHandle {
	s.activeConnsMu.Lock()
	defer s.activeConnsMu.Unlock()
	sc, ok := s.activeConns[cid.ToObjproto()]
	if !ok {
		return nil
	}
	return sc
}

// deliverClientControlResponse routes a client's answer to whoever asked.
func (s *Server) deliverClientControlResponse(payload []byte) {
	var resp protocol.ClientControlResponse
	if _, err := resp.Decode(payload); err != nil {
		s.cfg.Logger.Warn("client control: undecodable response", "err", err)
		return
	}
	if body := resp.TrsfState(); body != nil {
		s.clientTrsf.deliver(resp.RequestId, *body)
	}
}
