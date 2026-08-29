package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// chanSink stands in for the stream a real tap writes to.
type chanSink struct {
	out chan *protocol.ForwardTapRecord
	err error
}

func (s *chanSink) send(rec *protocol.ForwardTapRecord) error {
	if s.err != nil {
		return s.err
	}
	s.out <- rec
	return nil
}

func newTestTap(t *testing.T, pf *portForward, filter protocol.ForwardTapFilter, maxBytes uint32) (*forwardTap, chan *protocol.ForwardTapRecord) {
	t.Helper()
	sink := &chanSink{out: make(chan *protocol.ForwardTapRecord, 64)}
	tap := newForwardTap(sink, filter, maxBytes)
	pf.addTap(tap)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tap.run(ctx)
	return tap, sink.out
}

func drain(t *testing.T, ch chan *protocol.ForwardTapRecord, n int) []*protocol.ForwardTapRecord {
	t.Helper()
	out := make([]*protocol.ForwardTapRecord, 0, n)
	for len(out) < n {
		select {
		case rec := <-ch:
			out = append(out, rec)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d of %d records", len(out), n)
		}
	}
	return out
}

func TestTapReceivesBothDirections(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ping"))
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("pong"))

	got := drain(t, recs, 2)
	if string(got[0].Data().Data) != "ping" || got[0].Data().Direction != protocol.ForwardTapDirection_ToTarget {
		t.Fatalf("first record: %+v", got[0].Data())
	}
	if string(got[1].Data().Data) != "pong" || got[1].Data().Direction != protocol.ForwardTapDirection_FromTarget {
		t.Fatalf("second record: %+v", got[1].Data())
	}
}

// Each (conn, direction) is its own stream with its own offset space.
func TestTapStreamOffsetIsPerDirection(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("abcd"))
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("xy"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ef"))

	got := drain(t, recs, 3)
	if got[0].Data().StreamOffset != 0 {
		t.Fatalf("first to_target offset %d", got[0].Data().StreamOffset)
	}
	if got[1].Data().StreamOffset != 0 {
		t.Fatalf("from_target has its own offset space, got %d", got[1].Data().StreamOffset)
	}
	if got[2].Data().StreamOffset != 4 {
		t.Fatalf("second to_target offset %d, want 4", got[2].Data().StreamOffset)
	}
}

func TestTapFilterDropsTheOtherDirection(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_ToTarget, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_FromTarget, []byte("ignored"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("kept"))

	got := drain(t, recs, 1)
	if string(got[0].Data().Data) != "kept" {
		t.Fatalf("filter let the wrong direction through: %q", got[0].Data().Data)
	}
}

func TestTapTruncatesAndKeepsTheOffsetHonest(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 4)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("0123456789"))
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("ab"))

	got := drain(t, recs, 2)
	if string(got[0].Data().Data) != "0123" || got[0].Data().TruncatedBytes != 6 {
		t.Fatalf("truncation: %q cut=%d", got[0].Data().Data, got[0].Data().TruncatedBytes)
	}
	if got[1].Data().StreamOffset != 10 {
		t.Fatalf("offset must count the CUT bytes too, got %d want 10", got[1].Data().StreamOffset)
	}
}

func TestTapBracketsConnectionsAndReportsTheClose(t *testing.T) {
	pf := newCounterForward()
	pf.targetHost, pf.targetPort = "localhost", 3000
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	pf.tapConnOpen(seq, "localhost", 3000)
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("12345"))
	to, from := pf.connBytes(seq)
	pf.tapConnClose(seq, to, from)

	got := drain(t, recs, 3)
	if got[0].Kind != protocol.ForwardTapRecordKind_ConnOpen {
		t.Fatalf("first record kind %v", got[0].Kind)
	}
	if string(got[0].ConnOpen().TargetHost) != "localhost" || got[0].ConnOpen().TargetPort != 3000 {
		t.Fatalf("conn_open target: %+v", got[0].ConnOpen())
	}
	if got[2].Kind != protocol.ForwardTapRecordKind_ConnClose || got[2].ConnClose().BytesToTarget != 5 {
		t.Fatalf("conn_close must carry that connection's own totals: %+v", got[2].ConnClose())
	}
}

func TestTapLearnsWhyTheForwardEnded(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	pf.closeTaps(protocol.PortForwardCloseReason_Killed)
	got := drain(t, recs, 1)
	if got[0].Kind != protocol.ForwardTapRecordKind_ForwardClosed {
		t.Fatalf("kind %v", got[0].Kind)
	}
	if got[0].ForwardClosed().Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("reason %v", got[0].ForwardClosed().Reason)
	}
}

