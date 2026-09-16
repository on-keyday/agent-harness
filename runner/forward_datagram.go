package runner

import (
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// udpFlowIdleTimeout is how long a flow with no traffic survives.
//
// UDP has no close, so a flow ends only by going quiet. 60s sits between
// conntrack's two UDP timeouts — 30s while a reply is still unseen, 180s once
// one has arrived — which are the numbers every middlebox on the path is
// already applying to the same traffic.
const udpFlowIdleTimeout = 60 * time.Second

// udpFlowSweepInterval is how often idle flows are reaped. Coarse relative to
// the timeout, because the cost of a flow living a few seconds too long is one
// socket and the cost of sweeping often is a lock taken on every tick.
const udpFlowSweepInterval = 15 * time.Second

// udpFlowsPerForward caps the sockets one registration may hold open.
//
// Without it a peer that varies its source port makes the runner open sockets
// without bound — every new port is a new flow, and nothing in UDP says when
// the old one ended. The cap evicts the least recently used, which for this
// traffic is the flow most likely to be finished.
const udpFlowsPerForward = 512

// udpForward is one udp -L registration as the runner sees it: a target to dial
// and the per-flow sockets dialled so far.
//
// The target arrives ONCE, in the RunnerOpenPortForwardRequest that registration
// sends — unlike tcp, where one such request arrives per accepted connection and
// carries the stream for it. A udp flow has no open of its own, so the standing
// instruction is all the runner gets and all it needs.
type udpForward struct {
	forwardID uint64
	target    string
	send      datagramSender

	mu    sync.Mutex
	flows map[uint32]*udpFlow
}

// udpFlow is one 5-tuple: the socket the runner dialled for it, and when it was
// last used. The socket is per flow rather than shared because the target's
// reply has to come back on the socket the request left from — that is what
// makes a reply a reply.
type udpFlow struct {
	conn     *net.UDPConn
	lastSeen time.Time
	stop     chan struct{}
}

// datagramSender is the outbound half a udp forward needs. Narrower than Sender
// on purpose: the flow reader goroutines hold one of these, and handing them
// the whole Sender would let a reply path reach Publish and the control frame.
type datagramSender interface {
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}

// udpForwards is the runner's registry of udp -L forwards, keyed by the
// server-assigned forward id.
type udpForwards struct {
	mu sync.Mutex
	m  map[uint64]*udpForward
}

func (u *udpForwards) add(id uint64, target string, send datagramSender) *udpForward {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[uint64]*udpForward{}
	}
	f := &udpForward{forwardID: id, target: target, send: send, flows: map[uint32]*udpFlow{}}
	u.m[id] = f
	return f
}

func (u *udpForwards) get(id uint64) (*udpForward, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, ok := u.m[id]
	return f, ok
}

// close drops one registration and every socket under it. Called when the
// server says the forward is over; without it a registration's sockets outlive
// the forward that justified them.
func (u *udpForwards) close(id uint64) {
	u.mu.Lock()
	f := u.m[id]
	delete(u.m, id)
	u.mu.Unlock()
	if f != nil {
		f.closeAll()
	}
}

// closeAll drops every flow. Each reader goroutine ends when its socket closes.
func (f *udpForward) closeAll() {
	f.mu.Lock()
	flows := f.flows
	f.flows = map[uint32]*udpFlow{}
	f.mu.Unlock()
	for _, fl := range flows {
		close(fl.stop)
		_ = fl.conn.Close()
	}
}

// udpForwardRegistry lazily builds the session's registry.
func (s *Session) udpForwardRegistry() *udpForwards {
	if s.udpForwards == nil {
		s.udpForwards = &udpForwards{}
	}
	return s.udpForwards
}

// handleForwardDatagram delivers one datagram to its flow's socket, dialling
// the target the first time a flow id is seen.
//
// A flow is created by its first datagram arriving. That is the whole
// difference from tcp, where the client's accept is what starts a connection
// and a request has to cross the wire before any byte does.
func (s *Session) handleForwardDatagram(dg *protocol.ForwardDatagram) {
	log := s.logger()
	fwd, ok := s.udpForwardRegistry().get(dg.ForwardId)
	if !ok {
		// The registration is gone, or never reached this runner. Silent: at
		// teardown the far end may still have packets in flight, and one log
		// line per in-flight datagram would drown the reason the forward ended.
		return
	}
	flow, err := fwd.flow(dg.FlowId, log)
	if err != nil {
		log.Info("udp forward: dial failed", "fwd", dg.ForwardId, "target", fwd.target, "err", err)
		return
	}
	if _, werr := flow.conn.Write(dg.Payload); werr != nil {
		log.Info("udp forward: write to target failed", "fwd", dg.ForwardId, "err", werr)
	}
}

