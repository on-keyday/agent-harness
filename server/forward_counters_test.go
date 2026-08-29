package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func newCounterForward() *portForward {
	return &portForward{forwardID: 1, conns: map[uint64]connBytes{}}
}

// TestForwardCountersBothDirections is the one most likely to be implemented
// backwards: to_target and from_target are named against the forward's target,
// so a swap here inverts every row on every surface.
func TestForwardCountersBothDirections(t *testing.T) {
	pf := newCounterForward()
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 100)
	pf.noteBytes(protocol.ForwardTapDirection_FromTarget, 250)
	pf.noteBytes(protocol.ForwardTapDirection_ToTarget, 5)

	to, from, total, open, last := pf.counters()
	if to != 105 || from != 250 {
		t.Fatalf("to=%d from=%d, want 105/250", to, from)
	}
	if total != 0 || open != 0 {
		t.Fatalf("bytes must not invent connections: total=%d open=%d", total, open)
	}
	if last == 0 {
		t.Fatal("last activity still zero after bytes crossed")
	}
}

func TestForwardCountersConnLifecycle(t *testing.T) {
	pf := newCounterForward()
	a := pf.openConn()
	b := pf.openConn()
	if a != 1 || b != 2 {
		t.Fatalf("conn_seq must be 1-based and per forward: %d %d", a, b)
	}
	_, _, total, open, _ := pf.counters()
	if total != 2 || open != 2 {
		t.Fatalf("after two opens: total=%d open=%d", total, open)
	}
	pf.closeConn(a)
	_, _, total, open, _ = pf.counters()
	if total != 2 {
		t.Fatalf("conns_total is a lifetime count and must not go down: %d", total)
	}
	if open != 1 {
		t.Fatalf("conns_open after one close: %d", open)
	}
}

// A fresh forward reports zeros, not absence. The row renders them.
func TestForwardCountersStartAtZeroNotAbsent(t *testing.T) {
	pf := newCounterForward()
	to, from, total, open, last := pf.counters()
	if to != 0 || from != 0 || total != 0 || open != 0 {
		t.Fatalf("fresh forward: %d %d %d %d", to, from, total, open)
	}
	if last != 0 {
		t.Fatal("last activity must stay 0 until a byte crosses; the renderer prints 'never' for it")
	}
}

// connBytes are per connection, not the forward's totals: a conn_close record
// reports what THAT connection carried while others are still moving.
func TestForwardConnBytesArePerConnection(t *testing.T) {
	pf := newCounterForward()
	a := pf.openConn()
	b := pf.openConn()
	pf.observe(a, protocol.ForwardTapDirection_ToTarget, []byte("12345"))
	pf.observe(b, protocol.ForwardTapDirection_ToTarget, []byte("678"))
	pf.observe(a, protocol.ForwardTapDirection_FromTarget, []byte("xy"))

	to, from := pf.connBytes(a)
	if to != 5 || from != 2 {
		t.Fatalf("conn %d: to=%d from=%d, want 5/2", a, to, from)
	}
	to, from = pf.connBytes(b)
	if to != 3 || from != 0 {
		t.Fatalf("conn %d: to=%d from=%d, want 3/0", b, to, from)
	}
}

// A long-lived forward must not accumulate one map entry per connection
// forever; closeConn releases the entry after its totals have been read.
func TestForwardConnBytesReleasedOnClose(t *testing.T) {
	pf := newCounterForward()
	a := pf.openConn()
	pf.observe(a, protocol.ForwardTapDirection_ToTarget, []byte("hello"))
	pf.closeConn(a)

	pf.connMu.Lock()
	n := len(pf.conns)
	pf.connMu.Unlock()
	if n != 0 {
		t.Fatalf("closeConn leaked %d per-connection entries", n)
	}
}