// The load-bearing one: a tap that cannot keep up must neither block the relay
// nor disappear. It gets a gap record and stays attached.
func TestSlowTapGetsAGapAndSurvives(t *testing.T) {
	pf := newCounterForward()
	sink := &chanSink{out: make(chan *protocol.ForwardTapRecord, 1)}
	tap := newForwardTap(sink, protocol.ForwardTapFilter_Both, 0)
	pf.addTap(tap)
	defer pf.removeTap(tap)
	// Deliberately no run() goroutine: nothing drains the queue.

	seq := pf.openConn()
	done := make(chan struct{})
	go func() {
		for i := 0; i < forwardTapQueueDepth*4; i++ {
			pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("0123456789"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observe blocked on a tap that is not draining — the relay would stall")
	}
	if pf.tapCount() != 1 {
		t.Fatal("a slow tap was dropped; it must survive with a gap instead")
	}
	if tap.missedBytes() == 0 {
		t.Fatal("overflow was not accounted as missed bytes")
	}

	// Once it drains again, the next record it receives is preceded by a gap
	// naming what it lost.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink.out = make(chan *protocol.ForwardTapRecord, 1024)
	go tap.run(ctx)

	var sawGap bool
	deadline := time.After(3 * time.Second)
	for !sawGap {
		select {
		case rec := <-sink.out:
			if rec.Kind == protocol.ForwardTapRecordKind_Gap {
				if rec.Gap().DroppedBytes == 0 {
					t.Fatal("gap record reports zero dropped bytes")
				}
				sawGap = true
			}
		case <-deadline:
			t.Fatal("no gap record after an overflow")
		}
	}
}

// Two taps on one forward both receive, and the count the listing shows keeps
// up with them.
func TestTwoTapsBothReceiveAndAreCounted(t *testing.T) {
	pf := newCounterForward()
	tapA, recsA := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	tapB, recsB := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	if pf.tapCount() != 2 {
		t.Fatalf("tapCount = %d, want 2", pf.tapCount())
	}
	seq := pf.openConn()
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, []byte("x"))
	if string(drain(t, recsA, 1)[0].Data().Data) != "x" {
		t.Fatal("tap A missed the byte")
	}
	if string(drain(t, recsB, 1)[0].Data().Data) != "x" {
		t.Fatal("tap B missed the byte")
	}
	pf.removeTap(tapA)
	if pf.tapCount() != 1 {
		t.Fatalf("tapCount after one removal = %d", pf.tapCount())
	}
	pf.removeTap(tapB)
}

// The relay hands out a buffer it reuses; a tap that retained the slice would
// show bytes from a later chunk.
func TestTapCopiesThePayload(t *testing.T) {
	pf := newCounterForward()
	tap, recs := newTestTap(t, pf, protocol.ForwardTapFilter_Both, 0)
	defer pf.removeTap(tap)

	seq := pf.openConn()
	buf := []byte("first")
	pf.observe(seq, protocol.ForwardTapDirection_ToTarget, buf)
	copy(buf, []byte("SECON"))

	got := drain(t, recs, 1)
	if string(got[0].Data().Data) != "first" {
		t.Fatalf("tap retained the relay's buffer: %q", got[0].Data().Data)
	}
}

// A tapper that goes away must be reaped even if nothing is flowing. Before
// this, the tap was only dropped when the next record failed to send, so on a
// quiet forward it stayed attached forever and `taps=N` counted a reader that
// had left.
func TestTapIsReapedWhenTheReaderGoesAwayOnAQuietForward(t *testing.T) {
	pf := newCounterForward()
	sink := &chanSink{out: make(chan *protocol.ForwardTapRecord, 8)}
	tap := newForwardTap(sink, protocol.ForwardTapFilter_Both, 0)
	pf.addTap(tap)

	ctx, cancel := context.WithCancel(context.Background())
	go tap.run(ctx)
	go func() {
		defer cancel()
		defer pf.removeTap(tap)
		<-ctx.Done()
	}()

	if pf.tapCount() != 1 {
		t.Fatalf("tapCount = %d before the reader leaves", pf.tapCount())
	}
	// The reader leaves. No bytes cross the forward at any point.
	cancel()

	deadline := time.After(2 * time.Second)
	for pf.tapCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("tap still attached after its reader left a quiet forward")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
