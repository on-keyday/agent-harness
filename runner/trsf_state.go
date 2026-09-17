package runner

import (
	"errors"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// The runner had no way to report its transport state at all, on any platform.
// The server's dump is wired to SIGUSR1, which does not exist on Windows, and
// nothing equivalent was ever written here — so the congestion state of the end
// that SENDS every pull, and half of every splice, has been unobservable since
// the beginning. That is what this file adds.
//
// It is a registry rather than a walk of something existing because the runner
// keeps no list of its connections: the uplink lives on the Session and each
// data-plane connection lives for one request inside its own handler.

// trsfConn is one connection this runner holds.
type trsfConn struct {
	cid   string
	role  protocol.ConnRole // names the PEER: server for the uplink, a client kind otherwise
	trans trsf.Transport
	task  protocol.TaskID // data-plane only; zero for the uplink
}

type trsfRegistry struct {
	mu    sync.Mutex
	conns map[string]trsfConn
}

func (r *trsfRegistry) add(c trsfConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[string]trsfConn)
	}
	r.conns[c.cid] = c
}

func (r *trsfRegistry) remove(cid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, cid)
}

func (r *trsfRegistry) snapshot() []trsfConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]trsfConn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

// registerTrsfConn records a connection so it can be reported. Safe on a nil
// Session so the call sites need no guard of their own.
func (s *Session) registerTrsfConn(cid string, role protocol.ConnRole, trans trsf.Transport, task protocol.TaskID) {
	if s == nil || trans == nil {
		return
	}
	s.trsfConns.add(trsfConn{cid: cid, role: role, trans: trans, task: task})
}

func (s *Session) unregisterTrsfConn(cid string) {
	if s == nil {
		return
	}
	s.trsfConns.remove(cid)
}

// trsfStates reports every connection this runner holds. It applies no policy:
// the SERVER decides which rows a caller may see, because it is the only party
// that evaluates scope (D7). A second implementation of that rule living here
// is exactly the drift this design has already paid for once.
func (s *Session) trsfStates() []protocol.TrsfConnState {
	if s == nil {
		return nil
	}
	conns := s.trsfConns.snapshot()
	out := make([]protocol.TrsfConnState, 0, len(conns))
	for _, c := range conns {
		st := c.trans.GetInternalState()
		if st == nil {
			continue
		}
		row := protocol.TrsfRowFrom(st)
		row.Role = c.role
		row.PrincipalTask = c.task
		row.SetCid([]byte(c.cid))
		out = append(out, row)
	}
	return out
}

// sessionTelemetry is what the server may ask this runner about itself.
//
// An adapter rather than methods on Session, so the answer side stays one seam
// instead of two exported methods on the type the whole runner is built around.
type sessionTelemetry struct{ s *Session }

func (t sessionTelemetry) TrsfStates() []protocol.TrsfConnState { return t.s.trsfStates() }

// ForwardDrops reads s.udpForwards rather than going through
// udpForwardRegistry, which CREATES it when absent. A runner holding no udp
// forward has no registry, and "no such forward" is the honest answer; creating
// one from a read would also write a field the forward-open path owns, from a
// goroutine with no reason to touch it.
func (t sessionTelemetry) ForwardDrops(forwardID uint64) (protocol.ForwardDropsBody, bool) {
	if t.s == nil || t.s.udpForwards == nil {
		return protocol.ForwardDropsBody{}, false
	}
	return t.s.udpForwards.dropsFor(forwardID)
}

// handleTelemetry answers whatever the server asked about this runner.
//
// Nothing here knows which question it was: AnswerTelemetry decodes it and
// calls back for the numbers, so a kind added to the family reaches a runner
// through sessionTelemetry rather than through another case in the dispatch
// switch.
func handleTelemetry(sess *Session, payload []byte) {
	if err := protocol.AnswerTelemetry(payload, sessionTelemetry{sess}, sess.sendTelemetry); err != nil && sess != nil {
		sess.logger().Warn("telemetry: could not answer", "err", err)
	}
}

// sendTelemetry puts one already-encoded answer on the uplink. The AppKind byte
// is on the front already; AnswerTelemetry owns the framing so both peers that
// answer cannot disagree about it.
func (s *Session) sendTelemetry(b []byte) error {
	if s == nil || s.Sender == nil {
		return errNoSender
	}
	return s.Sender.Send(b)
}

var errNoSender = errors.New("runner: no sender wired")
