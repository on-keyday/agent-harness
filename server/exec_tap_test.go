package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type execChanSink struct {
	out chan *protocol.ExecTapRecord
}

func (s *execChanSink) send(rec *protocol.ExecTapRecord) error { s.out <- rec; return nil }

func newTestExecTap(t *testing.T, e *execRun, filter protocol.ExecTapFilter, maxBytes uint32) (*execTap, chan *protocol.ExecTapRecord) {
	t.Helper()
	sink := &execChanSink{out: make(chan *protocol.ExecTapRecord, 64)}
	tap := newExecTap(sink, filter, maxBytes)
	e.addTap(tap)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tap.run(ctx)
	return tap, sink.out
}

func drainExec(t *testing.T, ch chan *protocol.ExecTapRecord, n int) []*protocol.ExecTapRecord {
	t.Helper()
	out := make([]*protocol.ExecTapRecord, 0, n)
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

func registerTapTestExec(t *testing.T, h *TaskHandler, taskHex string) *execRun {
	t.Helper()
	e := &execRun{taskIDHex: taskHex, argv: []string{"cat"}, control: newRecordingBidiStream(1)}
	h.execs().add(e)
	return e
}

func TestOpenExecTapNeedsExecTapCap(t *testing.T) {
	want, gated := requiredCap[protocol.TaskControlKind_OpenExecTap]
	if !gated {
		t.Fatal("open_exec_tap is not in requiredCap: reading an exec's payload would be ungated")
	}
	if want != protocol.Capability_ExecTap {
		t.Fatalf("gated on %v, want exec_tap", want)
	}
}

// Unknown, zero, invisible and out-of-scope ids all answer no_such_exec and
// leave no stream and no tap behind.
func TestOpenExecTapRefusalsAreOneAnswer(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	e := registerTapTestExec(t, h, u) // owned by a task outside c's subtree
	cid := bindPrincipal(t, h, c)

	for name, id := range map[string]uint64{"unknown": 999, "zero": 0, "invisible": e.execID} {
		conn := tapConn("ws:127.0.0.1:9980-1")
		got := h.handleOpenExecTap(conn, &protocol.OpenExecTapRequest{ExecId: id}, cid)
		if got.Status != protocol.OpenExecTapStatus_NoSuchExec || got.StreamId != 0 {
			t.Fatalf("%s: status %v stream %d", name, got.Status, got.StreamId)
		}
	}
	setScope(t, h, c, Scope{
		Base:    protocol.ScopeBase_None,
		VisBase: protocol.ScopeBase_Global, VisBasePresent: true,
	})
	conn := tapConn("ws:127.0.0.1:9981-1")
	if got := h.handleOpenExecTap(conn, &protocol.OpenExecTapRequest{ExecId: e.execID}, cid); got.Status != protocol.OpenExecTapStatus_NoSuchExec {
		t.Fatalf("visible but out of action scope: %v", got.Status)
	}
	if e.tapCount() != 0 {
		t.Fatal("a refused tap was attached")
	}
}

func TestOpenExecTapAttachesAndIsCountedOnTheListing(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	e := registerTapTestExec(t, h, p)
	conn := tapConn("ws:127.0.0.1:9982-1")
	// A stream that stays open, as a real tapper's does (see the forward
	// sibling's comment on why noopBidiStream would race this test).
	conn.nextBidi = newRecordingBidiStream(78)

	got := h.handleOpenExecTap(conn, &protocol.OpenExecTapRequest{ExecId: e.execID}, conn.ConnectionID().String())
	if got.Status != protocol.OpenExecTapStatus_Ok {
		t.Fatalf("status %v", got.Status)
	}
	if e.tapCount() != 1 || execRunInfo(e).Taps != 1 {
		t.Fatal("taps= must report the open tap: an exec must not be watchable invisibly")
	}
}

// Review Focus 1: every way an exec ends tells its taps how, and then the tap
// stream ends — so `exec tap` exits.
func TestEveryRemovalPathEndsTheTapsWithItsOutcome(t *testing.T) {
	cases := []struct {
		name   string
		remove func(h *TaskHandler, e *execRun)
		kind   protocol.ExecEventKind
		code   int32
	}{
		{"finished", func(h *TaskHandler, e *execRun) {
			h.onExecRunFinished(&protocol.ExecRunFinished{ExecId: e.execID, ExitCode: 3, Kind: protocol.ExecEventKind_Exited})
		}, protocol.ExecEventKind_Exited, 3},
		{"killed", func(h *TaskHandler, e *execRun) {
			h.handleExecRunKill("", &protocol.ExecRunKillRequest{ExecId: e.execID})
		}, protocol.ExecEventKind_Killed, -1},
		{"client gone", func(h *TaskHandler, e *execRun) { h.DropExecRunsForConn(e.clientCID) },
			protocol.ExecEventKind_Killed, -1},
		{"never started", func(h *TaskHandler, e *execRun) {
			h.removeExec(e.execID, protocol.ExecEventKind_Failed, -1)
		}, protocol.ExecEventKind_Failed, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Tasks is needed even with no tasks in it: handleExecRunKill's
			// authorize builds the creator index from the store before it
			// notices the caller is the operator.
			h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
			e := &execRun{taskIDHex: "aaaa", clientCID: "c-" + tc.name, control: newRecordingBidiStream(1)}
			h.execs().add(e)
			sink := &execChanSink{out: make(chan *protocol.ExecTapRecord, 8)}
			tap := newExecTap(sink, protocol.ExecTapFilter_All, 0)
			e.addTap(tap)
			done := make(chan struct{})
			go func() { tap.run(context.Background()); close(done) }()

			tc.remove(h, e)

			rec := drainExec(t, sink.out, 1)[0]
			ended := rec.ExecEnded()
			if rec.Kind != protocol.ExecTapRecordKind_ExecEnded || ended == nil {
				t.Fatalf("kind %v", rec.Kind)
			}
			if ended.Kind != tc.kind || ended.ExitCode != tc.code {
				t.Fatalf("outcome %v/%d, want %v/%d", ended.Kind, ended.ExitCode, tc.kind, tc.code)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the tap kept running after exec_ended; `exec tap` would never exit")
			}
		})
	}
}

