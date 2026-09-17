package cli

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// udpFlowIdleTimeout mirrors the runner's: 60s, between conntrack's 30s
// unreplied and 180s assured. Both ends reap on the same clock so a flow does
// not survive on one side of the tunnel and not the other.
const udpFlowIdleTimeout = 60 * time.Second

// udpClientFlowCap bounds how many source addresses one forward tracks. A peer
// varying its source port would otherwise grow this map without bound, and
// nothing in UDP says when an old address is finished.
const udpClientFlowCap = 512

// udpForwardClient is the client end of a udp -L: ONE listening socket, and a
// flow id per source address that has spoken to it.
//
// One socket, not one per flow, because this end is a LISTENER — every source
// arrives on it, and what has to be remembered is which id belongs to whom so a
// reply can be written back to the right address. The runner's end is the
// mirror image: one socket per flow, because it dials.
type udpForwardClient struct {
	drops     protocol.ForwardDropCounters
	forwardID uint64
	conn      *net.UDPConn
	send      datagramSender

	mu      sync.Mutex
	nextID  uint32
	byAddr  map[string]uint32
	byID    map[uint32]*udpFlowPeer
	dropped func(cause string)
}

// udpFlowPeer is one source address and when it was last heard from.
type udpFlowPeer struct {
	addr     *net.UDPAddr
	lastSeen time.Time
}

// datagramSender is what a udp forward needs from the connection. Narrow on
// purpose: the read loop should not be able to reach the control frame.
type datagramSender interface {
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}

// flowFor returns the id for addr, allocating on first sight.
//
// Allocation is the CLIENT's because it is the end that sees source addresses;
// the runner only ever learns ids that arrive. That asymmetry is what lets the
// runner treat an unseen id as "dial the target" with no further negotiation.
func (u *udpForwardClient) flowFor(addr *net.UDPAddr) uint32 {
	key := addr.String()
	u.mu.Lock()
	defer u.mu.Unlock()
	if id, ok := u.byAddr[key]; ok {
		u.byID[id].lastSeen = time.Now()
		return id
	}
	if len(u.byID) >= udpClientFlowCap {
		u.evictOldestLocked()
	}
	u.nextID++
	id := u.nextID
	u.byAddr[key] = id
	u.byID[id] = &udpFlowPeer{addr: addr, lastSeen: time.Now()}
	return id
}

// peerFor resolves a reply's destination.
func (u *udpForwardClient) peerFor(id uint32) (*net.UDPAddr, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.byID[id]
	if !ok {
		return nil, false
	}
	p.lastSeen = time.Now()
	return p.addr, true
}

func (u *udpForwardClient) evictOldestLocked() {
	var oldestID uint32
	var oldest *udpFlowPeer
	for id, p := range u.byID {
		if oldest == nil || p.lastSeen.Before(oldest.lastSeen) {
			oldestID, oldest = id, p
		}
	}
	if oldest == nil {
		return
	}
	delete(u.byID, oldestID)
	delete(u.byAddr, oldest.addr.String())
}

// reapIdle drops flows that have gone quiet.
func (u *udpForwardClient) reapIdle(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, p := range u.byID {
		if now.Sub(p.lastSeen) < udpFlowIdleTimeout {
			continue
		}
		delete(u.byID, id)
		delete(u.byAddr, p.addr.String())
	}
}

// deliver writes one reply back to the source that started its flow.
func (u *udpForwardClient) deliver(dg *protocol.ForwardDatagram) {
	addr, ok := u.peerFor(dg.FlowId)
	if !ok {
		// The flow was reaped while a reply was in flight. Dropping is the only
		// option — there is no address left to write to — and it is also what a
		// NAT box would have done with the same packet.
		return
	}
	_, _ = u.conn.WriteToUDP(dg.Payload, addr)
}

