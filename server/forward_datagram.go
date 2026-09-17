package server

import (
	"context"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// pumpForwardDatagrams drains datagrams off one connection and relays each to
// the other end of the forward it names.
//
// This is the splice route for udp: the server terminates both legs and copies
// between them, exactly as it does for a TCP forward's streams — except that
// nothing here is a stream, so there is no splice to reuse and the relay is its
// own code. What the two share is the REGISTRY: a datagram names a forward_id,
// and that id is what says who may send it and where it goes.
//
// authed is read per datagram rather than the pump being started after the
// handshake. The pump must exist before the first packet arrives, and an
// unauthenticated connection must not reach the forward registry — the message
// path is fail-closed for the same reason, and a second path that was not would
// simply be the hole with extra steps.
func (s *Server) pumpForwardDatagrams(ctx context.Context, sc streamingConn, tr trsf.Transport, authed func() bool) {
	cid := sc.ConnectionID().String()
	for {
		b, err := tr.ReceiveDatagram(ctx)
		if err != nil {
			return // ctx ended with the connection
		}
		if !authed() {
			// Dropped in silence and deliberately: answering an unauthenticated
			// peer at all would tell it this port speaks the protocol.
			continue
		}
		if len(b) == 0 {
			continue
		}
		switch appwire.AppKind(b[0]) {
		case appwire.AppKind_ForwardDatagram:
			var dg protocol.ForwardDatagram
			if derr := dg.DecodeExact(b[1:]); derr != nil {
				s.cfg.Logger.Warn("forward datagram: undecodable", "cid", cid, "err", derr)
				continue
			}
			s.relayForwardDatagram(sc, cid, &dg, b)
		case appwire.AppKind_ForwardDropReport:
			var rep protocol.ForwardDropReport
			if derr := rep.DecodeExact(b[1:]); derr != nil {
				s.cfg.Logger.Warn("forward drop report: undecodable", "cid", cid, "err", derr)
				continue
			}
			s.noteForwardDropReport(cid, &rep)
		}
	}
}

// relayForwardDatagram re-emits one datagram on the far leg of its forward.
//
// The far leg depends on which end sent it: a datagram from the forward's own
// client goes to the runner holding its task, and one from that runner goes
// back to the client. Anything from a third connection is refused — the
// registration names exactly two endpoints, and a forward_id is not a
// capability anyone else may present.
func (s *Server) relayForwardDatagram(from streamingConn, cid string, dg *protocol.ForwardDatagram, raw []byte) {
	if s.taskHandler == nil {
		return
	}
	pf, ok := s.taskHandler.pforwards().get(dg.ForwardId)
	if !ok {
		// A registration that has gone is the ordinary race at teardown, not an
		// error: the far end may still have packets in flight. Silent, because a
		// log line per in-flight datagram would be the noisiest thing here.
		return
	}
	if pf.protocolKind != protocol.ForwardProtocol_Udp {
		s.cfg.Logger.Warn("forward datagram: forward is not udp", "cid", cid, "fwd", dg.ForwardId)
		return
	}

	var to ConnHandle
	switch {
	case pf.clientCID == cid:
		runner, rok := s.registry.GetByIdentity(pf.runnerID)
		if !rok || runner.Conn == nil {
			pf.noteDatagramDrop(dropQueue)
			return
		}
		to = runner.Conn
		pf.noteBytes(protocol.ForwardTapDirection_ToTarget, len(dg.Payload))
	default:
		runner, rok := s.registry.GetByIdentity(pf.runnerID)
		if !rok || runner.Conn == nil || runner.Conn.ConnectionID().String() != cid {
			// Neither endpoint of this registration. Refused rather than
			// relayed: a forward_id is an identifier, not an authorisation, and
			// treating it as one would let any connected peer inject into
			// somebody else's tunnel.
			s.cfg.Logger.Warn("forward datagram: sender is not an endpoint of this forward",
				"cid", cid, "fwd", dg.ForwardId)
			return
		}
		to = pf.clientCxn
		pf.noteBytes(protocol.ForwardTapDirection_FromTarget, len(dg.Payload))
	}
	if to == nil {
		pf.noteDatagramDrop(dropQueue)
		return
	}

	// Re-emitted byte for byte, framing included: the far end decodes the same
	// ForwardDatagram, so flow_id keeps its meaning across the hop and the
	// server needs to know nothing about what a flow is.
	pf.noteMaxDatagramSize(to.MaxDatagramSize())
	if len(raw) > to.MaxDatagramSize() {
		// The two legs can differ in MTU — a ws client and a udp runner do.
		// There is no fragmentation anywhere below this, so the honest outcome
		// is a counted drop, and the counter is what tells an operator why a
		// datagram protocol will not come up over this particular pair.
		pf.noteDatagramDrop(dropOversize)
		return
	}
	if err := to.SendDatagram(raw); err != nil {
		// The cause matters to whoever reads the row: a closed window is the
		// transport working, a full queue is this process falling behind.
		switch {
		case err == trsf.ErrCongestionBlocked:
			pf.noteDatagramDrop(dropCongestion)
		case err == trsf.ErrDatagramTooLarge:
			pf.noteDatagramDrop(dropOversize)
		default:
			pf.noteDatagramDrop(dropQueue)
		}
	}
}

// noteForwardDropReport records what one endpoint says it dropped before the
// server ever saw it.
//
// STORED, not added: the report carries running totals, because it rides the
// same unreliable frame as the data it counts and a lost one has to be
// harmless. Adding deltas would double-count a duplicate and lose a dropped
// one.
//
// Which endpoint sent it is read the same way relayForwardDatagram reads it,
// and for the same reason: a forward_id names two connections and nobody
// else's report about it is accepted.
func (s *Server) noteForwardDropReport(cid string, rep *protocol.ForwardDropReport) {
	if s.taskHandler == nil {
		return
	}
	pf, ok := s.taskHandler.pforwards().get(rep.ForwardId)
	if !ok {
		return // the registration went away; an in-flight report is ordinary
	}
	slot := &pf.runnerDrops
	if pf.clientCID == cid {
		slot = &pf.clientDrops
	} else {
		runner, rok := s.registry.GetByIdentity(pf.runnerID)
		if !rok || runner.Conn == nil || runner.Conn.ConnectionID().String() != cid {
			s.cfg.Logger.Warn("forward drop report: sender is not an endpoint of this forward",
				"cid", cid, "fwd", rep.ForwardId)
			return
		}
	}
	slot.oversize.Store(rep.DroppedOversize)
	slot.congestion.Store(rep.DroppedCongestion)
	slot.queue.Store(rep.DroppedQueue)
}
