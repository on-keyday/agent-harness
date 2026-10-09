package server

import (
	"context"
	"testing"
	"time"
)

// testRec is the smallest record recordTap can carry: a stream key, a kind and
// a byte count. It keeps these tests about the queue, not about any wire type.
type testRec struct {
	key  int
	kind string // "data" | "gap" | "end"
	n    uint64
}

type testRecSink struct{ out chan testRec }

func (s *testRecSink) send(r testRec) error { s.out <- r; return nil }

func newTestRecordTap(depth int) (*recordTap[int, testRec], *testRecSink) {
	sink := &testRecSink{out: make(chan testRec, depth)}
	t := newRecordTap[int, testRec](sink,
		func(r testRec) int { return r.key },
		func(k int, missed uint64) testRec { return testRec{key: k, kind: "gap", n: missed} })
	return t, sink
}

func recvRec(t *testing.T, ch chan testRec) testRec {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a record")
		return testRec{}
	}
}

// finish delivers everything already queued, then the final record, then run
// returns. That return is what lets the handler close the reader's stream —
// the reader's only signal that the subject ended.
func TestRecordTapFinishDrainsThenEnds(t *testing.T) {
	tap, sink := newTestRecordTap(16)
	tap.push(1, testRec{key: 1, kind: "data", n: 3}, 3)
	tap.push(1, testRec{key: 1, kind: "data", n: 4}, 4)
	tap.finish(testRec{kind: "end"})

	done := make(chan struct{})
	go func() { tap.run(context.Background()); close(done) }()

	if r := recvRec(t, sink.out); r.kind != "data" || r.n != 3 {
		t.Fatalf("first: %+v", r)
	}
	if r := recvRec(t, sink.out); r.kind != "data" || r.n != 4 {
		t.Fatalf("second: %+v", r)
	}
	if r := recvRec(t, sink.out); r.kind != "end" {
		t.Fatalf("third must be the final record: %+v", r)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the final record")
	}
}

// Bytes lost to overflow with no later record on that stream must still be
// reported before the end — otherwise the reader never learns they existed.
func TestRecordTapFinishFlushesOutstandingGaps(t *testing.T) {
	tap, sink := newTestRecordTap(64)
	for i := 0; i < recordTapQueueDepth+10; i++ {
		tap.push(2, testRec{key: 2, kind: "data", n: 1}, 1)
	}
	tap.finish(testRec{kind: "end"})
	go tap.run(context.Background())

	var sawGap bool
	for {
		r := recvRec(t, sink.out)
		if r.kind == "gap" {
			if r.n == 0 {
				t.Fatal("gap with zero missed bytes")
			}
			sawGap = true
		}
		if r.kind == "end" {
			break
		}
	}
	if !sawGap {
		t.Fatal("overflowed bytes were never reported before the end")
	}
}

// finish is idempotent: the first final record wins.
func TestRecordTapFinishTwiceKeepsTheFirst(t *testing.T) {
	tap, sink := newTestRecordTap(4)
	tap.finish(testRec{kind: "end", n: 1})
	tap.finish(testRec{kind: "end", n: 2})
	go tap.run(context.Background())
	if r := recvRec(t, sink.out); r.n != 1 {
		t.Fatalf("final record = %+v, want the first", r)
	}
}
