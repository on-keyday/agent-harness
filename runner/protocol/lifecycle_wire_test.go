package protocol

import "testing"

// The lifecycle and program-status fields sit after variable-length fields on
// both TaskInfo and TaskStatusEvent, so a round trip is the cheapest proof each
// one lands where its neighbours expect it.
func TestLifecycleWire_RoundTrip(t *testing.T) {
	req := TaskControlRequest{Kind: TaskControlKind_LifecycleSet, RequestId: 3}
	req.SetLifecycleSet(LifecycleSetRequest{Lifecycle: LifecycleState_Waiting, ReportedAt: 123})
	enc := req.MustAppend(nil)
	if len(enc) >= 64 {
		t.Fatalf("lifecycle_set request is %d bytes; it must stay tiny", len(enc))
	}
	var gotReq TaskControlRequest
	if err := gotReq.DecodeExact(enc); err != nil {
		t.Fatalf("LifecycleSet decode: %v", err)
	}
	if l := gotReq.LifecycleSet(); l == nil || l.Lifecycle != LifecycleState_Waiting || l.ReportedAt != 123 {
		t.Fatalf("LifecycleSet round trip = %+v", gotReq.LifecycleSet())
	}

	resp := TaskControlResponse{Kind: TaskControlKind_LifecycleSet, RequestId: 3}
	resp.SetLifecycleSet(LifecycleSetResponse{Status: LifecycleSetStatus_Stale, LifecycleAt: 0})
	var gotResp TaskControlResponse
	if err := gotResp.DecodeExact(resp.MustAppend(nil)); err != nil {
		t.Fatalf("LifecycleSetResponse decode: %v", err)
	}
	if r := gotResp.LifecycleSet(); r == nil || r.Status != LifecycleSetStatus_Stale {
		t.Fatalf("LifecycleSetResponse round trip = %+v", gotResp.LifecycleSet())
	}

	var ti TaskInfo
	ti.SetTitle([]byte("t"))
	ti.Lifecycle = LifecycleState_Idle
	ti.LifecycleAt = 7
	ti.LifecycleAgeMs = 9
	ti.ProgramState = ProgramState_Blocked
	ti.ProgramKind = ProgramKind_Question
	ti.SetProgramMsg([]byte("q?"))
	ti.AuthorityAgeMs = 44
	var gotTI TaskInfo
	if err := gotTI.DecodeExact(ti.MustAppend(nil)); err != nil {
		t.Fatalf("TaskInfo decode: %v", err)
	}
	if string(gotTI.Title) != "t" || gotTI.Lifecycle != LifecycleState_Idle || gotTI.LifecycleAt != 7 ||
		gotTI.LifecycleAgeMs != 9 || gotTI.ProgramState != ProgramState_Blocked ||
		gotTI.ProgramKind != ProgramKind_Question || string(gotTI.ProgramMsg) != "q?" || gotTI.AuthorityAgeMs != 44 {
		t.Fatalf("TaskInfo round trip = %+v", gotTI)
	}

	ev := TaskStatusEvent{Kind: StatusEventKind_TaskLifecycle, Lifecycle: LifecycleState_Idle, LifecycleAt: 7, LifecycleAgeMs: 9,
		ProgramState: ProgramState_Blocked, ProgramKind: ProgramKind_Question}
	ev.SetTitle([]byte("t"))
	ev.SetProgramMsg([]byte("q?"))
	var gotEv TaskStatusEvent
	if err := gotEv.DecodeExact(ev.MustAppend(nil)); err != nil {
		t.Fatalf("TaskStatusEvent decode: %v", err)
	}
	if gotEv.Kind != StatusEventKind_TaskLifecycle || string(gotEv.Title) != "t" || gotEv.Lifecycle != LifecycleState_Idle ||
		gotEv.LifecycleAt != 7 || gotEv.LifecycleAgeMs != 9 || gotEv.ProgramState != ProgramState_Blocked ||
		gotEv.ProgramKind != ProgramKind_Question || string(gotEv.ProgramMsg) != "q?" {
		t.Fatalf("TaskStatusEvent round trip = %+v", gotEv)
	}
}
