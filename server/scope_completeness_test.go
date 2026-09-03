package server

import (
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A capability names a verb. Until scopes existed, nothing named the target,
// and most kinds read the task id straight off the wire — so --caps cancel
// reached every task on the server, --caps file_read every worktree.
//
// The fix is only as good as its coverage, and coverage is invisible: a new
// TaskControlKind that carries a task_id and skips the gate looks exactly like
// one that does not need it. This table makes the decision explicit. Add a
// kind and the test goes red until it is classified, which forces whoever adds
// it to answer "does this name a task?" instead of leaving the answer to the
// next person to read the dispatch loop.
type targetClass int

const (
	// targetGated: carries a target task id; must pass authorize / inScope and
	// answer the kind's own not-found when the target is out of scope.
	targetGated targetClass = iota
	// infoScoped: carries a target but is read-only listing; filtered through
	// visibleToCaller, which is scope widened by info_global.
	infoScoped
	// noTarget: names no task, or names only the caller's own connection.
	noTarget
)

var kindTargetClass = map[protocol.TaskControlKind]targetClass{
	// Spawn paths carry resume_task_id, which names an EXISTING task to take
	// over — see scope_attenuation_test.go for that gate.
	protocol.TaskControlKind_Submit:          targetGated,
	protocol.TaskControlKind_OpenInteractive: targetGated,

	protocol.TaskControlKind_Cancel:              targetGated,
	protocol.TaskControlKind_AttachSession:       targetGated,
	protocol.TaskControlKind_AwaitIdle:           targetGated,
	protocol.TaskControlKind_OpenFileTransfer:    targetGated,
	protocol.TaskControlKind_ListFiles:           targetGated,
	protocol.TaskControlKind_GitQuery:            targetGated,
	protocol.TaskControlKind_OpenPortForward:     targetGated,
	protocol.TaskControlKind_RegisterPortForward: targetGated,
	protocol.TaskControlKind_KillPortForward:     targetGated,
	// open_forward_tap names a FORWARD that resolves to a task, exactly as
	// kill_port_forward does — and it reads that forward's payload, so it must
	// clear the same two gates: visible to the caller, and in the caller's
	// action scope for forward_tap.
	protocol.TaskControlKind_OpenForwardTap: targetGated,
	// open_exec_run names a task; exec_run_kill names an EXEC that resolves to
	// one, and gates on that task exactly as kill_port_forward does with a
	// forward id.
	protocol.TaskControlKind_OpenExecRun: targetGated,
	protocol.TaskControlKind_ExecRunKill: targetGated,
	// prune names a set of tasks, or sweeps by age; filtered to the caller's
	// scope in the PruneFn closure.
	protocol.TaskControlKind_PruneTasks: targetGated,

	protocol.TaskControlKind_List:             infoScoped,
	protocol.TaskControlKind_ListConns:        infoScoped,
	protocol.TaskControlKind_GetTaskLog:       infoScoped,
	protocol.TaskControlKind_ListPortForwards: infoScoped,
	// exec_run_list is bounded by task VISIBILITY and needs no capability, for
	// the reason await_idle needs none: gating a fact `ls` already hands out
	// would make the direct path cost more authority than polling for it.
	protocol.TaskControlKind_ExecRunList: infoScoped,

	protocol.TaskControlKind_ClientHello: noTarget,
	protocol.TaskControlKind_Notify:      noTarget,
	protocol.TaskControlKind_DialRunner:  noTarget, // names a runner, not a task
	protocol.TaskControlKind_Whoami:      noTarget, // the caller's own identity
	// Agentboard kinds address topics, not tasks. Topics have no owner, so
	// scope does not apply; info_global / purge remain their gates.
	protocol.TaskControlKind_BoardTopics:      noTarget,
	protocol.TaskControlKind_BoardRead:        noTarget,
	protocol.TaskControlKind_BoardPurge:       noTarget,
	protocol.TaskControlKind_BoardRetract:     noTarget,
	protocol.TaskControlKind_BoardSubscribers: noTarget,
	// set_caps / set_parent name a target task but are operator-only by
	// principal identity, which is strictly stronger than any scope: an
	// operator's scope is global.
	protocol.TaskControlKind_SetCaps:   noTarget,
	protocol.TaskControlKind_SetParent: noTarget,
	// restore_tasks names target ids and is SCOPE-gated on prune, the same bit
	// and target set the destructive half takes. Its walk needs the WAL: a
	// pruned task's creator edge survives only in its task_created record, so
	// walChildIndex feeds scopeSetWith and the ordinary policy decides.
	protocol.TaskControlKind_RestoreTasks: targetGated,
	// permission_denied is a RESPONSE kind; it never arrives as a request.
	protocol.TaskControlKind_PermissionDenied: noTarget,
}

func TestEveryTaskControlKindIsClassified(t *testing.T) {
	for i := 0; i <= int(protocol.TaskControlKind_RestoreTasks); i++ {
		k := protocol.TaskControlKind(i)
		if k.String() == fmt.Sprintf("TaskControlKind(%d)", i) {
			continue // gap in the enum, not a real kind
		}
		if _, ok := kindTargetClass[k]; !ok {
			t.Errorf("TaskControlKind %v is unclassified.\n"+
				"Add it to kindTargetClass. If its request carries a task id, it must "+
				"also pass authorize/inScope before touching the target and answer its "+
				"own not-found status when the target is out of scope — otherwise the "+
				"capability it needs reaches every task on the server.", k)
		}
	}
}

// restore_tasks is the last kind; if the enum grows past it the loop above
// stops short and silently covers nothing new. It caught restore_tasks itself:
// the bound was open_forward_tap and this is what said so.
func TestRestoreTasksIsStillTheLastKind(t *testing.T) {
	next := protocol.TaskControlKind(int(protocol.TaskControlKind_RestoreTasks) + 1)
	if next.String() != fmt.Sprintf("TaskControlKind(%d)", int(next)) {
		t.Fatalf("a kind was appended after restore_tasks (%v) — raise the loop bound in "+
			"TestEveryTaskControlKindIsClassified, which otherwise stops before it", next)
	}
}
