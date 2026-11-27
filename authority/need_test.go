package authority

import (
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// lastKind is the highest TaskControlKind the protocol defines. The test below
// fails when a kind is added past it, so the bound cannot go stale silently.
const lastKind = protocol.TaskControlKind_HostLog

func reqOf(k protocol.TaskControlKind) *protocol.TaskControlRequest {
	return &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: k}}
}

func TestEveryKindIsClassified(t *testing.T) {
	if s := (lastKind + 1).String(); s != "TaskControlKind(93)" {
		t.Fatalf("kind %d is now %q: a kind was added past lastKind; classify it in RequestNeed and move lastKind", lastKind+1, s)
	}
	for k := protocol.TaskControlKind(0); k <= lastKind; k++ {
		if got := RequestNeed(reqOf(k)).Class; got == NeedUnclassified {
			t.Errorf("%v: RequestNeed does not classify it", k)
		}
	}
}

// The kinds whose whole decision is the central map: no request field, lookup
// or identity adds anything (spec §9, MAP rows).
var pureMapKinds = []protocol.TaskControlKind{
	protocol.TaskControlKind_Cancel,
	protocol.TaskControlKind_PruneTasks,
	protocol.TaskControlKind_DialRunner,
	protocol.TaskControlKind_Notify,
	protocol.TaskControlKind_HostLog,
	protocol.TaskControlKind_BoardTopics,
	protocol.TaskControlKind_BoardRead,
	protocol.TaskControlKind_BoardPurge,
	protocol.TaskControlKind_GitQuery,
	protocol.TaskControlKind_BoardSubscribers,
	protocol.TaskControlKind_OpenExecRun,
	protocol.TaskControlKind_BoardRetract,
	protocol.TaskControlKind_OpenForwardTap,
	protocol.TaskControlKind_BoardSend,
	protocol.TaskControlKind_BoardWake,
	protocol.TaskControlKind_OpenExecTap,
	protocol.TaskControlKind_BoardContactsClear,
	protocol.TaskControlKind_ProgressClear,
	protocol.TaskControlKind_RunnerEnroll,
	protocol.TaskControlKind_RunnerUnenroll,
	protocol.TaskControlKind_RunnerKeys,
	protocol.TaskControlKind_BoardArchives,
	protocol.TaskControlKind_BoardArchiveRead,
	protocol.TaskControlKind_BoardArchivePurge,
	protocol.TaskControlKind_RunnerDialAdd,
	protocol.TaskControlKind_RunnerDialList,
	protocol.TaskControlKind_RunnerDialRemove,
	protocol.TaskControlKind_RunnerKick,
	protocol.TaskControlKind_RecordList,
	protocol.TaskControlKind_RecordGet,
	protocol.TaskControlKind_RecordDelete,
	protocol.TaskControlKind_TranscriptList,
	protocol.TaskControlKind_TranscriptGet,
	protocol.TaskControlKind_ScheduleDelete,
	protocol.TaskControlKind_CredentialIssue,
	protocol.TaskControlKind_CredentialRekey,
}

func TestMapKindsNeedTheirCentralBit(t *testing.T) {
	for _, k := range pureMapKinds {
		bit, ok := CentralCap(k)
		if !ok {
			t.Errorf("%v: not in the central map", k)
			continue
		}
		want := Need{Class: NeedBits, Clauses: []Clause{{AnyOf: bit, Report: bit}}}
		if got := RequestNeed(reqOf(k)); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: RequestNeed = %+v, want %+v", k, got, want)
		}
	}
}

// A central kind that also has a request or lookup part still starts with the
// central clause: the central gate checks it first.
func TestCentralClauseComesFirst(t *testing.T) {
	for k := protocol.TaskControlKind(0); k <= lastKind; k++ {
		bit, ok := CentralCap(k)
		if !ok {
			continue
		}
		n := RequestNeed(reqOf(k))
		if n.Class != NeedBits || len(n.Clauses) == 0 || n.Clauses[0] != (Clause{AnyOf: bit, Report: bit}) {
			t.Errorf("%v: first clause %+v, want the central %v", k, n.Clauses, bit)
		}
	}
}

func TestNoBitKinds(t *testing.T) {
	cases := map[NeedClass][]protocol.TaskControlKind{
		NeedNone: {
			protocol.TaskControlKind_List,
			protocol.TaskControlKind_ListConns,
			protocol.TaskControlKind_Whoami,
			protocol.TaskControlKind_ListPortForwards,
			protocol.TaskControlKind_ExecRunList,
			protocol.TaskControlKind_TrsfState,
			protocol.TaskControlKind_AgentUnsubscribe,
			protocol.TaskControlKind_AgentListSubscriptions,
			protocol.TaskControlKind_AgentInbox,
			protocol.TaskControlKind_AgentInboxAdvance,
			protocol.TaskControlKind_AgentListRetained,
			protocol.TaskControlKind_AgentReadSeq,
			protocol.TaskControlKind_AgentRetract,
			protocol.TaskControlKind_AwaitList,
			protocol.TaskControlKind_AwaitKill,
			protocol.TaskControlKind_AgentListContacts,
			protocol.TaskControlKind_ProgressSet,
			protocol.TaskControlKind_LifecycleSet,
			protocol.TaskControlKind_AgentReadPage,
			protocol.TaskControlKind_AgentArchives,
			protocol.TaskControlKind_AgentArchiveRead,
			protocol.TaskControlKind_ScheduleList,
			protocol.TaskControlKind_ScheduleShow,
		},
		NeedRetired:    {protocol.TaskControlKind_RetiredSubmit, protocol.TaskControlKind_RetiredGetTaskLog},
		NeedNotRequest: {protocol.TaskControlKind_ClientHello, protocol.TaskControlKind_PermissionDenied},
		NeedIdentity:   {protocol.TaskControlKind_SetCaps, protocol.TaskControlKind_SetParent, protocol.TaskControlKind_TaskRename},
	}
	for class, kinds := range cases {
		for _, k := range kinds {
			if _, central := CentralCap(k); central {
				t.Errorf("%v: in the central map, but classified %v", k, class)
			}
			n := RequestNeed(reqOf(k))
			if n.Class != class || len(n.Clauses) != 0 || len(n.Later) != 0 {
				t.Errorf("%v: RequestNeed = %+v, want class %v and nothing else", k, n, class)
			}
		}
	}
}
