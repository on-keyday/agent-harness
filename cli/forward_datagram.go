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
			// payload cannot cross. The COUNT lives server-side against the row
			// `forward ls` shows; here the honest act is to drop it rather than
			// send a truncated datagram the far end would treat as complete.
			logf(fmt.Sprintf("udp forward %d: datagram of %d bytes exceeds the %d that fit; dropped",
				forwardID, n, send.MaxDatagramSize()))
			continue
		}
		_ = send.SendDatagram(b)
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
