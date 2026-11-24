package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Pins of answers the request-cap-need refactor must not change and that no
// other test held when it started (docs/superpowers/specs/
// 2026-11-22-request-cap-need-design.md §5.4). Already pinned elsewhere:
// ListFiles' file_read report and its file_write-only allowance
// (TestDirectionGate), RecordKeep's direction and its no-oracle order
// (TestRecordKeepGatedByDirection, TestRecordKeepIsNoOracleOutOfScope),
// AgentSend's board_peer label (reach_handlers_test.go), the in-reach unbind
// refusal (TestForegroundResumeOfBoundTaskNeedsBitAndUnbinds).

func pinCaller(t *testing.T, h *TaskHandler, g authority.Grant, port string) (*fakeConn, string) {
	t.Helper()
	id := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, g, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1")}
	if h.principals == nil {
		h.principals = make(map[objproto.ConnectionID]protocol.TaskID)
	}
	h.principals[conn.ConnectionID()] = hexToTaskID(t, id)
	return conn, id
}

// A caller holding only file_write lists through file_write's OWN scope. The
// target is running with no runner, so a call that passes the scope answers
// runner_offline and one that does not answers no_such_task.
func TestPinListFilesScopesThroughTheHeldBit(t *testing.T) {
	for _, c := range []struct {
		name      string
		writeBase protocol.ScopeBase // file_write's override; the task's base is global
		want      protocol.ListFilesStatus
	}{
		// file_write's scope is self only. Resolving through file_read's
		// (global, not held) would wrongly pass.
		{"outside file_write's scope", protocol.ScopeBase_None, protocol.ListFilesStatus_NoSuchTask},
		{"inside file_write's scope", protocol.ScopeBase_Global, protocol.ListFilesStatus_RunnerOffline},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHandler(t)
			other := h.Tasks.Create("repo", "o", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
				protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
			h.Tasks.tasks[other].Status = protocol.TaskStatus_Running
			conn, _ := pinCaller(t, h, authority.Grant{
				Caps: protocol.Capability_FileWrite,
				Scope: authority.Scope{Base: protocol.ScopeBase_Global, Overrides: []authority.ScopeOverride{
					{Caps: protocol.Capability_FileWrite, Base: c.writeBase},
				}},
			}, "9801")
			req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ListFiles, RequestId: 1}}
			req.SetListFiles(protocol.ListFilesRequest{TaskId: hexToTaskID(t, other)})
			h.Handle(conn, encodeTaskControlRequest(t, req))
			resp := lastTaskControlResponse(t, conn)
			if resp.Kind != protocol.TaskControlKind_ListFiles || resp.ListFiles().Status != c.want {
				t.Fatalf("%+v, want list_files %v", resp, c.want)
			}
		})
	}
}

// The schedule bit of a foreground resume is asked only of a caller that
// reaches the task through the resume gate: one that cannot reach it is told
// resume_not_found, so the bit's absence says nothing about whether an
// unreachable task is bound (task_handler.go's S-D15 comment).
func TestPinUnbindIsAskedOnlyInsideResumeReach(t *testing.T) {
	e := newSchedEnv(t)
	openRunner(t, e.h)
	task := e.terminal(t, protocol.RunMode_Background, "")
	e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	outside, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_Resume | protocol.Capability_Cancel,
		Scope: authority.Scope{Base: protocol.ScopeBase_None}})
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 3}}
	req.SetOpenInteractive(*foregroundResume(task))
	e.h.Handle(outside, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, outside)
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("an out-of-reach resume was refused naming %v: that answers whether the task is bound", resp.PermissionDenied().RequiredCap)
	}
	if oi := resp.OpenInteractive(); oi == nil || oi.Status != protocol.OpenInteractiveStatus_ResumeNotFound {
		t.Fatalf("out-of-reach resume: %+v, want resume_not_found", resp)
	}
}

// exec kill never answers permission_denied: a caller without exec_run is told
// not_found, so a refusal cannot confirm that an exec it may not see exists.
func TestPinExecRunKillWithoutTheBitIsNotFound(t *testing.T) {
	h := newTestHandler(t)
	target := h.Tasks.Create("repo", "t", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All}, "")
	e := &execRun{taskIDHex: target, clientCID: tcid("c-pin"), control: newRecordingBidiStream(1)}
	h.execs().add(e)
	conn, _ := pinCaller(t, h, authority.Grant{Caps: protocol.Capability_Cancel,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}}, "9802")
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ExecRunKill, RequestId: 4}}
	req.SetExecRunKill(protocol.ExecRunKillRequest{ExecId: e.execID})
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_ExecRunKill || resp.ExecRunKill().Status != protocol.ExecRunStatus_NotFound {
		t.Fatalf("exec kill without exec_run: %+v, want exec_run_kill not_found", resp)
	}
}

// Subscribing to another task's direct topic without board_observe is refused
// naming board_observe (the reach check's label, spec §3.4).
func TestPinAgentSubscribeReportsBoardObserve(t *testing.T) {
	h, conn, _, _ := reachHandler(t, protocol.Capability_None)
	h.handleAgentSubscribe(conn, 8, "chat.0123abcd", false)
	resp := lastTaskControlResponse(t, conn)
	if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil ||
		pd.RequiredCap != protocol.Capability_BoardObserve {
		t.Fatalf("subscribe to another task's chat: %+v, want permission_denied board_observe", resp)
	}
}
