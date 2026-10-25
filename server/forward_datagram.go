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
//
// Every datagram that belongs to the forward is offered to its taps with what
// became of it. One refused before that point (an unknown sender, a forward
// that carries no datagrams) never belonged to it and is not offered.
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
	if !pf.protocolKind.CarriesDatagrams() {
		s.cfg.Logger.Warn("forward datagram: forward carries no datagrams", "cid", cid, "fwd", dg.ForwardId,
			"protocol", pf.protocolKind.String())
		return
	}

	var to ConnHandle
	var dir protocol.ForwardTapDirection
	switch {
	case pf.clientCID == cid:
		dir = protocol.ForwardTapDirection_ToTarget
		runner, rok := s.registry.GetByIdentity(pf.runnerID)
		if !rok || runner.Conn == nil {
			pf.noteDatagramDrop(dropQueue)
			pf.observeDatagram(dg.FlowId, dir, protocol.ForwardTapDatagramFate_DroppedQueue, dg.Payload)
			return
		}
		to = runner.Conn
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
		dir = protocol.ForwardTapDirection_FromTarget
		to = pf.clientCxn
	}
	pf.noteBytes(dir, len(dg.Payload))
	pf.observeDatagram(dg.FlowId, dir, s.sendForwardDatagram(pf, to, raw), dg.Payload)
}

// sendForwardDatagram re-emits raw on to and reports what became of it,
// counting every drop against the forward.
func (s *Server) sendForwardDatagram(pf *portForward, to ConnHandle, raw []byte) protocol.ForwardTapDatagramFate {
	if to == nil {
		pf.noteDatagramDrop(dropQueue)
		return protocol.ForwardTapDatagramFate_DroppedQueue
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
		return protocol.ForwardTapDatagramFate_DroppedOversize
	}
	fate := datagramFate(to.SendDatagram(raw))
	switch fate {
	case protocol.ForwardTapDatagramFate_DroppedCongestion:
		pf.noteDatagramDrop(dropCongestion)
	case protocol.ForwardTapDatagramFate_DroppedOversize:
		pf.noteDatagramDrop(dropOversize)
	case protocol.ForwardTapDatagramFate_DroppedQueue:
		pf.noteDatagramDrop(dropQueue)
	}
	return fate
}

// datagramFate names what a send error meant. The cause matters to whoever
// reads the row: a closed window is the transport working, a full queue is
// this process falling behind.
func datagramFate(err error) protocol.ForwardTapDatagramFate {
	switch {
	case err == nil:
		return protocol.ForwardTapDatagramFate_Relayed
	case err == trsf.ErrCongestionBlocked:
		return protocol.ForwardTapDatagramFate_DroppedCongestion
	case err == trsf.ErrDatagramTooLarge:
		return protocol.ForwardTapDatagramFate_DroppedOversize
	}
	return protocol.ForwardTapDatagramFate_DroppedQueue
}
