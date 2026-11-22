package server

import (
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestRequestNeedAgreesWithTheServer is spec V3: for each request whose bits
// the request decides, a caller missing exactly one clause's bits is answered
// as authority.RequestNeed says -- PermissionDenied naming the clause's Report,
// or (Report 0) the kind's own answer, never PermissionDenied. RequestNeed is
// what a consumer outside the server reads, so a disagreement here is a wrong
// public answer even while the server itself is unchanged.
func TestRequestNeedAgreesWithTheServer(t *testing.T) {
	type mk func(target protocol.TaskID) *protocol.TaskControlRequest
	kindReq := func(k protocol.TaskControlKind, set func(*protocol.TaskControlRequest, protocol.TaskID)) mk {
		return func(target protocol.TaskID) *protocol.TaskControlRequest {
			r := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: k, RequestId: 9}}
			set(r, target)
			return r
		}
	}
	openReq := func(resume, detached, record bool) mk {
		return kindReq(protocol.TaskControlKind_OpenInteractive, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
			var oi protocol.OpenInteractiveRequest
			if resume {
				oi.ResumeTaskId = target
			}
			oi.SetRepoPath([]byte("/r"))
			oi.SetDetached(detached)
			if detached {
				oi.SetInitialRows(24) // a detached open carries its size
				oi.SetInitialCols(80)
			}
			if record {
				var ext protocol.OpenInteractiveExt
				ext.SetRecordSet(true)
				ext.SetRecord(true)
				oi.SetExtPresent(true)
				oi.SetExt(ext)
			}
			r.SetOpenInteractive(oi)
		})
	}
	cases := map[string]mk{}
	for _, m := range []protocol.AttachMode{protocol.AttachMode_View, protocol.AttachMode_Cowrite, protocol.AttachMode_Control} {
		cases[fmt.Sprintf("attach %v", m)] = kindReq(protocol.TaskControlKind_AttachSession, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
			r.SetAttach(protocol.AttachSessionRequest{TaskId: target, Mode: m})
		})
	}
	for _, d := range []protocol.FileTransferDirection{protocol.FileTransferDirection_Pull, protocol.FileTransferDirection_Push, protocol.FileTransferDirection_Mkdir} {
		cases[fmt.Sprintf("file transfer %v", d)] = kindReq(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
			r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{TaskId: target, Direction: d})
		})
	}
	cases["list files"] = kindReq(protocol.TaskControlKind_ListFiles, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		r.SetListFiles(protocol.ListFilesRequest{TaskId: target})
	})
	cases["open port forward"] = kindReq(protocol.TaskControlKind_OpenPortForward, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		r.SetOpenPortForward(protocol.OpenPortForwardRequest{TaskId: target})
	})
	for _, d := range []protocol.PortForwardDirection{protocol.PortForwardDirection_Local, protocol.PortForwardDirection_Remote} {
		cases[fmt.Sprintf("register forward %v", d)] = kindReq(protocol.TaskControlKind_RegisterPortForward, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
			r.SetRegisterPortForward(protocol.RegisterPortForwardRequest{TaskId: target, Direction: d})
		})
	}
	for _, on := range []bool{true, false} {
		cases[fmt.Sprintf("record switch on=%t", on)] = kindReq(protocol.TaskControlKind_RecordSwitch, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
			rs := protocol.RecordSwitchRequest{TaskId: target}
			rs.SetOn(on)
			r.SetRecordSwitch(rs)
		})
	}
	cases["record keep"] = kindReq(protocol.TaskControlKind_RecordKeep, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		r.SetRecordKeep(protocol.RecordKeepRequest{Ref: protocol.RecordRef{TaskId: target}})
	})
	cases["restore"] = kindReq(protocol.TaskControlKind_RestoreTasks, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		rt := protocol.RestoreTasksRequest{}
		rt.SetTaskIds([]protocol.TaskID{target})
		r.SetRestoreTasks(rt)
	})
	cases["spawn detached"] = openReq(false, true, false)
	cases["spawn attached"] = openReq(false, false, false)
	cases["spawn detached recorded"] = openReq(false, true, true)
	cases["resume attached"] = openReq(true, false, false)
	cases["await idle notify"] = kindReq(protocol.TaskControlKind_AwaitIdle, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		r.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: target, Sink: protocol.AwaitSink_Notify})
	})
	cases["await progress notify"] = kindReq(protocol.TaskControlKind_AwaitProgress, func(r *protocol.TaskControlRequest, target protocol.TaskID) {
		r.SetAwaitProgress(protocol.AwaitProgressRequest{TaskId: target, Sink: protocol.AwaitSink_Notify})
	})

	port := 9900
	for name, build := range cases {
		probe := build(protocol.TaskID{Id: [16]byte{1}})
		n := authority.RequestNeed(probe)
		if n.Class != authority.NeedBits || len(n.Clauses) == 0 {
			t.Errorf("%s: RequestNeed %+v has no clause to check", name, n)
			continue
		}
		for i, c := range n.Clauses {
			port++
			t.Run(fmt.Sprintf("%s/clause%d", name, i), func(t *testing.T) {
				h := newTestHandler(t)
				recs, err := NewRecordStore(RecordConfig{Dir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				h.Records = recs // without one every record kind answers not_found before its gate
				target := h.Tasks.Create("repo", "t", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
					protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
				h.Tasks.tasks[target].Status = protocol.TaskStatus_Running
				conn, _ := pinCaller(t, h, authority.Grant{Caps: protocol.Capability_All &^ c.AnyOf,
					Scope: authority.Scope{Base: protocol.ScopeBase_Global}}, itoa(port))
				h.Handle(conn, encodeTaskControlRequest(t, build(hexToTaskID(t, target))))
				resp := lastTaskControlResponse(t, conn)
				denied := resp.Kind == protocol.TaskControlKind_PermissionDenied
				switch {
				case c.Report != 0 && (!denied || resp.PermissionDenied().RequiredCap != c.Report):
					t.Errorf("without %v: %+v, want permission_denied naming %v", c.AnyOf, resp, c.Report)
				case c.Report == 0 && denied:
					t.Errorf("without %v: permission_denied naming %v, but RequestNeed says the kind answers with its own status",
						c.AnyOf, resp.PermissionDenied().RequiredCap)
				}
			})
		}
	}
}
