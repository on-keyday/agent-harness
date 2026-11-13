package server

import (
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// ptyRecordSink is where a session's record goes: *RunWriter in the server, a
// memory sink in tests.
type ptyRecordSink interface {
	SegmentLimit() int64
	Append(protocol.RecordEntry) error
	Rotate(head []protocol.RecordEntry) error
	Flush() error
	Detach() error
	Close(now time.Time) error
}

// recordStart says how a record begins (pillar A spec, A-D22, A-D23).
type recordStart uint8

const (
	// recordFresh: the run's first record, from the PTY's first byte. No head.
	recordFresh recordStart = iota
	// recordFromRing: switched on with no record yet; the frames an attach
	// would replay come first, stamped with the start time. No head.
	recordFromRing
	// recordAgain: switched on with a record already; a new segment starting
	// with the screen as it is now.
	recordAgain
	// recordContinue: re-adoption; append where the previous server stopped.
	recordContinue
)

// recorderQueue bounds the frames waiting for the disk. A full queue blocks
// the runner pump: a record that drops or reorders frames cannot be played,
// and the bound keeps memory finite (plan Review Focus 5).
const recorderQueue = 1024

type recItem struct {
	entry  protocol.RecordEntry
	rotate bool
	head   []protocol.RecordEntry
}

// sessionRecorder moves a session's entries to its sink on one goroutine, so
// disk I/O never runs on the runner pump.
type sessionRecorder struct {
	sink     ptyRecordSink
	ch       chan recItem
	done     chan struct{}
	segBytes int64 // bytes enqueued into the current segment (pump side)
	limit    int64
	close    bool // set before ch closes: Close rather than Detach
}

func newSessionRecorder(sink ptyRecordSink) *sessionRecorder {
	r := &sessionRecorder{sink: sink, ch: make(chan recItem, recorderQueue), done: make(chan struct{}), limit: sink.SegmentLimit()}
	go r.run()
	return r
}

func (r *sessionRecorder) run() {
	defer close(r.done)
	for it := range r.ch {
		if it.rotate {
			_ = r.sink.Rotate(it.head)
		} else {
			_ = r.sink.Append(it.entry)
		}
		if len(r.ch) == 0 {
			_ = r.sink.Flush()
		}
	}
	if r.close {
		_ = r.sink.Close(time.Now())
	} else {
		_ = r.sink.Detach()
	}
}

// add queues one entry and reports whether the segment is now full.
func (r *sessionRecorder) add(e protocol.RecordEntry) bool {
	r.ch <- recItem{entry: e}
	r.segBytes += int64(len(e.Payload)) + recordEntryOverhead
	return r.segBytes >= r.limit
}

func (r *sessionRecorder) rotate(head []protocol.RecordEntry) {
	r.ch <- recItem{rotate: true, head: head}
	r.segBytes = 0
	for _, e := range head {
		r.segBytes += int64(len(e.Payload)) + recordEntryOverhead
	}
}

// finish stops the recorder after everything queued has reached the sink.
func (r *sessionRecorder) finish(closeRecord bool) {
	r.close = closeRecord
	close(r.ch)
	<-r.done
}

// recordEntryOverhead is a RecordEntry's fixed part: ts_ns, frame_type,
// payload_len.
const recordEntryOverhead = 8 + 1 + 4

// frameEntry turns one wire frame (header + payload) into a record entry.
func frameEntry(ts int64, fb []byte) (protocol.RecordEntry, bool) {
	if len(fb) < frame.FrameHeaderSize {
		return protocol.RecordEntry{}, false
	}
	h := frameHeaderAt(fb)
	end := frame.FrameHeaderSize + int(h.Len)
	if end > len(fb) {
		return protocol.RecordEntry{}, false
	}
	e := protocol.RecordEntry{TsNs: uint64(ts), FrameType: uint8(h.Type)}
	e.SetPayload(append([]byte(nil), fb[frame.FrameHeaderSize:end]...))
	return e, true
}

func synthEntry(ts int64, payload []byte) protocol.RecordEntry {
	e := protocol.RecordEntry{TsNs: uint64(ts), FrameType: uint8(frame.FrameType_Synth)}
	e.SetPayload(payload)
	return e
}

// recState is the mux's recording state. recMu serialises recordFrame's
// whole body against StartRecording, so a ring seed and the live frames
// after it neither overlap nor leave a gap.
type recState struct {
	recMu    sync.Mutex
	recorder *sessionRecorder
}

// StartRecording begins recording the session into sink. A second call while
// recording is a no-op.
func (m *SessionMux) StartRecording(sink ptyRecordSink, how recordStart) error {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	if m.recorder != nil {
		return nil
	}
	r := newSessionRecorder(sink)
	now := time.Now().UnixNano()
	switch how {
	case recordFromRing:
		data := m.replaySnapshot()
		for off := 0; off+frame.FrameHeaderSize <= len(data); {
			end := off + frame.FrameHeaderSize + int(frameHeaderAt(data[off:]).Len)
			if end > len(data) {
				break
			}
			if e, ok := frameEntry(now, data[off:end]); ok {
				r.add(e)
			}
			off = end
		}
	case recordAgain:
		r.rotate(m.segmentHead(now))
	}
	m.recorder = r
	return nil
}

// StopRecording is a switch-off: the record's current segment closes and the
// record stays.
func (m *SessionMux) StopRecording() { m.endRecording(true) }

// detachRecording is the mux stopping: the writer is released with its
// segment open, for a re-adopting server to continue.
func (m *SessionMux) detachRecording() { m.endRecording(false) }

func (m *SessionMux) endRecording(closeRecord bool) {
	m.recMu.Lock()
	r := m.recorder
	m.recorder = nil
	m.recMu.Unlock()
	if r != nil {
		r.finish(closeRecord)
	}
}

// Recording reports whether the session is being recorded.
func (m *SessionMux) Recording() bool {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	if m.recorder == nil {
		return false
	}
	if w, ok := m.recorder.sink.(*RunWriter); ok && w.gone.Load() {
		return false
	}
	return true
}

// segmentHead is what a segment after a record's first begins with: the size,
// the terminal modes and a repaint of the screen as it is now — what an attach
// ends its replay with — so the segment plays alone (A-D23).
func (m *SessionMux) segmentHead(ts int64) []protocol.RecordEntry {
	var head []protocol.RecordEntry
	m.mu.Lock()
	size := append([]byte(nil), m.lastWinSize...)
	pre := m.modes.Preamble()
	m.mu.Unlock()
	if e, ok := frameEntry(ts, size); ok {
		head = append(head, e)
	}
	if len(pre) > 0 {
		head = append(head, synthEntry(ts, pre))
	}
	if rp := m.screenRepaint(); len(rp) > 0 {
		head = append(head, synthEntry(ts, rp))
	}
	return head
}

// recordOutputLocked queues one frame the session showed, rotating when the
// segment is full. The caller holds recMu.
func (m *SessionMux) recordOutputLocked(fb []byte) {
	r := m.recorder
	if r == nil {
		return
	}
	now := time.Now().UnixNano()
	e, ok := frameEntry(now, fb)
	if !ok {
		return
	}
	if r.add(e) {
		r.rotate(m.segmentHead(now))
	}
}
