package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// peerTelemetryTimeout bounds one round trip to a peer. Short: the answer is a
// snapshot of counters already in memory, so a peer that has not replied in
// this long is not slow, it is not answering.
const peerTelemetryTimeout = 3 * time.Second

// telemetryPending correlates a peer's answer with the caller waiting for it.
//
// Keyed by request_id rather than by the peer's connection id, which is what
// the data-plane sibling uses. The id is on the wire, so reading it is what
// keeps it from being a field nobody consults — and it lets two callers poll
// the same peer at once, which --watch makes ordinary.
//
// ONE table for every peer kind. It was briefly two, split on the argument that
// a runner and a client carried different response types; giving them one wire
// removed the difference, and the table that existed only to hold it.
type telemetryPending struct {
	seq atomic.Uint32

	mu sync.Mutex
	ch map[uint32]chan protocol.TelemetryResponse
}

func (p *telemetryPending) register() (uint32, chan protocol.TelemetryResponse) {
	id := p.seq.Add(1)
	respCh := make(chan protocol.TelemetryResponse, 1)
	p.mu.Lock()
	if p.ch == nil {
		p.ch = map[uint32]chan protocol.TelemetryResponse{}
	}
	p.ch[id] = respCh
	p.mu.Unlock()
	return id, respCh
}

func (p *telemetryPending) forget(id uint32) {
	p.mu.Lock()
	delete(p.ch, id)
	p.mu.Unlock()
}