// flow returns the socket for id, dialling and starting its reader on first
// use.
func (f *udpForward) flow(id uint32, log *slog.Logger) (*udpFlow, error) {
	f.mu.Lock()
	if fl, ok := f.flows[id]; ok {
		fl.lastSeen = time.Now()
		f.mu.Unlock()
		return fl, nil
	}
	f.mu.Unlock()

	// Dialled OUTSIDE the lock: a dial can block on DNS, and holding the lock
	// there would stall every other flow on this forward behind one slow name.
	raddr, err := net.ResolveUDPAddr("udp", f.target)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	// Another datagram for the same flow may have won the race while this one
	// was dialling. Keep the winner and drop this socket, rather than replacing
	// it — the replaced one would have a reader goroutine already writing
	// replies under the same flow id.
	if fl, ok := f.flows[id]; ok {
		f.mu.Unlock()
		_ = conn.Close()
		return fl, nil
	}
	if len(f.flows) >= udpFlowsPerForward {
		f.evictOldestLocked()
	}
	fl := &udpFlow{conn: conn, lastSeen: time.Now(), stop: make(chan struct{})}
	f.flows[id] = fl
	f.mu.Unlock()

	go f.readReplies(id, fl, log)
	return fl, nil
}

// evictOldestLocked drops the least recently used flow. Caller holds f.mu.
func (f *udpForward) evictOldestLocked() {
	var oldestID uint32
	var oldest *udpFlow
	for id, fl := range f.flows {
		if oldest == nil || fl.lastSeen.Before(oldest.lastSeen) {
			oldestID, oldest = id, fl
		}
	}
	if oldest == nil {
		return
	}
	delete(f.flows, oldestID)
	close(oldest.stop)
	_ = oldest.conn.Close()
}

// readReplies pumps the target's answers back under the same flow id, which is
// what makes them answers: the client keyed its own source address by that id
// and has no other way to know where to write them.
func (f *udpForward) readReplies(id uint32, fl *udpFlow, log *slog.Logger) {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-fl.stop:
			return
		default:
		}
		// A read deadline rather than a bare Read, so a flow whose target never
		// answers still wakes often enough to be reaped.
		_ = fl.conn.SetReadDeadline(time.Now().Add(udpFlowSweepInterval))
		n, err := fl.conn.Read(buf)
		if n > 0 {
			f.mu.Lock()
			fl.lastSeen = time.Now()
			f.mu.Unlock()
			f.sendBack(id, buf[:n], log)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // socket closed (reaped, or the forward ended)
		}
	}
}

// sendBack wraps one reply in its framing and hands it to the transport.
func (f *udpForward) sendBack(id uint32, payload []byte, log *slog.Logger) {
	if f.send == nil {
		return
	}
	send := f.send
	dg := protocol.ForwardDatagram{ForwardId: f.forwardID, FlowId: id, Payload: payload}
	b, err := dg.Append([]byte{byte(appwire.AppKind_ForwardDatagram)})
	if err != nil {
		log.Warn("udp forward: encode reply", "err", err)
		return
	}
	if len(b) > send.MaxDatagramSize() {
		// No fragmentation anywhere below this, so the reply cannot cross. The
		// COUNT lives server-side, against the row an operator reads; here the
		// honest thing is to drop it rather than send something truncated.
		return
	}
	_ = send.SendDatagram(b)
}

// sweepIdleFlows reaps flows that have gone quiet. Runs for the session, not
// per forward: one ticker is enough for a registry this size, and a ticker per
// forward would be a goroutine per registration doing nothing.
func (u *udpForwards) sweepIdleFlows(now time.Time) {
	u.mu.Lock()
	forwards := make([]*udpForward, 0, len(u.m))
	for _, f := range u.m {
		forwards = append(forwards, f)
	}
	u.mu.Unlock()
	for _, f := range forwards {
		f.mu.Lock()
		for id, fl := range f.flows {
			if now.Sub(fl.lastSeen) < udpFlowIdleTimeout {
				continue
			}
			delete(f.flows, id)
			close(fl.stop)
			_ = fl.conn.Close()
		}
		f.mu.Unlock()
	}
}

// targetAddr renders the dial target the standing instruction named.
func targetAddr(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// handleDatagram is the runner's datagram seam. One kind reaches it today; the
// switch exists so a second one is a case rather than a rewrite of the caller.
func (s *Session) handleDatagram(kind appwire.AppKind, payload []byte) {
	if kind != appwire.AppKind_ForwardDatagram {
		return
	}
	var dg protocol.ForwardDatagram
	if err := dg.DecodeExact(payload); err != nil {
		s.logger().Warn("udp forward: undecodable datagram", "err", err)
		return
	}
	s.handleForwardDatagram(&dg)
}
