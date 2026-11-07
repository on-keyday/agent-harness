package protocol

import "testing"

// The signal and the fire's readings are appended after variable-length
// fields (topic, origin_cid), so a round trip is the proof each lands where
// its neighbours expect it.
func TestAwaitIdleSignalRoundTrip(t *testing.T) {
	req := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_AwaitIdle, RequestId: 5}}
	ar := AwaitIdleRequest{ThresholdMs: 100, Sink: AwaitSink_Board, Signal: AwaitIdleSignal_Any}
	ar.SetTopic([]byte("t"))
	req.SetAwaitIdle(ar)
	var gotReq TaskControlRequest
	if err := gotReq.DecodeExact(req.MustAppend(nil)); err != nil {
		t.Fatalf("AwaitIdleRequest decode: %v", err)
	}
	if a := gotReq.AwaitIdle(); a == nil || a.Signal != AwaitIdleSignal_Any || string(a.Topic) != "t" || a.ThresholdMs != 100 {
		t.Fatalf("AwaitIdleRequest round trip = %+v", gotReq.AwaitIdle())
	}

	resp := TaskControlResponse{Kind: TaskControlKind_AwaitIdle, RequestId: 5}
	resp.SetAwaitIdle(AwaitIdleResponse{
		Status: AwaitIdleStatus_Fired, LastOutputAt: 9, WatcherId: 3,
		FiredBy: AwaitIdleSignal_Program, Lifecycle: LifecycleState_Idle,
		ProgramState: ProgramState_Done, ProgramKind: ProgramKind_None,
	})
	var gotResp TaskControlResponse
	if err := gotResp.DecodeExact(resp.MustAppend(nil)); err != nil {
		t.Fatalf("AwaitIdleResponse decode: %v", err)
	}
	r := gotResp.AwaitIdle()
	if r == nil || r.Status != AwaitIdleStatus_Fired || r.WatcherId != 3 || r.FiredBy != AwaitIdleSignal_Program ||
		r.Lifecycle != LifecycleState_Idle || r.ProgramState != ProgramState_Done || r.ProgramKind != ProgramKind_None {
		t.Fatalf("AwaitIdleResponse round trip = %+v", r)
	}

	info := AwaitIdleWatcherInfo{WatcherId: 1, Sink: AwaitSink_Board, Condition: AwaitCondition_Idle, Signal: AwaitIdleSignal_Lifecycle}
	info.SetTopic([]byte("chat.x"))
	info.SetOriginCid([]byte("ws:1.2.3.4:5-6"))
	var gotInfo AwaitIdleWatcherInfo
	if err := gotInfo.DecodeExact(info.MustAppend(nil)); err != nil {
		t.Fatalf("AwaitIdleWatcherInfo decode: %v", err)
	}
	if gotInfo.Signal != AwaitIdleSignal_Lifecycle || string(gotInfo.Topic) != "chat.x" || string(gotInfo.OriginCid) != "ws:1.2.3.4:5-6" {
		t.Fatalf("AwaitIdleWatcherInfo round trip = %+v", gotInfo)
	}
}
