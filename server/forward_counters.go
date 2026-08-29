package server

import (
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// connBytes is one connection's own halves. Kept per connection rather than
// derived from the forward's totals, which move under every other connection at
// the same time — a conn_close record has to report what THAT connection
// carried.
type connBytes struct{ toTarget, fromTarget uint64 }

// noteBytes records n bytes crossing this forward in dir. Called from the relay
// goroutine on every chunk, so it does two atomic stores and nothing else: the
// data plane must never wait on the accounting.
func (pf *portForward) noteBytes(dir protocol.ForwardTapDirection, n int) {
	if pf == nil || n <= 0 {
		return
	}
	if dir == protocol.ForwardTapDirection_ToTarget {
		pf.bytesToTarget.Add(uint64(n))
	} else {
		pf.bytesFromTarget.Add(uint64(n))
	}
	pf.lastActivityMs.Store(time.Now().UnixMilli())
}

// openConn assigns this forward's next conn_seq and counts the connection.
// 1-based: 0 is not a connection, which is why the tap's forward_closed arm
// carries no seq at all rather than a zero one.
func (pf *portForward) openConn() uint64 {
	if pf == nil {
		return 0
	}
	pf.connsTotal.Add(1)
	pf.connsOpen.Add(1)
	seq := pf.nextConnSeq.Add(1)
	pf.connMu.Lock()
	if pf.conns == nil {
		pf.conns = map[uint64]connBytes{}
	}
	pf.conns[seq] = connBytes{}
	pf.connMu.Unlock()
	return seq
}

// closeConn releases a connection and its per-connection entry. conns_total is
// a lifetime count and never goes down; only conns_open does. The entry is
// dropped here so a long-lived forward does not grow one map entry per
// connection for the life of the process.
func (pf *portForward) closeConn(seq uint64) {
	if pf == nil {
		return
	}
	pf.connsOpen.Add(-1)
	pf.connMu.Lock()
	delete(pf.conns, seq)
	pf.connMu.Unlock()
}

// connBytes returns what one connection has carried, for its conn_close record.
func (pf *portForward) connBytes(seq uint64) (toTarget, fromTarget uint64) {
	if pf == nil {
		return 0, 0
	}
	pf.connMu.Lock()
	defer pf.connMu.Unlock()
	c := pf.conns[seq]
	return c.toTarget, c.fromTarget
}

// counters reads the set the listing renders. conns_open is clamped at zero: a
// double close would otherwise surface a negative as a huge unsigned number on
// an operator's screen.
func (pf *portForward) counters() (toTarget, fromTarget, connsTotal uint64, connsOpen uint32, lastMs uint64) {
	if pf == nil {
		return 0, 0, 0, 0, 0
	}
	open := pf.connsOpen.Load()
	if open < 0 {
		open = 0
	}
	last := pf.lastActivityMs.Load()
	if last < 0 {
		last = 0
	}
	return pf.bytesToTarget.Load(), pf.bytesFromTarget.Load(),
		pf.connsTotal.Load(), uint32(open), uint64(last)
}

// observe accumulates the per-connection halves and, from the tap change on,
// offers the bytes to every tap reading this forward. Called from the relay
// goroutine: it must not block and must not retain data, which the relay
// reuses.
func (pf *portForward) observe(seq uint64, dir protocol.ForwardTapDirection, data []byte) {
	if pf == nil || len(data) == 0 {
		return
	}
	pf.connMu.Lock()
	if pf.conns != nil {
		c := pf.conns[seq]
		if dir == protocol.ForwardTapDirection_ToTarget {
			c.toTarget += uint64(len(data))
		} else {
			c.fromTarget += uint64(len(data))
		}
		pf.conns[seq] = c
	}
	pf.connMu.Unlock()

	pf.tapMu.Lock()
	taps := pf.taps
	pf.tapMu.Unlock()
	for _, t := range taps {
		t.offer(seq, dir, data)
	}
}
