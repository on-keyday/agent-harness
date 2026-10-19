package protocol

import "testing"

// The fields the progress spec adds sit after variable-length fields and, for
// live_terminal, inside a bit-packed byte, so a round trip is the cheapest
// proof each one lands where its neighbours expect it.
func TestProgressWire_RoundTrip(t *testing.T) {
	var ti TaskInfo
	ti.SetIsAttached(true)
	ti.SetLiveTerminal(true)
	ti.SetProgress([]byte("task A / stage 2"))
	ti.ProgressAt = 11
	ti.ProgressAgeMs = 22
	ti.SetTitle([]byte("✳ topic"))
	ti.HoldDeadlineNs = 33
	var gotTI TaskInfo
	if err := gotTI.DecodeExact(ti.MustAppend(nil)); err != nil {
		t.Fatalf("TaskInfo decode: %v", err)
	}
	if !gotTI.IsAttached() || !gotTI.LiveTerminal() || gotTI.SkillsInjected() ||
		string(gotTI.Progress) != "task A / stage 2" || gotTI.ProgressAt != 11 ||
		gotTI.ProgressAgeMs != 22 || string(gotTI.Title) != "✳ topic" || gotTI.HoldDeadlineNs != 33 {
		t.Fatalf("TaskInfo round trip = %+v", gotTI)
	}

	ev := TaskStatusEvent{Kind: StatusEventKind_TaskProgress, Viewers: 2, ProgressAt: 5}
	ev.SetLiveTerminal(true)
	ev.SetProgress([]byte("p"))
	ev.SetTitle([]byte("t"))
	var gotEv TaskStatusEvent
	if err := gotEv.DecodeExact(ev.MustAppend(nil)); err != nil {
		t.Fatalf("TaskStatusEvent decode: %v", err)
	}
	if gotEv.Kind != StatusEventKind_TaskProgress || gotEv.Viewers != 2 || !gotEv.LiveTerminal() ||
		gotEv.ProgressAt != 5 || string(gotEv.Progress) != "p" || string(gotEv.Title) != "t" {
		t.Fatalf("TaskStatusEvent round trip = %+v", gotEv)
	}

	req := TaskControlRequest{Kind: TaskControlKind_ProgressSet, RequestId: 3}
	var ps ProgressSetRequest
	ps.SetClear(true)
	req.SetProgressSet(ps)
	var gotReq TaskControlRequest
	if err := gotReq.DecodeExact(req.MustAppend(nil)); err != nil {
		t.Fatalf("ProgressSet decode: %v", err)
	}
	if p := gotReq.ProgressSet(); p == nil || !p.Clear() || len(p.Text) != 0 {
		t.Fatalf("ProgressSet round trip = %+v", gotReq.ProgressSet())
	}

	resp := TaskControlResponse{Kind: TaskControlKind_AwaitProgress, RequestId: 4}
	ar := AwaitProgressResponse{Status: AwaitProgressStatus_Fired, ProgressAt: 9, WatcherId: 7}
	ar.SetProgress([]byte("s2"))
	resp.SetAwaitProgress(ar)
	var gotResp TaskControlResponse
	if err := gotResp.DecodeExact(resp.MustAppend(nil)); err != nil {
		t.Fatalf("AwaitProgress decode: %v", err)
	}
	if a := gotResp.AwaitProgress(); a == nil || a.Status != AwaitProgressStatus_Fired ||
		a.ProgressAt != 9 || string(a.Progress) != "s2" || a.WatcherId != 7 {
		t.Fatalf("AwaitProgress round trip = %+v", gotResp.AwaitProgress())
	}
}
