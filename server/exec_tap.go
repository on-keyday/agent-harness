package server

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// execTap is one reader attached to one exec. Its stream keys are the three
// channels: stdin, stdout and stderr are independent streams, each with its own
// offsets and its own missed-byte count.
type execTap struct {
	filter         protocol.ExecTapFilter
	maxRecordBytes uint32
	q              *recordTap[protocol.ExecTapChannel, *protocol.ExecTapRecord]
}

func newExecTap(sink recordSink[*protocol.ExecTapRecord], filter protocol.ExecTapFilter, maxRecordBytes uint32) *execTap {
	return &execTap{
		filter:         filter,
		maxRecordBytes: maxRecordBytes,
		q:              newRecordTap[protocol.ExecTapChannel, *protocol.ExecTapRecord](sink, execTapRecordKey, execTapGapRecord),
	}
}

func nowExecTapRecord(kind protocol.ExecTapRecordKind) *protocol.ExecTapRecord {
	return &protocol.ExecTapRecord{Kind: kind, UnixMs: uint64(time.Now().UnixMilli())}
}

func execTapGapRecord(ch protocol.ExecTapChannel, missed uint64) *protocol.ExecTapRecord {
	rec := nowExecTapRecord(protocol.ExecTapRecordKind_Gap)
	rec.SetGap(protocol.ExecTapGap{Channel: ch, DroppedBytes: missed})
	return rec
}

// execTapRecordKey is the channel a record belongs to. exec_ended has none and
// keys to stdin arbitrarily; it carries no bytes, so it never holds a count.
func execTapRecordKey(rec *protocol.ExecTapRecord) protocol.ExecTapChannel {
	switch rec.Kind {
	case protocol.ExecTapRecordKind_Data:
		if d := rec.Data(); d != nil {
			return d.Channel
		}
	case protocol.ExecTapRecordKind_Gap:
		if g := rec.Gap(); g != nil {
			return g.Channel
		}
	case protocol.ExecTapRecordKind_Eof:
		if f := rec.Eof(); f != nil {
			return f.Channel
		}
	}
	return protocol.ExecTapChannel_Stdin
}

func (t *execTap) wants(ch protocol.ExecTapChannel) bool {
	switch t.filter {
	case protocol.ExecTapFilter_Stdin:
		return ch == protocol.ExecTapChannel_Stdin
	case protocol.ExecTapFilter_Stdout:
		return ch == protocol.ExecTapChannel_Stdout
	case protocol.ExecTapFilter_Stderr:
		return ch == protocol.ExecTapChannel_Stderr
	}
	return true
}

func (t *execTap) run(ctx context.Context) { t.q.run(ctx) }

// offer is called from the relay goroutine, once per payload run. Same rules
// as forwardTap.offer: never blocks, copies the payload, and offset is the
// channel's count BEFORE these bytes, counted by the splice from the exec's
// first byte.
func (t *execTap) offer(ch protocol.ExecTapChannel, offset uint64, data []byte) {
	if t.q.isClosed() || !t.wants(ch) || len(data) == 0 {
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
	d := protocol.ExecTapData{Channel: ch, StreamOffset: offset, TruncatedBytes: cut}
	d.SetData(payload)
	rec := nowExecTapRecord(protocol.ExecTapRecordKind_Data)
	rec.SetData(d)
	t.q.push(ch, rec, len(data))
}

// emitEOF tells the tap a channel closed (a zero-length frame).
func (t *execTap) emitEOF(ch protocol.ExecTapChannel) {
	if !t.wants(ch) {
		return
	}
	rec := nowExecTapRecord(protocol.ExecTapRecordKind_Eof)
	rec.SetEof(protocol.ExecTapEof{Channel: ch})
	t.q.push(ch, rec, 0)
}

func execEndedRecord(kind protocol.ExecEventKind, code int32) *protocol.ExecTapRecord {
	rec := nowExecTapRecord(protocol.ExecTapRecordKind_ExecEnded)
	rec.SetExecEnded(protocol.ExecTapExecEnded{Kind: kind, ExitCode: code})
	return rec
}

// --- registration side ---

// addTap attaches t. On an exec that has already ended, t is finished at once
// with the stored outcome: the handler can look an exec up an instant before it
// is removed, and a tap attached then would otherwise wait forever.
func (e *execRun) addTap(t *execTap) {
	e.tapMu.Lock()
	defer e.tapMu.Unlock()
	if e.tapsFinished {
		t.q.finish(execEndedRecord(e.endedKind, e.endedCode))
		return
	}
	e.taps = append(e.taps, t)
}

func (e *execRun) removeTap(t *execTap) {
	e.tapMu.Lock()
	for i, cur := range e.taps {
		if cur == t {
			e.taps = append(e.taps[:i], e.taps[i+1:]...)
			break
		}
	}
	e.tapMu.Unlock()
	t.q.close()
}

// tapCount is what the listing reports as taps=N, capped at the field's width
// rather than wrapping: a wrapped count would read as "nobody is watching".
func (e *execRun) tapCount() uint16 {
	e.tapMu.Lock()
	defer e.tapMu.Unlock()
	if len(e.taps) > 0xffff {
		return 0xffff
	}
	return uint16(len(e.taps))
}

func (e *execRun) eachTap(fn func(*execTap)) {
	e.tapMu.Lock()
	taps := make([]*execTap, len(e.taps))
	copy(taps, e.taps)
	e.tapMu.Unlock()
	for _, t := range taps {
		fn(t)
	}
}

// execTapEndGrace bounds how long exec_ended waits for the runner→client relay
// after the end was reported. A kill can leave the runner's stream open; the
// tap must still end.
var execTapEndGrace = 2 * time.Second

// beginOutput marks the runner→client relay as running; outputDone marks it
// finished. spliceExecCounted brackets that relay with them.
func (e *execRun) beginOutput() {
	e.tapMu.Lock()
	e.outputPending = true
	e.tapMu.Unlock()
}

func (e *execRun) outputDone() {
	e.tapMu.Lock()
	e.outputPending = false
	e.tapMu.Unlock()
	e.maybeFinishTaps(false)
}

// endTaps records how the exec ended and ends every tap with it — once the
// runner→client relay has also finished. The runner sends the outcome on its
// control stream and the last output on the data stream, and the server sees
// the two in either order; finishing on the outcome alone dropped output that
// arrived after it, with no gap to say so. The record goes through finish, not
// the queue, so a tap that has fallen behind still learns the outcome after
// what it already holds.
func (e *execRun) endTaps(kind protocol.ExecEventKind, code int32) {
	e.tapMu.Lock()
	e.ended, e.endedKind, e.endedCode = true, kind, code
	pending := e.outputPending
	e.tapMu.Unlock()
	if pending {
		time.AfterFunc(execTapEndGrace, func() { e.maybeFinishTaps(true) })
	}
	e.maybeFinishTaps(false)
}

// maybeFinishTaps finishes every tap once the exec has ended and its output
// relay is done; force skips the second condition (the grace timer). Runs at
// most once.
func (e *execRun) maybeFinishTaps(force bool) {
	e.tapMu.Lock()
	if !e.ended || e.tapsFinished || (e.outputPending && !force) {
		e.tapMu.Unlock()
		return
	}
	e.tapsFinished = true
	kind, code := e.endedKind, e.endedCode
	taps := make([]*execTap, len(e.taps))
	copy(taps, e.taps)
	e.tapMu.Unlock()
	for _, t := range taps {
		t.q.finish(execEndedRecord(kind, code))
	}
}
