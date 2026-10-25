package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunnerReportsRequireCurrentAssignment(t *testing.T) {
	for _, kind := range []protocol.RunnerMessageType{protocol.RunnerMessageType_TaskAccepted, protocol.RunnerMessageType_TaskStarted, protocol.RunnerMessageType_TaskFinished} {
		for _, sender := range []string{"owner", "other runner", "unregistered", "displaced", "no reservation"} {
			t.Run(kind.String()+"/"+sender, func(t *testing.T) {
				reg, tasks := NewRegistry(), NewTaskStore()
				owner := &fakeConn{id: tcid("report-owner")}
				identity := testRunnerID("report-owner")
				taskID := tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
				tasks.Assign(taskID, identity, "/original", false)
				reg.Add(&RunnerEntry{ID: owner.id, Identity: identity, MaxTasks: 1, ActiveTasks: map[string]struct{}{taskID: {}}, Conn: owner})
				conn := owner
				switch sender {
				case "other runner":
					conn = &fakeConn{id: tcid("report-other")}
					reg.Add(&RunnerEntry{ID: conn.id, Identity: testRunnerID("other"), MaxTasks: 1, Conn: conn})
				case "unregistered":
					conn = &fakeConn{id: tcid("report-unknown")}
				case "displaced":
					replacement := &fakeConn{id: tcid("report-replacement")}
					reg.Add(&RunnerEntry{ID: replacement.id, Identity: identity, MaxTasks: 1, ActiveTasks: map[string]struct{}{taskID: {}}, Conn: replacement})
				case "no reservation":
					reg.UnbindTask(owner.id, taskID)
				}
				board := newTestBoard(t)
				ticket := [16]byte{1}
				boardRegisterTask(board, identity, taskID, ticket, "", agentboard.NewConversation)
				changes, started := 0, 0
				h := &RunnerHandler{Registry: reg, Tasks: tasks, Board: board, Now: time.Now,
					OnChange: func() { changes++ }, OnTaskStarted: func(string) { started++ }}
				msg := &protocol.RunnerMessage{Kind: kind}
				tid := taskIDFromHex(taskID)
				switch kind {
				case protocol.RunnerMessageType_TaskAccepted:
					msg.SetTaskAccepted(protocol.TaskAccepted{TaskId: tid})
				case protocol.RunnerMessageType_TaskStarted:
					report := protocol.TaskStarted{TaskId: tid}
					report.SetWorktreeDir([]byte("/reported"))
					msg.SetTaskStarted(report)
				case protocol.RunnerMessageType_TaskFinished:
					msg.SetTaskFinished(protocol.TaskFinished{TaskId: tid})
				}
				h.Handle(conn, encodeRunnerMessage(t, msg))
				task, _ := tasks.Get(taskID)
				_, ticketExists := boardTaskTicket(board, identity, tid)
				if sender != "owner" {
					if changes != 0 || started != 0 || task.Status != protocol.TaskStatus_Running || task.WorktreeDir != "/original" || !ticketExists {
						t.Fatalf("non-owner changed state: changes=%d started=%d task=%+v ticket=%v", changes, started, task, ticketExists)
					}
					return
				}
				if changes != 1 {
					t.Fatalf("owner report did not reach its handler: changes=%d", changes)
				}
				if kind == protocol.RunnerMessageType_TaskFinished && (task.Status != protocol.TaskStatus_Succeeded || ticketExists) {
					t.Fatal("owner completion did not finish task and revoke ticket")
				}
				if kind == protocol.RunnerMessageType_TaskStarted && (task.WorktreeDir != "/reported" || started != 1) {
					t.Fatal("owner startup did not update worktree and callback")
				}
			})
		}
	}
}

func TestExecCompletionRequiresCurrentRunner(t *testing.T) {
	h := newTestHandler(t)
	owner, wrong := &fakeConn{id: tcid("exec-owner")}, &fakeConn{id: tcid("exec-wrong")}
	identity := testRunnerID("exec-owner")
	h.Registry.Add(&RunnerEntry{ID: owner.id, Identity: identity, Conn: owner})
	h.Registry.Add(&RunnerEntry{ID: wrong.id, Identity: testRunnerID("exec-wrong"), Conn: wrong})
	ctrl := newRecordingBidiStream(1)
	id := h.execs().add(&execRun{runnerID: identity, control: ctrl})
	fin := &protocol.ExecRunFinished{ExecId: id, Kind: protocol.ExecEventKind_Exited}
	h.handleExecRunFinished(wrong, fin)
	if _, ok := h.execs().get(id); !ok || len(ctrl.Written()) != 0 {
		t.Fatal("another runner completed the exec")
	}
	replacement := &fakeConn{id: tcid("exec-replacement")}
	h.Registry.Add(&RunnerEntry{ID: replacement.id, Identity: identity, Conn: replacement})
	h.handleExecRunFinished(owner, fin)
	if _, ok := h.execs().get(id); !ok || len(ctrl.Written()) != 0 {
		t.Fatal("displaced runner completed the exec")
	}
	h.handleExecRunFinished(replacement, fin)
	if _, ok := h.execs().get(id); ok || !ctrl.Ended() {
		t.Fatal("current owning runner failed to complete the exec")
	}
}