func (p *telemetryPending) deliver(resp protocol.TelemetryResponse) {
	p.mu.Lock()
	ch, ok := p.ch[resp.RequestId]
	p.mu.Unlock()
	if !ok {
		// A late answer to a request that already timed out. Not worth a
		// warning: --watch produces these whenever a peer is slow once.
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// The two refusals a peer can name, kept apart from "it never answered" because
// they send the caller somewhere different: these are answers.
var (
	errTelemetryUnsupported    = errors.New("peer does not answer that question")
	errTelemetryUnknownForward = errors.New("peer holds no forward by that id")
)

// askTelemetry puts one question to a peer and waits for the answer.
//
// ONE implementation for every peer kind, which is the point: a runner and a
// cli client are asked the same bytes on the same AppKind, so which connection
// the request leaves on is the only thing that differs between them. This was
// briefly two — a pending table and an ask-and-await per peer kind, the second
// a copy of the first with the response type swapped.
func (s *Server) askTelemetry(ctx context.Context, conn ConnHandle, req protocol.TelemetryRequest) (protocol.TelemetryResponse, error) {
	var zero protocol.TelemetryResponse
	if conn == nil {
		return zero, fmt.Errorf("peer offline")
	}
	id, respCh := s.telemetry.register()
	defer s.telemetry.forget(id)

	req.RequestId = id
	payload, err := req.Append([]byte{byte(appwire.AppKind_Telemetry)})
	if err != nil {
		return zero, fmt.Errorf("encode: %w", err)
	}
	if _, _, err := conn.SendMessage(payload); err != nil {
		return zero, fmt.Errorf("send: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, peerTelemetryTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case resp := <-respCh:
		switch resp.Status {
		case protocol.TelemetryStatus_Ok:
			return resp, nil
		case protocol.TelemetryStatus_UnknownForward:
			return zero, errTelemetryUnknownForward
		default:
			return zero, errTelemetryUnsupported
		}
	}
}

// peerTrsfState asks one peer — a runner or a client — for its own transport
// state.
func (s *Server) peerTrsfState(ctx context.Context, conn ConnHandle) ([]protocol.TrsfConnState, int64, error) {
	resp, err := s.askTelemetry(ctx, conn, protocol.TelemetryRequest{Kind: protocol.TelemetryKind_TrsfState})
	if err != nil {
		return nil, 0, err
	}
	body := resp.TrsfState()
	if body == nil {
		return nil, 0, fmt.Errorf("telemetry: an ok trsf_state answer carried no body")
	}
	// The PEER's stamp, not this server's: the counters advanced on its clock,
	// and one round trip separates the two.
	return body.Conns, int64(body.SampledUnixNs), nil
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

// deliverTelemetryResponse routes a peer's answer to whoever asked. Every peer
// kind arrives here: the answer says which request it belongs to, and nothing
// downstream has to care who sent it.
func (s *Server) deliverTelemetryResponse(payload []byte) {
	var resp protocol.TelemetryResponse
	if _, err := resp.Decode(payload); err != nil {
		s.cfg.Logger.Warn("telemetry: undecodable response", "err", err)
		return
	}
	s.telemetry.deliver(resp)
}

// peerForwardDrops asks one endpoint what IT dropped for a forward.
func (s *Server) peerForwardDrops(ctx context.Context, conn ConnHandle, forwardID uint64) (protocol.ForwardDropsBody, error) {
	req := protocol.TelemetryRequest{Kind: protocol.TelemetryKind_ForwardDrops}
	req.SetForwardDrops(protocol.ForwardDropsQuery{ForwardId: forwardID})
	resp, err := s.askTelemetry(ctx, conn, req)
	if err != nil {
		return protocol.ForwardDropsBody{}, err
	}
	body := resp.ForwardDrops()
	if body == nil {
		return protocol.ForwardDropsBody{}, fmt.Errorf("telemetry: an ok forward_drops answer carried no body")
	}
	return *body, nil
}

// fillEndpointDrops asks both ends of every udp row what each of them dropped,
// and writes the answers onto the rows under their own per-hop keys.
//
// Only reached when the caller asked for it. It is two round trips per forward
// against a plain listing's map walk, and the numbers it adds are the ones the
// server structurally cannot know: a datagram an endpoint refused never arrived
// here. Measured at an offered 3,500 datagrams/s, that was 74% of the loss and
// every operator surface read zero for it.
//
// A peer that does not answer leaves its keys ABSENT rather than zero. The
// difference is the whole reason the row is a keyed list: "this endpoint did
// not report" and "this endpoint dropped nothing" send an operator to different
// places, and the three seconds an unreachable peer costs is itself a finding.
//
// Concurrent across rows and across the two ends of each, because the cost is
// entirely waiting: serially, one unreachable endpoint would add its full
// timeout to every row behind it.
func (s *Server) fillEndpointDrops(ctx context.Context, rows []protocol.PortForwardInfo) {
	if s.taskHandler == nil {
		return
	}
	type answer struct {
		row  int
		hop  protocol.ForwardHop
		body protocol.ForwardDropsBody
	}
	out := make(chan answer, 2*len(rows))
	var wg sync.WaitGroup
	ask := func(i int, hop protocol.ForwardHop, conn ConnHandle, id uint64) {
		if conn == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := s.peerForwardDrops(ctx, conn, id)
			if err != nil {
				s.cfg.Logger.Debug("forward drops: endpoint did not answer",
					"fwd", id, "hop", hop.String(), "err", err)
				return
			}
			out <- answer{row: i, hop: hop, body: body}
		}()
	}
	for i := range rows {
		// A udp row is one that reports a datagram size. Asking a tcp forward
		// what datagrams it dropped is a question with no meaning, and the row
		// itself is what says which kind it is.
		if _, ok := rows[i].Counter(protocol.ForwardCounterKey_MaxDatagramSize); !ok {
			continue
		}
		pf, ok := s.taskHandler.pforwards().get(rows[i].ForwardId)
		if !ok {
			continue // torn down between the listing and here; ordinary
		}
		ask(i, protocol.ForwardHopClient, pf.clientCxn, rows[i].ForwardId)
		if runner, rok := s.registry.GetByIdentity(pf.runnerID); rok {
			ask(i, protocol.ForwardHopRunner, runner.Conn, rows[i].ForwardId)
		}
	}
	wg.Wait()
	close(out)
	for a := range out {
		rows[a.row].SetForwardDropCounters(a.hop, a.body)
	}
}