// runUDPForward listens for a udp -L and carries its datagrams until ctx ends.
//
// Blocks, like RunForward's accept loop: the caller owns the goroutine.
func runUDPForward(ctx context.Context, send datagramSender, sp ForwardSpec,
	forwardID uint64, ln *net.UDPConn, logf func(string)) {
	u := &udpForwardClient{
		forwardID: forwardID,
		conn:      ln,
		send:      send,
		byAddr:    map[string]uint32{},
		byID:      map[uint32]*udpFlowPeer{},
	}
	registerUDPForwardClient(forwardID, u)
	defer unregisterUDPForwardClient(forwardID)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		t := time.NewTicker(udpFlowIdleTimeout / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				u.reapIdle(now)
				// The drop report rides the same ticker: no goroutine and no
				// timer of its own for something that only has to be roughly
				// current, and silent when nothing changed.
				u.reportDrops(send, logf)
			}
		}
	}()

	buf := make([]byte, 64*1024)
	for {
		n, addr, err := ln.ReadFromUDP(buf)
		if err != nil {
			return // listener closed (ctx done) or a fatal read error
		}
		if n == 0 {
			continue
		}
		id := u.flowFor(addr)
		dg := protocol.ForwardDatagram{ForwardId: forwardID, FlowId: id, Payload: buf[:n]}
		b, eerr := dg.Append([]byte{byte(appwire.AppKind_ForwardDatagram)})
		if eerr != nil {
			continue
		}
		if len(b) > send.MaxDatagramSize() {
			// There is no fragmentation anywhere below this, so an oversized
			// payload cannot cross. Counted HERE and reported: the server's own
			// oversize counter fires when the FAR leg cannot take a datagram it
			// already received, and one too large for this leg never arrives,
			// which is how a row came to read oversize=0 while this very line
			// was logging every drop.
			u.drops.NoteOversize()
			u.reportDrops(send, logf)
			logf(fmt.Sprintf("udp forward %d: datagram of %d bytes exceeds the %d that fit; dropped",
				forwardID, n, send.MaxDatagramSize()))
			continue
		}
		if err := send.SendDatagram(b); err != nil {
			u.drops.NoteSendError(err)
			u.reportDrops(send, logf)
		}
	}
}

// reportDrops tells the server what this endpoint dropped, so `forward ls`
// stops reporting only what the server's own relay dropped.
//
// Sent on the same unreliable frame as the data it counts, which is safe
// because the totals are cumulative: a refused report is carried by the next
// tick. A failure here is deliberately not counted as a drop of its own --
// counting the loss of a loss report is a hall of mirrors, and the number it
// would corrupt is the one being reported.
func (u *udpForwardClient) reportDrops(send datagramSender, logf func(string)) {
	if err := u.drops.ReportTo(u.forwardID, send.SendDatagram); err != nil {
		// Logged, not swallowed. A report that cannot cross is the one case
		// where the operator's numbers go stale without anything saying so, and
		// this file has already paid once for a drop nobody counted.
		logf(fmt.Sprintf("udp forward %d: drop report not sent: %v", u.forwardID, err))
	}
}

func (d *udpRemoteDialer) reportDrops(send datagramSender, logf func(string)) {
	if err := d.drops.ReportTo(d.forwardID, send.SendDatagram); err != nil {
		logf(fmt.Sprintf("udp remote forward %d: drop report not sent: %v", d.forwardID, err))
	}
}

// udpForwardClients is the process-wide registry a received datagram is routed
// through. Keyed by forward id, which is what the datagram names.
//
// Process-wide rather than per-Client because the datagram seam is on the
// connection and a forward is identified by an id the SERVER assigned — two
// Clients in one process cannot be handed the same id.
var (
	udpForwardClientsMu sync.Mutex
	udpForwardClients   = map[uint64]*udpForwardClient{}
)

func registerUDPForwardClient(id uint64, u *udpForwardClient) {
	udpForwardClientsMu.Lock()
	udpForwardClients[id] = u
	udpForwardClientsMu.Unlock()
}

func unregisterUDPForwardClient(id uint64) {
	udpForwardClientsMu.Lock()
	delete(udpForwardClients, id)
	udpForwardClientsMu.Unlock()
}

