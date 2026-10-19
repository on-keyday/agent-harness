package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// armProgress sends an await_progress from conn and returns how many messages
// conn had before, so the caller can wait for the (possibly deferred) reply.
func armProgress(t *testing.T, h *TaskHandler, conn *fakeConn, taskHex string, since uint64, sink protocol.AwaitSink, topic string) int {
	t.Helper()
	before := len(conn.Sent())
	ap := protocol.AwaitProgressRequest{TaskId: hexToTaskID(t, taskHex), SinceProgressAt: since, Sink: sink}
	ap.SetTopic([]byte(topic))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitProgress, RequestId: 21}
	req.SetAwaitProgress(ap)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	return before
}

func awaitProgressReply(t *testing.T, conn *fakeConn, before int) *protocol.AwaitProgressResponse {
	t.Helper()
	waitForWithin(t, 3*time.Second, func() bool { return len(conn.Sent()) > before })
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_AwaitProgress || resp.AwaitProgress() == nil {
		t.Fatalf("response kind = %v, want await_progress", resp.Kind)
	}
	return resp.AwaitProgress()
}

func noReplyYet(t *testing.T, conn *fakeConn, before int) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if n := len(conn.Sent()); n != before {
		t.Fatalf("got %d replies before any qualifying write, want none", n-before)
	}
}

func operatorConn(port string) *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1"), nextSendStreamID: 10}
}

func TestAwaitProgress_ReplyFiresOnNextWrite(t *testing.T) {
	f := newProgressFixture(t)
	op := operatorConn("9741")
	before := armProgress(t, f.h, op, f.tHex, 0, protocol.AwaitSink_Reply, "")
	noReplyYet(t, op, before)
	w := f.set(t, "s2", false)
	got := awaitProgressReply(t, op, before)
	if got.Status != protocol.AwaitProgressStatus_Fired || string(got.Progress) != "s2" || got.ProgressAt != w.ProgressAt || got.WatcherId == 0 {
		t.Fatalf("reply = %+v, want fired with s2 at %d", got, w.ProgressAt)
	}
	if n := len(f.h.idleWatchers().list("")); n != 0 {
		t.Fatalf("%d watchers left after firing, want 0 (one-shot)", n)
	}
}

func TestAwaitProgress_SinceFiresImmediately(t *testing.T) {
	f := newProgressFixture(t)
	a := f.set(t, "s1", false)
	time.Sleep(time.Millisecond)
	f.set(t, "s2", false)
	op := operatorConn("9742")
	got := awaitProgressReply(t, op, armProgress(t, f.h, op, f.tHex, a.ProgressAt, protocol.AwaitSink_Reply, ""))
	if got.Status != protocol.AwaitProgressStatus_Fired || string(got.Progress) != "s2" {
		t.Fatalf("reply = %+v, want an immediate fire with s2", got)
	}
}

func TestAwaitProgress_SinceCurrentWaits(t *testing.T) {
	f := newProgressFixture(t)
	b := f.set(t, "s2", false)
	op := operatorConn("9743")
	before := armProgress(t, f.h, op, f.tHex, b.ProgressAt, protocol.AwaitSink_Reply, "")
	noReplyYet(t, op, before)
	f.set(t, "s3", false)
	if got := awaitProgressReply(t, op, before); string(got.Progress) != "s3" {
		t.Fatalf("reply = %+v, want the third write", got)
	}
}

// A line written just before the end is the one a supervisor most needs: the
// baseline check runs before the terminal check.
func TestAwaitProgress_TerminalAfterBaseline(t *testing.T) {
	f := newProgressFixture(t)
	a := f.set(t, "working", false)
	time.Sleep(time.Millisecond)
	f.set(t, "final", false)
	f.h.Tasks.mu.Lock()
	f.h.Tasks.tasks[f.tHex].Status = protocol.TaskStatus_Succeeded
	f.h.Tasks.mu.Unlock()
	op := operatorConn("9744")
	got := awaitProgressReply(t, op, armProgress(t, f.h, op, f.tHex, a.ProgressAt, protocol.AwaitSink_Reply, ""))
	if got.Status != protocol.AwaitProgressStatus_Fired || string(got.Progress) != "final" {
		t.Fatalf("reply = %+v, want fired with final, not task_ended", got)
	}
	// With the baseline at the last write, the same terminal task answers ended.
	op2 := operatorConn("9745")
	got2 := awaitProgressReply(t, op2, armProgress(t, f.h, op2, f.tHex, got.ProgressAt, protocol.AwaitSink_Reply, ""))
	if got2.Status != protocol.AwaitProgressStatus_TaskEnded {
		t.Fatalf("reply = %+v, want task_ended", got2)
	}
}

