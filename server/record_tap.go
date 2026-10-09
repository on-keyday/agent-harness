package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/on-keyday/objtrsf/trsf"
)

// recordTapQueueDepth bounds one tap's backlog. Same shape as the session
// mux's viewer queue (session_mux.go, viewerQueueDepth) and for the same
// reason: the producer is a relay that must never wait.
//
// It differs in what overflow DOES. SessionMux drops the viewer; a tap keeps
// its consumer and reports a gap instead. A session viewer that is dropped can
// reattach and replay from the ring — a tap has no ring by design, and a tap
// that vanishes mid-investigation reads as "the subject ended", which is a
// false statement about the thing being investigated.
const recordTapQueueDepth = 256

// recordSink is where a tap's records go. The stream implementation is the
// real one; tests substitute a channel.
type recordSink[R any] interface {
	send(rec R) error
}

// recordTap is the queue every tap kind shares: bounded, non-blocking on the
// producer side, per-stream accounting of what overflowed, and a gap record
// flushed before that stream's next record. K names a stream inside the tapped
// subject (a forward's connection+direction, an exec's channel); R is the wire
// record.
//
// It exists so the gap logic lives once. forward tap and exec tap each keep
// their own filter and truncation, because those differ in the type they
// filter on.
type recordTap[K comparable, R any] struct {
	ch     chan R
	sink   recordSink[R]
	keyOf  func(R) K
	gapFor func(K, uint64) R

	mu     sync.Mutex
	missed map[K]uint64

	finOnce sync.Once
	finCh   chan struct{}
	final   R

	closed atomic.Bool
}

func newRecordTap[K comparable, R any](sink recordSink[R], keyOf func(R) K, gapFor func(K, uint64) R) *recordTap[K, R] {
	return &recordTap[K, R]{
		ch:     make(chan R, recordTapQueueDepth),
		sink:   sink,
		keyOf:  keyOf,
		gapFor: gapFor,
		missed: map[K]uint64{},
		finCh:  make(chan struct{}),
	}
}

func (t *recordTap[K, R]) close()         { t.closed.Store(true) }
func (t *recordTap[K, R]) isClosed() bool { return t.closed.Load() }

// push queues rec, which carries payloadBytes bytes of stream key. It never
// blocks. When the queue is full a payload record's bytes are counted as
// missed for its stream; a record with no payload (a bracket) costs the reader
// a delimiter, not data, and is dropped without a count.
func (t *recordTap[K, R]) push(key K, rec R, payloadBytes int) {
	if t.closed.Load() {
		return
	}
	select {
	case t.ch <- rec:
	default:
		if payloadBytes > 0 {
			t.mu.Lock()
			t.missed[key] += uint64(payloadBytes)
			t.mu.Unlock()
		}
	}
}

// finish ends the tap after everything already queued: run delivers the queue,
// any outstanding gaps, then final, and returns. It does not go through the
// queue, so a full queue cannot drop the end — the record a reader needs most.
// The first call wins.
func (t *recordTap[K, R]) finish(final R) {
	t.finOnce.Do(func() {
		t.final = final
		close(t.finCh)
	})
}

// missedBytes is the total this tap has failed to keep up with, across every
// stream. Test and diagnostic use; the gap records carry the per-stream halves.
func (t *recordTap[K, R]) missedBytes() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n uint64
	for _, m := range t.missed {
		n += m
	}
	return n
}

func (t *recordTap[K, R]) takeMissed(key K) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.missed[key]
	delete(t.missed, key)
	return n
}

// deliver sends rec, preceded by a gap for its stream when bytes were missed
// there. Reports false when the sink failed and the tap must stop.
func (t *recordTap[K, R]) deliver(rec R) bool {
	key := t.keyOf(rec)
	if missed := t.takeMissed(key); missed > 0 {
		if err := t.sink.send(t.gapFor(key, missed)); err != nil {
			slog.Debug("tap: sink ended", "err", err)
			return false
		}
	}
	if err := t.sink.send(rec); err != nil {
		slog.Debug("tap: sink ended", "err", err)
		return false
	}
	return true
}

// run drains the queue onto the sink until ctx ends, the sink fails, or finish
// was called and everything before the final record has been delivered.
func (t *recordTap[K, R]) run(ctx context.Context) {
	defer t.closed.Store(true)
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-t.ch:
			if !t.deliver(rec) {
				return
			}
		case <-t.finCh:
		drain:
			for {
				select {
				case rec := <-t.ch:
					if !t.deliver(rec) {
						return
					}
				default:
					break drain
				}
			}
			t.mu.Lock()
			pending := make(map[K]uint64, len(t.missed))
			for k, n := range t.missed {
				pending[k] = n
			}
			t.missed = map[K]uint64{}
			t.mu.Unlock()
			for k, n := range pending {
				if err := t.sink.send(t.gapFor(k, n)); err != nil {
					return
				}
			}
			_ = t.sink.send(t.final)
			return
		}
	}
}

// encodableRecord is a wire record a tap stream can carry.
type encodableRecord interface {
	EncodeCopy(reserved []byte) ([]byte, error)
}

// streamRecordSink writes tap records onto the client's stream.
//
// Records are CONCATENATED, with no length prefix of their own: every tap
// record is self-delimiting under its own schema, so the reader decodes one and
// keeps the remainder. A length prefix would be a wire byte the schema does not
// describe, which is the one thing this project's message format is not allowed
// to have.
type streamRecordSink[R encodableRecord] struct {
	stream trsf.BidirectionalStream
}

func (s *streamRecordSink[R]) send(rec R) error {
	buf, err := rec.EncodeCopy(nil)
	if err != nil {
		return err
	}
	return s.stream.AppendData(false, buf)
}

// serveTapStream runs one tap against the stream its reader holds, until the
// tap finishes or the reader goes away, then detaches it and closes the stream.
//
// The reader never writes on this stream, so any read returning EOF or an
// error means it is gone. Without this watcher a tap is only reaped when the
// NEXT record fails to send, so a tap closed on a quiet subject is never
// noticed and `taps=N` keeps counting a reader that left — observed exactly
// that way on forwards, with the TUI's tap view closed and the row still at
// taps=1.
func serveTapStream(stream trsf.BidirectionalStream, run func(context.Context), detach func()) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		defer detach()
		defer func() { _ = stream.CloseBoth() }()
		run(ctx)
	}()
	go func() {
		defer cancel()
		for {
			_, eof, err := stream.ReadDirect(4096)
			if eof || err != nil {
				return
			}
		}
	}()
}