func lookupUDPForwardClient(id uint64) (*udpForwardClient, bool) {
	udpForwardClientsMu.Lock()
	defer udpForwardClientsMu.Unlock()
	u, ok := udpForwardClients[id]
	return u, ok
}

// dispatchDatagram is the client's datagram seam, the mirror of
// dispatchControl. One kind reaches it today; the switch is so a second is a
// case rather than a rewrite of the caller.
func (c *Client) dispatchDatagram(kind appwire.AppKind, payload []byte) {
	if kind != appwire.AppKind_ForwardDatagram {
		return
	}
	var dg protocol.ForwardDatagram
	if err := dg.DecodeExact(payload); err != nil {
		return
	}
	// A -R forward's datagrams are REQUESTS from a source on the runner side,
	// to be dialled locally. Checked first because the two registries are
	// disjoint and an id belongs to exactly one of them.
	if d, ok := lookupUDPRemoteDialer(dg.ForwardId); ok {
		d.deliver(&dg, func(string) {})
		return
	}
	u, ok := lookupUDPForwardClient(dg.ForwardId)
	if !ok {
		// Either the forward has ended or this reply belongs to another
		// process's. Silent: at teardown the far end may still have packets in
		// flight, and a log line each would drown whatever ended the forward.
		return
	}
	u.deliver(&dg)
}

// listenUDPForward binds the client-side socket for a udp -L and reports the
// port the kernel actually assigned, which is what gets registered.
func listenUDPForward(sp ForwardSpec) (*net.UDPConn, int, error) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(sp.BindAddr, strconv.Itoa(sp.LocalPort)))
	if err != nil {
		return nil, 0, err
	}
	ln, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.LocalAddr().(*net.UDPAddr).Port, nil
}

// --- udp -R: the runner listens, the CLIENT dials -----------------------
//
// The mirror of the pair above. Here the runner allocates the flow ids — it is
// the end holding the listening socket and therefore the end that sees source
// addresses — and this end dials one socket per flow, because the target's
// reply has to come back on the socket the request left from.

// udpRemoteDialer is the client end of a udp -R: the local target, and the
// per-flow sockets dialled toward it so far.
type udpRemoteDialer struct {
	drops     protocol.ForwardDropCounters
	logf      func(string)
	forwardID uint64
	target    string
	send      datagramSender

	mu    sync.Mutex
	flows map[uint32]*udpDialedFlow
}

type udpDialedFlow struct {
	conn     *net.UDPConn
	lastSeen time.Time
	stop     chan struct{}
}

// deliver sends one datagram to the local target, dialling on first sight of
// its flow id. An unseen id IS the flow's announcement — there is no
// conn_notify for udp, because a flow has no accept to report.
func (d *udpRemoteDialer) deliver(dg *protocol.ForwardDatagram, logf func(string)) {
	fl, err := d.flow(dg.FlowId, logf)
	if err != nil {
		logf(fmt.Sprintf("remote-forward udp %d: dial %s failed: %v", d.forwardID, d.target, err))
		return
	}
	_, _ = fl.conn.Write(dg.Payload)
}

func (d *udpRemoteDialer) flow(id uint32, logf func(string)) (*udpDialedFlow, error) {
	d.mu.Lock()
	if fl, ok := d.flows[id]; ok {
		fl.lastSeen = time.Now()
		d.mu.Unlock()
		return fl, nil
	}
	d.mu.Unlock()

	// Dialled outside the lock: a name can be slow to resolve, and holding it
	// there would stall every other flow behind one.
	raddr, err := net.ResolveUDPAddr("udp", d.target)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if fl, ok := d.flows[id]; ok {
		d.mu.Unlock()
		_ = conn.Close()
		return fl, nil
	}
	if len(d.flows) >= udpClientFlowCap {
		d.evictOldestLocked()
	}
	fl := &udpDialedFlow{conn: conn, lastSeen: time.Now(), stop: make(chan struct{})}
	d.flows[id] = fl
	d.mu.Unlock()
	go d.readReplies(id, fl)
	return fl, nil
}

