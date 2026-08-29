package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// forwardTapQueueDepth bounds one tap's backlog. Same shape as the session
// mux's viewer queue (session_mux.go, viewerQueueDepth) and for the same
// reason: the producer is a relay that must never wait.
//
// It differs in what overflow DOES. SessionMux drops the viewer; a tap keeps
// its consumer and reports a gap instead. A session viewer that is dropped can
// reattach and replay from the ring — a tap has no ring by design, and a tap
// that vanishes mid-investigation reads as "the forward closed", which is a
// false statement about the thing being investigated.
const forwardTapQueueDepth = 256

// forwardTapSink is where a tap's records go. The stream implementation is the
// real one; tests substitute a channel.
type forwardTapSink interface {
	send(rec *protocol.ForwardTapRecord) error
}

// tapStreamKey identifies one byte stream inside a forward: a connection and a
// direction. Offsets and missed-byte counts are per key, because the two
// directions of one connection are two independent streams.
type tapStreamKey struct {
	seq uint64
	dir protocol.ForwardTapDirection
}

type tapStreamState struct {
	offset uint64
	missed uint64
}

// forwardTap is one reader attached to one forward.
type forwardTap struct {
	filter         protocol.ForwardTapFilter
	maxRecordBytes uint32
	ch             chan *protocol.ForwardTapRecord
	sink           forwardTapSink

	mu      sync.Mutex
	streams map[tapStreamKey]*tapStreamState

	closed atomic.Bool
}

func newForwardTap(sink forwardTapSink, filter protocol.ForwardTapFilter, maxRecordBytes uint32) *forwardTap {
	return &forwardTap{
		filter:         filter,
		maxRecordBytes: maxRecordBytes,
		ch:             make(chan *protocol.ForwardTapRecord, forwardTapQueueDepth),
		sink:           sink,
		streams:        map[tapStreamKey]*tapStreamState{},
	}
}

func (t *forwardTap) wants(dir protocol.ForwardTapDirection) bool {
	switch t.filter {
	case protocol.ForwardTapFilter_ToTarget:
		return dir == protocol.ForwardTapDirection_ToTarget
	case protocol.ForwardTapFilter_FromTarget:
		return dir == protocol.ForwardTapDirection_FromTarget
	}
	return true
}

// missedBytes is the total this tap has failed to keep up with, across every
// stream. Test and diagnostic use; the gap records carry the per-stream halves.
func (t *forwardTap) missedBytes() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n uint64
	for _, st := range t.streams {
		n += st.missed
	}
	return n
}

func (t *forwardTap) state(key tapStreamKey) *tapStreamState {
	st := t.streams[key]
	if st == nil {
		st = &tapStreamState{}
		t.streams[key] = st
	}
	return st
}

// offer is called from the relay goroutine, once per chunk. It never blocks and
// never returns an error the relay would have to handle: a tap that cannot keep
// up loses bytes and is told so, rather than slowing the forward down.
//
// The payload is COPIED. relayBytes hands out the buffer it is about to reuse
// (the same rule spliceConnStream documents), so retaining the slice would make
// a tap show bytes from a later chunk.
func (t *forwardTap) offer(seq uint64, dir protocol.ForwardTapDirection, data []byte) {
	if t.closed.Load() || !t.wants(dir) || len(data) == 0 {
		return
	}
	key := tapStreamKey{seq: seq, dir: dir}

	t.mu.Lock()
	st := t.state(key)
	offset := st.offset
	// The offset advances by what CROSSED, not by what is kept: a truncated
	// record must not make the stream look shorter than it is.
	st.offset += uint64(len(data))
	t.mu.Unlock()

	keep := data
	var cut uint32
	if t.maxRecordBytes > 0 && uint32(len(keep)) > t.maxRecordBytes {
		cut = uint32(len(keep)) - t.maxRecordBytes
		keep = keep[:t.maxRecordBytes]
	}
	payload := make([]byte, len(keep))
	copy(payload, keep)

	d := protocol.ForwardTapData{
		ConnSeq:        seq,
		Direction:      dir,
		StreamOffset:   offset,
		TruncatedBytes: cut,
	}
	d.SetData(payload)
	rec := &protocol.ForwardTapRecord{
		Kind:   protocol.ForwardTapRecordKind_Data,
		UnixMs: uint64(time.Now().UnixMilli()),
	}
	rec.SetData(d)

	select {
	case t.ch <- rec:
	default:
		t.mu.Lock()
		t.state(key).missed += uint64(len(data))
		t.mu.Unlock()
	}
}

// emit queues a record that carries no payload (conn_open / conn_close /
// forward_closed). Same non-blocking rule; a dropped bracket costs the reader a
// delimiter, not data, so it is not counted as missed bytes.
func (t *forwardTap) emit(rec *protocol.ForwardTapRecord) {
	if t.closed.Load() {
		return
	}
	select {
	case t.ch <- rec:
	default:
	}
}

// takeMissed returns and clears the missed count for one stream.
func (t *forwardTap) takeMissed(key tapStreamKey) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.streams[key]
	if st == nil {
		return 0
	}
	n := st.missed
	st.missed = 0
	return n
}

