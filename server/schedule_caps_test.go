package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestScheduleManagementAndRunAreIndependent(t *testing.T) {
	for _, runOnly := range []bool{false, true} {
		name := "edit only"
		bit := protocol.Capability_ScheduleEdit
		if runOnly {
			name, bit = "run only", protocol.Capability_ScheduleRun
		}
		t.Run(name, func(t *testing.T) {
			e := newSchedEnv(t)
			e.addCandidate("claude")
			task := e.terminal(t, protocol.RunMode_Background, "")
			sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
			conn, _ := e.agent(t, authority.Grant{
				Caps:  bit | protocol.Capability_Spawn | protocol.Capability_Resume | protocol.Capability_FileRead,
				Scope: authority.Scope{Base: protocol.ScopeBase_Global},
			})
			for _, kind := range []protocol.TaskControlKind{
				protocol.TaskControlKind_ScheduleCreate, protocol.TaskControlKind_ScheduleEdit,
				protocol.TaskControlKind_ScheduleDelete, protocol.TaskControlKind_ScheduleRun,
			} {
				r := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind, RequestId: 1}}
				switch kind {
				case protocol.TaskControlKind_ScheduleCreate:
					sp := createSpec()
					sp.SetPromptPresent(true)
					sp.SetPromptStreamId(5)
					r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *sp})
				case protocol.TaskControlKind_ScheduleEdit:
					r.SetScheduleEdit(protocol.ScheduleEditRequest{Id: schedHexID(t, sc.ID)})
				case protocol.TaskControlKind_ScheduleDelete:
					r.SetScheduleDelete(protocol.ScheduleIDRequest{Id: schedHexID(t, sc.ID)})
				case protocol.TaskControlKind_ScheduleRun:
					r.SetScheduleRun(protocol.ScheduleIDRequest{Id: schedHexID(t, sc.ID)})
				}
				body := protocol.PromptBody{}
				body.SetPrompt([]byte("run the saved work"))
				wireConn := promptConn{fakeConn: conn, st: &fakeRecv{data: body.MustAppend(nil)}}
				e.h.Handle(wireConn, encodeTaskControlRequest(t, r))
				waitSent(wireConn)
				resp := lastTaskControlResponse(t, conn)
				allowed := (kind == protocol.TaskControlKind_ScheduleRun) == runOnly
				if allowed {
					if r := scheduleResp(t, conn, kind); r.Status != protocol.ScheduleStatus_Ok {
						t.Fatalf("%v: %v %s", kind, r.Status, r.Message)
					}
				} else {
					want := protocol.Capability_ScheduleEdit
					if kind == protocol.TaskControlKind_ScheduleRun {
						want = protocol.Capability_ScheduleRun
					}
					if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != want {
						t.Fatalf("%v: expected denial for %v, got %+v", kind, want, resp)
					}
				}
			}
			if runOnly && len(e.fires) != 1 || !runOnly && len(e.fires) != 0 {
				t.Fatalf("unexpected firings: %v", e.fires)
			}
		})
	}
}

func TestScheduleRunUsesItsOwnScope(t *testing.T) {
	for _, runReaches := range []bool{false, true} {
		e := newSchedEnv(t)
		task := e.terminal(t, protocol.RunMode_Background, "")
		sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
		editBase, runBase := protocol.ScopeBase_Global, protocol.ScopeBase_None
		if runReaches {
			editBase, runBase = runBase, editBase
		}
		conn, _ := e.agent(t, authority.Grant{
			Caps: protocol.Capability_ScheduleRun | protocol.Capability_Resume,
			Scope: authority.Scope{Base: protocol.ScopeBase_Global, Overrides: []authority.ScopeOverride{
				{Caps: protocol.Capability_ScheduleEdit, Base: editBase},
				{Caps: protocol.Capability_ScheduleRun, Base: runBase},
			}},
		})
		want := protocol.ScheduleStatus_NoSuchSchedule
		if runReaches {
			want = protocol.ScheduleStatus_Ok
		}
		if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID); r.Status != want {
			t.Fatalf("runReaches=%v: got %v, want %v", runReaches, r.Status, want)
		}
		if runReaches != (len(e.fires) == 1) {
			t.Fatalf("runReaches=%v: firings %v", runReaches, e.fires)
		}
	}
}