func (d *udpRemoteDialer) evictOldestLocked() {
	var oldestID uint32
	var oldest *udpDialedFlow
	for id, fl := range d.flows {
		if oldest == nil || fl.lastSeen.Before(oldest.lastSeen) {
			oldestID, oldest = id, fl
		}
	}
	if oldest == nil {
		return
	}
	delete(d.flows, oldestID)
	close(oldest.stop)
	_ = oldest.conn.Close()
}

// readReplies carries the local target's answers back under the same flow id,
// which is the only thing that tells the runner which source they belong to.
func (d *udpRemoteDialer) readReplies(id uint32, fl *udpDialedFlow) {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-fl.stop:
			return
		default:
		}
		// A deadline rather than a bare Read, so a flow whose target never
		// answers still wakes often enough to be reaped.
		_ = fl.conn.SetReadDeadline(time.Now().Add(udpFlowIdleTimeout / 4))
		n, err := fl.conn.Read(buf)
		if n > 0 {
			d.mu.Lock()
			fl.lastSeen = time.Now()
			d.mu.Unlock()
			dg := protocol.ForwardDatagram{ForwardId: d.forwardID, FlowId: id, Payload: buf[:n]}
			if b, eerr := dg.Append([]byte{byte(appwire.AppKind_ForwardDatagram)}); eerr == nil {
				if len(b) > d.send.MaxDatagramSize() {
					d.drops.NoteOversize()
					d.reportDrops(d.send, d.logf)
				} else if serr := d.send.SendDatagram(b); serr != nil {
					d.drops.NoteSendError(serr)
					d.reportDrops(d.send, d.logf)
				}
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
	}
}

func (d *udpRemoteDialer) reapIdle(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, fl := range d.flows {
		if now.Sub(fl.lastSeen) < udpFlowIdleTimeout {
			continue
		}
		delete(d.flows, id)
		close(fl.stop)
		_ = fl.conn.Close()
	}
}

func (d *udpRemoteDialer) closeAll() {
	d.mu.Lock()
	flows := d.flows
	d.flows = map[uint32]*udpDialedFlow{}
	d.mu.Unlock()
	for _, fl := range flows {
		close(fl.stop)
		_ = fl.conn.Close()
	}
}

// udpRemoteDialers is the registry a received -R datagram routes through,
// keyed by forward id like its -L counterpart.
var (
	udpRemoteDialersMu sync.Mutex
	udpRemoteDialers   = map[uint64]*udpRemoteDialer{}
)

func registerUDPRemoteDialer(id uint64, d *udpRemoteDialer) {
	udpRemoteDialersMu.Lock()
	udpRemoteDialers[id] = d
	udpRemoteDialersMu.Unlock()
}

func unregisterUDPRemoteDialer(id uint64) {
	udpRemoteDialersMu.Lock()
	d := udpRemoteDialers[id]
	delete(udpRemoteDialers, id)
	udpRemoteDialersMu.Unlock()
	if d != nil {
		d.closeAll()
	}
}

func lookupUDPRemoteDialer(id uint64) (*udpRemoteDialer, bool) {
	udpRemoteDialersMu.Lock()
	defer udpRemoteDialersMu.Unlock()
	d, ok := udpRemoteDialers[id]
	return d, ok
}

// runUDPRemoteForward carries one udp -R until ctx ends. Blocks, like its -L
// counterpart: the caller owns the goroutine.
func runUDPRemoteForward(ctx context.Context, send datagramSender, sp RemoteForwardSpec,
	forwardID uint64, logf func(string)) {
	d := &udpRemoteDialer{
		logf:      logf,
		forwardID: forwardID,
		target:    net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort)),
		send:      send,
		flows:     map[uint32]*udpDialedFlow{},
	}
	registerUDPRemoteDialer(forwardID, d)
	defer unregisterUDPRemoteDialer(forwardID)

	t := time.NewTicker(udpFlowIdleTimeout / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			d.reapIdle(now)
			d.reportDrops(send, logf)
		}
	}
}
