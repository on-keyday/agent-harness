package runner

import (
	"errors"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
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

// handleTrsfState answers the server's request with this runner's own state.
func handleTrsfState(sess *Session, req protocol.RunnerTrsfStateRequest, send func(protocol.RunnerMessage) error) {
	rows := sess.trsfStates()
	var rm protocol.RunnerMessage
	rm.Kind = protocol.RunnerMessageType_TrsfStateResponse
	rm.SetTrsfStateResponse(protocol.RunnerTrsfStateResponse{
		RequestId: req.RequestId,
		Count:     uint16(len(rows)),
		// Stamped HERE, by the clock the counters advanced against. The server
		// passes it through rather than restamping: its own clock is one round
		// trip away, and the delta between two readings is the interval every
		// one of these counters is read as a rate over.
		SampledUnixNs: uint64(time.Now().UnixNano()),
		Conns:         rows,
	})
	if err := send(rm); err != nil && sess != nil {
		sess.logger().Warn("trsf_state: could not answer", "err", err)
	}
}

// sendRunnerMessage is the one-liner the handler needs, kept here so the
// dispatch case reads as a single call.
func (s *Session) sendRunnerMessage(rm protocol.RunnerMessage) error {
	if s == nil || s.Sender == nil {
		return errNoSender
	}
	return s.Sender.Send(rm.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
}

var errNoSender = errors.New("runner: no sender wired")