func TestAwaitProgress_EndedWhileArmed(t *testing.T) {
	f := newProgressFixture(t)
	op := operatorConn("9746")
	before := armProgress(t, f.h, op, f.tHex, 0, protocol.AwaitSink_Reply, "")
	noReplyYet(t, op, before)
	f.h.fireProgressWatchers(f.tHex, protocol.AwaitProgressStatus_TaskEnded, "", 0)
	if got := awaitProgressReply(t, op, before); got.Status != protocol.AwaitProgressStatus_TaskEnded {
		t.Fatalf("reply = %+v, want task_ended", got)
	}
}

// Unlike await-idle, no live session is needed: a oneshot task reports too.
func TestAwaitProgress_OneshotNoMux(t *testing.T) {
	h := newTestHandler(t)
	tHex := h.Tasks.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	op := operatorConn("9747")
	before := armProgress(t, h, op, tHex, 0, protocol.AwaitSink_Reply, "")
	noReplyYet(t, op, before)
	if n := len(h.idleWatchers().list(tHex)); n != 1 {
		t.Fatalf("%d watchers armed on a oneshot task, want 1", n)
	}
}

func TestAwaitProgress_NotVisible(t *testing.T) {
	f := newProgressFixture(t)
	other := f.h.Tasks.Create("/repo", "stranger", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	// f.conn is T's agent connection; the stranger is outside T's subtree.
	got := awaitProgressReply(t, f.conn, armProgress(t, f.h, f.conn, other, 0, protocol.AwaitSink_Reply, ""))
	if got.Status != protocol.AwaitProgressStatus_NotFound {
		t.Fatalf("reply = %+v, want not_found", got)
	}
}

func TestAwaitProgress_NotifySinkRequiresNotifyCap(t *testing.T) {
	f := newProgressFixture(t) // T holds Capability_None
	before := armProgress(t, f.h, f.conn, f.tHex, 0, protocol.AwaitSink_Notify, "")
	waitFor(t, func() bool { return len(f.conn.Sent()) > before })
	resp := lastTaskControlResponse(t, f.conn)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied || resp.PermissionDenied().RequiredCap != protocol.Capability_Notify {
		t.Fatalf("sink=notify without notify: kind=%v, want permission_denied(notify)", resp.Kind)
	}
}

func TestAwaitProgress_NotifySinkFiresWithText(t *testing.T) {
	f := newProgressFixture(t)
	var got []protocol.NotifyEvent
	f.h.OnNotify = func(ev protocol.NotifyEvent) { got = append(got, ev) }
	op := operatorConn("9748")
	if r := awaitProgressReply(t, op, armProgress(t, f.h, op, f.tHex, 0, protocol.AwaitSink_Notify, "")); r.Status != protocol.AwaitProgressStatus_Armed {
		t.Fatalf("arm = %+v, want armed", r)
	}
	f.set(t, "stage 4", false)
	if len(got) != 1 || string(got[0].Text) != "progress "+f.tHex[:8]+": stage 4" {
		t.Fatalf("notifications = %+v, want one carrying the line", got)
	}
}

func TestAwaitList_ShowsCondition(t *testing.T) {
	f := newProgressFixture(t)
	reg := f.h.Sessions
	mux := titledMux(t, "x")
	reg.Add(f.tHex, mux)
	op := operatorConn("9749")
	armProgress(t, f.h, op, f.tHex, 0, protocol.AwaitSink_Reply, "")
	idle := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 30}
	idle.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: hexToTaskID(t, f.tHex), ThresholdMs: 60000, Sink: protocol.AwaitSink_Reply})
	f.h.Handle(op, encodeTaskControlRequest(t, &idle))
	waitFor(t, func() bool { return len(f.h.idleWatchers().list("")) == 2 })

	rows := f.h.visibleIdleWatchers(op.ConnectionID().String(), protocol.TaskID{})
	conds := map[protocol.AwaitCondition]uint32{}
	for _, r := range rows {
		conds[r.Condition] = r.ThresholdMs
	}
	if th, ok := conds[protocol.AwaitCondition_Progress]; !ok || th != 0 {
		t.Errorf("progress row missing or threshold %d, want present with 0", th)
	}
	if th, ok := conds[protocol.AwaitCondition_Idle]; !ok || th != 60000 {
		t.Errorf("idle row missing or threshold %d, want 60000", th)
	}
}
