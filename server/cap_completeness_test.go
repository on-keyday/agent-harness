package server

import (
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The companion to kindTargetClass, for the other half of the gate.
//
// scope_completeness_test.go asks "does this kind name a target?" and
// restore_tasks answered targetGated, correctly, and passed. Nothing asked the
// second question -- "and what capability does it need?" -- so restore_tasks
// shipped with its scope filter and no cap check, and a caller whose prune had
// been revoked could still put records back. Scope says WHICH tasks; only the
// cap says WHETHER. The absences from requiredCap were documented in prose
// above the map, and prose does not go red.
//
// Add a kind and this test fails until it is classified, which forces the
// question to be answered where it can be checked.
type capClass int

const (
	// capInMap: the generic gate in Handle, from requiredCap.
	capInMap capClass = iota
	// capInHandler: gated inside the case or the handler, because the required
	// bit depends on something requiredCap cannot see -- a direction, an attach
	// mode, a list_only flag, a registry lookup, or operator identity.
	capInHandler
	// capNone: needs no capability. Bounded by task VISIBILITY instead, or it
	// reports only the caller's own state, or it is a response kind that never
	// arrives as a request.
	capNone
)

var kindCapClass = map[protocol.TaskControlKind]capClass{
	protocol.TaskControlKind_Submit:           capInMap,
	protocol.TaskControlKind_OpenInteractive:  capInMap,
	protocol.TaskControlKind_Cancel:           capInMap,
	protocol.TaskControlKind_PruneTasks:       capInMap,
	protocol.TaskControlKind_Notify:           capInMap,
	protocol.TaskControlKind_DialRunner:       capInMap,
	protocol.TaskControlKind_BoardTopics:      capInMap,
	protocol.TaskControlKind_BoardRead:        capInMap,
	protocol.TaskControlKind_BoardPurge:       capInMap,
	protocol.TaskControlKind_BoardRetract:     capInMap,
	protocol.TaskControlKind_BoardSubscribers: capInMap,
	protocol.TaskControlKind_GitQuery:         capInMap,
	protocol.TaskControlKind_OpenExecRun:      capInMap,
	protocol.TaskControlKind_OpenForwardTap:   capInMap,

	// Direction-dependent: file_read or file_write is only known once the
	// request's direction is decoded.
	protocol.TaskControlKind_OpenFileTransfer: capInHandler,
	protocol.TaskControlKind_ListFiles:        capInHandler,
	// forward_local / forward_remote likewise, and kill_port_forward's target
	// direction is known only after the registry lookup.
	protocol.TaskControlKind_OpenPortForward:     capInHandler,
	protocol.TaskControlKind_RegisterPortForward: capInHandler,
	protocol.TaskControlKind_KillPortForward:     capInHandler,
	// The bit depends on the requested AttachMode: attachModeCap.
	protocol.TaskControlKind_AttachSession: capInHandler,
	// exec_run_kill names an EXEC that resolves to a task; authorize(ExecRun).
	protocol.TaskControlKind_ExecRunKill: capInHandler,
	// hasCap(ExecView) then inScope, inside handleGetTaskLog.
	protocol.TaskControlKind_GetTaskLog: capInHandler,
	// Operator-only by principal identity, which is stronger than any cap.
	protocol.TaskControlKind_SetCaps:   capInHandler,
	protocol.TaskControlKind_SetParent: capInHandler,
	// prune, but only on the half that mutates: both halves arrive as this one
	// kind and are told apart by list_only, which requiredCap cannot see. The
	// listing half stays open because the ids of forgotten tasks live only in
	// the WAL.
	protocol.TaskControlKind_RestoreTasks: capInHandler,

	// Bounded by task visibility, not by a bit.
	protocol.TaskControlKind_List:             capNone,
	protocol.TaskControlKind_ListConns:        capNone,
	protocol.TaskControlKind_ListPortForwards: capNone,
	protocol.TaskControlKind_ExecRunList:      capNone,
	// await_idle reports last_output_at, which `ls` already hands to any caller
	// that can see the task; its one side effect, sink=notify, is gated on
	// notify inside the handler.
	protocol.TaskControlKind_AwaitIdle: capNone,
	// The caller's own connection and identity.
	protocol.TaskControlKind_ClientHello: capNone,
	protocol.TaskControlKind_Whoami:      capNone,
	// A RESPONSE kind; it never arrives as a request.
	protocol.TaskControlKind_PermissionDenied: capNone,
}

func TestEveryTaskControlKindHasACapVerdict(t *testing.T) {
	for i := 0; i <= int(protocol.TaskControlKind_RestoreTasks); i++ {
		k := protocol.TaskControlKind(i)
		if k.String() == fmt.Sprintf("TaskControlKind(%d)", i) {
			continue // gap in the enum, not a real kind
		}
		if _, ok := kindCapClass[k]; !ok {
			t.Errorf("TaskControlKind %v has no capability verdict.\n"+
				"Add it to kindCapClass. If it needs a bit that does not depend on the "+
				"request's contents, put it in requiredCap; if it does depend on them, gate "+
				"it in the handler and say so here. An unclassified kind dispatches with "+
				"whatever gate its neighbours happen to have -- which is how restore_tasks "+
				"shipped able to mutate the task list with the cap revoked.", k)
		}
	}
}

// The table and the map have to agree in BOTH directions, or the table is a
// second place the answer is written down rather than a check on the first.
func TestCapClassAgreesWithRequiredCap(t *testing.T) {
	for k, class := range kindCapClass {
		_, inMap := requiredCap[k]
		switch class {
		case capInMap:
			if !inMap {
				t.Errorf("%v is classified capInMap but is absent from requiredCap: it dispatches ungated", k)
			}
		case capInHandler, capNone:
			if inMap {
				t.Errorf("%v is classified %d but IS in requiredCap; reclassify it as capInMap", k, class)
			}
		}
	}
	for k := range requiredCap {
		if kindCapClass[k] != capInMap {
			t.Errorf("%v is in requiredCap but not classified capInMap", k)
		}
	}
}
