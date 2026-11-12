package protocol

import "testing"

// Run mode, background conditions and the prompt stream id ride the second
// flag byte of OpenInteractiveRequest and two conditional blocks appended
// after the detached block; a round trip proves each lands where its
// neighbours expect it.
func TestOpenInteractiveRequestRunModeRoundTrip(t *testing.T) {
	oi := OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	oi.SetMaxDescendantsPresent(true)
	oi.SetRunModeSet(true)
	oi.SetBackground(true)
	oi.SetDetached(true)
	oi.SetInitialRows(40)
	oi.SetInitialCols(120)
	oi.SetConditionsPresent(true)
	oi.SetConditions(BackgroundConditions{DeadlineMs: 3600000, TurnEnd: TurnEndSignal_Lifecycle, QuietMs: 1, BlockedMs: 2, QueueTimeoutMs: 3})
	oi.SetPromptPresent(true)
	oi.SetPromptStreamId(7)

	var got OpenInteractiveRequest
	if err := got.DecodeExact(oi.MustAppend(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.RunModeSet() || !got.Background() || !got.MaxDescendantsPresent() || !got.Detached() {
		t.Fatalf("flags = run_mode_set %v background %v max_desc %v detached %v",
			got.RunModeSet(), got.Background(), got.MaxDescendantsPresent(), got.Detached())
	}
	if r, c := got.InitialRows(), got.InitialCols(); r == nil || c == nil || *r != 40 || *c != 120 {
		t.Fatalf("initial size = %v x %v", r, c)
	}
	cond := got.Conditions()
	if !got.ConditionsPresent() || cond == nil || cond.DeadlineMs != 3600000 || cond.TurnEnd != TurnEndSignal_Lifecycle ||
		cond.QuietMs != 1 || cond.BlockedMs != 2 || cond.QueueTimeoutMs != 3 {
		t.Fatalf("conditions = %+v", cond)
	}
	if id := got.PromptStreamId(); !got.PromptPresent() || id == nil || *id != 7 {
		t.Fatalf("prompt stream id = %v", id)
	}
}

// A request from a client that predates the run-mode bits sets none of them,
// and the bits already in that byte keep their positions.
func TestOpenInteractiveRequestOldFlagsKeepTheirBits(t *testing.T) {
	oi := OpenInteractiveRequest{}
	oi.SetRepoPath([]byte("/r"))
	oi.SetMaxDescendantsPresent(true)
	var got OpenInteractiveRequest
	if err := got.DecodeExact(oi.MustAppend(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MaxDescendantsPresent() || got.RunModeSet() || got.Background() || got.ConditionsPresent() || got.PromptPresent() {
		t.Fatalf("flags drifted: %+v", got)
	}
}

func TestOpenExecRunnerRequestPromptStreamRoundTrip(t *testing.T) {
	oer := OpenExecRunnerRequest{StreamId: 3}
	oer.SetRepoPath([]byte("/r"))
	oer.SetEventStream(true)
	oer.SetPromptPresent(true)
	oer.SetPromptStreamId(9)
	var got OpenExecRunnerRequest
	if err := got.DecodeExact(oer.MustAppend(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if id := got.PromptStreamId(); !got.EventStream() || !got.PromptPresent() || id == nil || *id != 9 {
		t.Fatalf("got %+v", got)
	}

	body := PromptBody{}
	body.SetPrompt([]byte("triage new issues"))
	var gotBody PromptBody
	if err := gotBody.DecodeExact(body.MustAppend(nil)); err != nil || string(gotBody.Prompt) != "triage new issues" {
		t.Fatalf("PromptBody round trip = %q, %v", gotBody.Prompt, err)
	}
}

func TestSessionModeRoundTrip(t *testing.T) {
	req := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_SessionMode, RequestId: 4}}
	sm := SessionModeRequest{}
	sm.SetBackground(true)
	sm.SetConditionsPresent(true)
	sm.SetConditions(BackgroundConditions{DeadlineMs: 60000, TurnEnd: TurnEndSignal_Any})
	req.SetSessionMode(sm)
	var gotReq TaskControlRequest
	if err := gotReq.DecodeExact(req.MustAppend(nil)); err != nil {
		t.Fatalf("request decode: %v", err)
	}
	g := gotReq.SessionMode()
	if g == nil || !g.Background() || g.Conditions() == nil || g.Conditions().DeadlineMs != 60000 {
		t.Fatalf("request = %+v", g)
	}

	resp := TaskControlResponse{Kind: TaskControlKind_SessionMode, RequestId: 4}
	sr := SessionModeResponse{Status: SessionModeStatus_Queued}
	sr.SetErrorMsg([]byte("queued"))
	resp.SetSessionMode(sr)
	var gotResp TaskControlResponse
	if err := gotResp.DecodeExact(resp.MustAppend(nil)); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	if r := gotResp.SessionMode(); r == nil || r.Status != SessionModeStatus_Queued || string(r.ErrorMsg) != "queued" {
		t.Fatalf("response = %+v", r)
	}
}

func TestTaskInfoRunFieldsRoundTrip(t *testing.T) {
	info := TaskInfo{
		RunMode: RunMode_Background, EndReason: EndReason_TurnDoneQuiet,
		DeadlineNs: 11, QueueDeadlineNs: 12, WaitReason: WaitReason_AllBusy,
		Conditions: BackgroundConditions{DeadlineMs: 5},
	}
	info.CancelledBy.Id[0] = 0xab
	var got TaskInfo
	if err := got.DecodeExact(info.MustAppend(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunMode != RunMode_Background || got.EndReason != EndReason_TurnDoneQuiet || got.DeadlineNs != 11 ||
		got.QueueDeadlineNs != 12 || got.WaitReason != WaitReason_AllBusy || got.Conditions.DeadlineMs != 5 ||
		got.CancelledBy.Id[0] != 0xab {
		t.Fatalf("got %+v", got)
	}

	ev := TaskStatusEvent{RunMode: RunMode_Background, EndReason: EndReason_Deadline}
	var gotEv TaskStatusEvent
	if err := gotEv.DecodeExact(ev.MustAppend(nil)); err != nil || gotEv.RunMode != RunMode_Background || gotEv.EndReason != EndReason_Deadline {
		t.Fatalf("event = %+v, %v", gotEv, err)
	}
}
