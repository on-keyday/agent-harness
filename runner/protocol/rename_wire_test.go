package protocol

import "testing"

func TestTaskRenameRoundTrip(t *testing.T) {
	var req TaskRenameRequest
	req.TaskId.Id[0] = 0xab
	req.SetName([]byte("ci-runner"))
	var gotReq TaskRenameRequest
	roundTrip(t, func() []byte { return req.MustAppend(nil) },
		func(b []byte) error { return gotReq.DecodeExact(b) },
		func() []byte { return gotReq.MustAppend(nil) })
	if gotReq.TaskId != req.TaskId || string(gotReq.Name) != "ci-runner" {
		t.Fatalf("request = %+v", gotReq)
	}

	resp := TaskRenameResponse{Status: TaskRenameStatus_BadName}
	var gotResp TaskRenameResponse
	roundTrip(t, func() []byte { return resp.MustAppend(nil) },
		func(b []byte) error { return gotResp.DecodeExact(b) },
		func() []byte { return gotResp.MustAppend(nil) })
	if gotResp.Status != TaskRenameStatus_BadName {
		t.Fatalf("status = %v", gotResp.Status)
	}

	var ctl TaskControlRequest
	ctl.Header.Kind = TaskControlKind_TaskRename
	if !ctl.SetTaskRename(req) {
		t.Fatal("SetTaskRename on the request union returned false")
	}
	var ctlResp TaskControlResponse
	ctlResp.Kind = TaskControlKind_TaskRename
	if !ctlResp.SetTaskRename(resp) {
		t.Fatal("SetTaskRename on the response union returned false")
	}
}

func TestSetCapsResponseCarriesMessage(t *testing.T) {
	var resp SetCapsResponse
	resp.Status = SetCapsStatus_InvalidScope
	resp.SetMessage([]byte("scope id x is outside what you may delegate"))
	var got SetCapsResponse
	roundTrip(t, func() []byte { return resp.MustAppend(nil) },
		func(b []byte) error { return got.DecodeExact(b) },
		func() []byte { return got.MustAppend(nil) })
	if string(got.Message) != "scope id x is outside what you may delegate" {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestTaskCapsSetAuthoritySourceAppended(t *testing.T) {
	if AuthoritySource_Resume != 5 || AuthoritySource_TaskCapsSet != 6 {
		t.Fatalf("authority source numbers: resume=%d task_caps_set=%d", AuthoritySource_Resume, AuthoritySource_TaskCapsSet)
	}
}