// recordKey is the stream a record belongs to. Records without a direction of
// their own (conn_open, conn_close) key to to_target arbitrarily — they carry
// no bytes, so they never hold a missed count.
func recordKey(rec *protocol.ForwardTapRecord) tapStreamKey {
	switch rec.Kind {
	case protocol.ForwardTapRecordKind_Data:
		if d := rec.Data(); d != nil {
			return tapStreamKey{seq: d.ConnSeq, dir: d.Direction}
		}
	case protocol.ForwardTapRecordKind_Gap:
		if g := rec.Gap(); g != nil {
			return tapStreamKey{seq: g.ConnSeq, dir: g.Direction}
		}
	case protocol.ForwardTapRecordKind_ConnOpen:
		if o := rec.ConnOpen(); o != nil {
			return tapStreamKey{seq: o.ConnSeq}
		}
	case protocol.ForwardTapRecordKind_ConnClose:
		if c := rec.ConnClose(); c != nil {
			return tapStreamKey{seq: c.ConnSeq}
		}
	}
	return tapStreamKey{}
}

// run drains the queue onto the sink until ctx ends or the sink fails. Before
// each record it flushes that stream's accumulated overflow as a gap, so the
// reader learns what it missed at the point it missed it rather than finding a
// silent discontinuity in the offsets.
func (t *forwardTap) run(ctx context.Context) {
	defer t.closed.Store(true)
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-t.ch:
			key := recordKey(rec)
			if missed := t.takeMissed(key); missed > 0 {
				gap := &protocol.ForwardTapRecord{
					Kind:   protocol.ForwardTapRecordKind_Gap,
					UnixMs: uint64(time.Now().UnixMilli()),
				}
				gap.SetGap(protocol.ForwardTapGap{
					ConnSeq:      key.seq,
					Direction:    key.dir,
					DroppedBytes: missed,
				})
				if err := t.sink.send(gap); err != nil {
					slog.Debug("forward tap: sink ended", "err", err)
					return
				}
			}
			if err := t.sink.send(rec); err != nil {
				slog.Debug("forward tap: sink ended", "err", err)
				return
			}
		}
	}
}

// --- registration side ---

func (pf *portForward) addTap(t *forwardTap) {
	pf.tapMu.Lock()
	pf.taps = append(pf.taps, t)
	pf.tapMu.Unlock()
}

func (pf *portForward) removeTap(t *forwardTap) {
	pf.tapMu.Lock()
	for i, cur := range pf.taps {
		if cur == t {
			pf.taps = append(pf.taps[:i], pf.taps[i+1:]...)
			break
		}
	}
	pf.tapMu.Unlock()
	t.closed.Store(true)
}

// tapCount is what the listing reports as taps=N. Capped at the field's width
// rather than wrapping: a wrapped count would read as "nobody is watching".
func (pf *portForward) tapCount() uint16 {
	if pf == nil {
		return 0
	}
	pf.tapMu.Lock()
	defer pf.tapMu.Unlock()
	if len(pf.taps) > 0xffff {
		return 0xffff
	}
	return uint16(len(pf.taps))
}

func (pf *portForward) eachTap(fn func(*forwardTap)) {
	if pf == nil {
		return
	}
	pf.tapMu.Lock()
	taps := make([]*forwardTap, len(pf.taps))
	copy(taps, pf.taps)
	pf.tapMu.Unlock()
	for _, t := range taps {
		fn(t)
	}
}

func nowTapRecord(kind protocol.ForwardTapRecordKind) *protocol.ForwardTapRecord {
	return &protocol.ForwardTapRecord{Kind: kind, UnixMs: uint64(time.Now().UnixMilli())}
}

// tapConnOpen tells every tap that a connection was accepted, so a multiplexed
// dump can bracket its records instead of interleaving unlabelled bytes.
func (pf *portForward) tapConnOpen(seq uint64, host string, port uint16) {
	pf.eachTap(func(t *forwardTap) {
		rec := nowTapRecord(protocol.ForwardTapRecordKind_ConnOpen)
		o := protocol.ForwardTapConnOpen{ConnSeq: seq, TargetPort: port}
		o.SetTargetHost([]byte(host))
		rec.SetConnOpen(o)
		t.emit(rec)
	})
}

func (pf *portForward) tapConnClose(seq uint64, toTarget, fromTarget uint64) {
	pf.eachTap(func(t *forwardTap) {
		rec := nowTapRecord(protocol.ForwardTapRecordKind_ConnClose)
		rec.SetConnClose(protocol.ForwardTapConnClose{
			ConnSeq:         seq,
			BytesToTarget:   toTarget,
			BytesFromTarget: fromTarget,
		})
		t.emit(rec)
	})
}

// closeTaps tells every tap why the forward ended. Without it a tapper sees a
// bare EOF, which is indistinguishable from its own connection dropping — the
// registration's owner already gets this fact through its control stream, and a
// tapper is not necessarily the owner.
func (pf *portForward) closeTaps(reason protocol.PortForwardCloseReason) {
	pf.eachTap(func(t *forwardTap) {
		rec := nowTapRecord(protocol.ForwardTapRecordKind_ForwardClosed)
		rec.SetForwardClosed(protocol.ForwardTapForwardClosed{Reason: reason})
		t.emit(rec)
	})
}
