package server

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// forwardTapQueueDepth is recordTapQueueDepth under the name the forward tests
// were written against.
const forwardTapQueueDepth = recordTapQueueDepth

// forwardTapSink is where a forward tap's records go.
type forwardTapSink = recordSink[*protocol.ForwardTapRecord]

// tapStreamKey identifies one byte stream inside a forward: a connection and a
// direction. Missed-byte counts are per key, because the two directions of one
// connection are two independent streams.
type tapStreamKey struct {
	seq uint64
	dir protocol.ForwardTapDirection
}

// forwardTap is one reader attached to one forward.
type forwardTap struct {
	filter         protocol.ForwardTapFilter
	maxRecordBytes uint32
	q              *recordTap[tapStreamKey, *protocol.ForwardTapRecord]
}

func newForwardTap(sink forwardTapSink, filter protocol.ForwardTapFilter, maxRecordBytes uint32) *forwardTap {
	return &forwardTap{
		filter:         filter,
		maxRecordBytes: maxRecordBytes,
		q:              newRecordTap[tapStreamKey, *protocol.ForwardTapRecord](sink, recordKey, forwardGapRecord),
	}
}

func forwardGapRecord(key tapStreamKey, missed uint64) *protocol.ForwardTapRecord {
	gap := nowTapRecord(protocol.ForwardTapRecordKind_Gap)
	gap.SetGap(protocol.ForwardTapGap{ConnSeq: key.seq, Direction: key.dir, DroppedBytes: missed})
	return gap
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

func (t *forwardTap) missedBytes() uint64     { return t.q.missedBytes() }
func (t *forwardTap) run(ctx context.Context) { t.q.run(ctx) }

// offer is called from the relay goroutine, once per chunk. It never blocks and
// never returns an error the relay would have to handle: a tap that cannot keep
// up loses bytes and is told so, rather than slowing the forward down.
//
// offset is where data starts in its (connection, direction) stream, counted by
// the caller from the connection's first byte — not by the tap, which would
// start at 0 whenever it was opened mid-connection (D12 of the exec tap spec).
//
// The payload is COPIED. relayBytes hands out the buffer it is about to reuse
// (the same rule spliceConnStream documents), so retaining the slice would make
// a tap show bytes from a later chunk.
func (t *forwardTap) offer(seq uint64, dir protocol.ForwardTapDirection, offset uint64, data []byte) {
	if t.q.isClosed() || !t.wants(dir) || len(data) == 0 {
		return
	}
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
	rec := nowTapRecord(protocol.ForwardTapRecordKind_Data)
	rec.SetData(d)
	t.q.push(tapStreamKey{seq: seq, dir: dir}, rec, len(data))
}

// emit queues a record that carries no payload (conn_open / conn_close).
func (t *forwardTap) emit(rec *protocol.ForwardTapRecord) {
	t.q.push(recordKey(rec), rec, 0)
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
	t.q.close()
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
//
// forward_closed is the last record: finish delivers it after everything queued
// and ends the tap, so the handler closes the reader's stream.
func (pf *portForward) closeTaps(reason protocol.PortForwardCloseReason) {
	pf.eachTap(func(t *forwardTap) {
		rec := nowTapRecord(protocol.ForwardTapRecordKind_ForwardClosed)
		rec.SetForwardClosed(protocol.ForwardTapForwardClosed{Reason: reason})
		t.q.finish(rec)
	})
}