// Review Focus 1, at the stream: the handler closes the tapper's stream once
// the exec ends.
func TestExecTapStreamEndsWhenTheExecEnds(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	e := registerTapTestExec(t, h, p)
	conn := tapConn("ws:127.0.0.1:9983-1")
	st := newRecordingBidiStream(79)
	conn.nextBidi = st
	if got := h.handleOpenExecTap(conn, &protocol.OpenExecTapRequest{ExecId: e.execID}, conn.ConnectionID().String()); got.Status != protocol.OpenExecTapStatus_Ok {
		t.Fatalf("status %v", got.Status)
	}
	h.onExecRunFinished(&protocol.ExecRunFinished{ExecId: e.execID, Kind: protocol.ExecEventKind_Exited})

	deadline := time.After(2 * time.Second)
	for !st.Ended() {
		select {
		case <-deadline:
			t.Fatal("the tap stream was not closed after the exec ended")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Review Focus 2: a reader leaving a quiet exec is reaped.
func TestExecTapIsReapedWhenTheReaderLeavesAQuietExec(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	e := registerTapTestExec(t, h, p)
	conn := tapConn("ws:127.0.0.1:9984-1")
	st := newRecordingBidiStream(80)
	conn.nextBidi = st
	h.handleOpenExecTap(conn, &protocol.OpenExecTapRequest{ExecId: e.execID}, conn.ConnectionID().String())
	if e.tapCount() != 1 {
		t.Fatal("tap not attached")
	}
	_ = st.CloseBoth() // the reader leaves; nothing crosses the exec

	deadline := time.After(2 * time.Second)
	for e.tapCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("tap still counted after its reader left a quiet exec")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Review Focus 3: a tap attached to an exec that has already ended is finished
// at once with the stored outcome, rather than waiting on a registration that
// will never produce anything.
func TestExecTapOpenedAfterTheEndIsFinishedImmediately(t *testing.T) {
	e := &execRun{taskIDHex: "aaaa"}
	e.endTaps(protocol.ExecEventKind_Exited, 0)
	sink := &execChanSink{out: make(chan *protocol.ExecTapRecord, 4)}
	tap := newExecTap(sink, protocol.ExecTapFilter_All, 0)
	e.addTap(tap)
	done := make(chan struct{})
	go func() { tap.run(context.Background()); close(done) }()
	if rec := drainExec(t, sink.out, 1)[0]; rec.Kind != protocol.ExecTapRecordKind_ExecEnded {
		t.Fatalf("kind %v", rec.Kind)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a tap on an ended exec kept running")
	}
}
