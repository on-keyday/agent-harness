package authority

import (
	"reflect"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	cSpawn    = protocol.Capability_Spawn
	cResume   = protocol.Capability_Resume
	cExact    = protocol.Capability_ResumeExact
	cControl  = protocol.Capability_ExecControl
	cCowrite  = protocol.Capability_ExecCowrite
	cView     = protocol.Capability_ExecView
	cRead     = protocol.Capability_FileRead
	cWrite    = protocol.Capability_FileWrite
	cPersist  = protocol.Capability_RecordPersist
	cDelete   = protocol.Capability_RecordDelete
	cSchedEd  = protocol.Capability_ScheduleEdit
	cSchedRun = protocol.Capability_ScheduleRun
	cPeer     = protocol.Capability_BoardPeer
	cObserve  = protocol.Capability_BoardObserve
	cPersistB = protocol.Capability_BoardPersist
	cMember   = protocol.Capability_BoardMember
	cFwdAll   = protocol.Capability_ForwardLocal | protocol.Capability_ForwardRemote | protocol.Capability_ForwardLink
)

func req(k protocol.TaskControlKind, set func(*protocol.TaskControlRequest)) *protocol.TaskControlRequest {
	r := reqOf(k)
	if set != nil {
		set(r)
	}
	return r
}

func open(id protocol.TaskID, exact, detached, record bool) *protocol.TaskControlRequest {
	return req(protocol.TaskControlKind_OpenInteractive, func(r *protocol.TaskControlRequest) {
		var oi protocol.OpenInteractiveRequest
		oi.ResumeTaskId = id
		oi.SetResumeExact(exact)
		oi.SetDetached(detached)
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

func spec(task bool, record bool) protocol.ScheduleSpec {
	var s protocol.ScheduleSpec
	if task {
		s.SetTaskPresent(true)
		s.SetTaskId(protocol.TaskID{Id: [16]byte{1}})
	}
	if record {
		s.SetRecordSet(true)
		s.SetRecord(true)
	}
	return s
}

func cl(bits, report protocol.Capability) Clause { return Clause{AnyOf: bits, Report: report} }

func TestRequestNeedBranches(t *testing.T) {
	someone := protocol.TaskID{Id: [16]byte{7}}
	scheduleBinding := Later{What: "schedule binding", Possible: cSchedEd, Report: cSchedEd}
	boardReach := func(bit protocol.Capability) Later {
		return Later{What: "board reach", Possible: bit, Report: bit}
	}
	cases := []struct {
		name string
		req  *protocol.TaskControlRequest
		want Need
	}{
		{"attach view", req(protocol.TaskControlKind_AttachSession, func(r *protocol.TaskControlRequest) {
			r.SetAttach(protocol.AttachSessionRequest{Mode: protocol.AttachMode_View})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cView | cCowrite | cControl)}}},
		{"attach control", req(protocol.TaskControlKind_AttachSession, func(r *protocol.TaskControlRequest) {
			r.SetAttach(protocol.AttachSessionRequest{Mode: protocol.AttachMode_Control})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cControl)}}},
		{"file pull", req(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest) {
			r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection_Pull})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cRead)}}},
		{"file dir pull", req(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest) {
			r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection_DirPull})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cRead)}}},
		{"file push", req(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest) {
			r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection_Push})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cWrite)}}},
		{"file mkdir", req(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest) {
			r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection_Mkdir})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cWrite)}}},
		{"list files", reqOf(protocol.TaskControlKind_ListFiles),
			Need{Class: NeedBits, Clauses: []Clause{cl(cRead|cWrite, cRead)}}},
		{"open port forward", reqOf(protocol.TaskControlKind_OpenPortForward),
			Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_ForwardLocal)}}},
		{"register remote", req(protocol.TaskControlKind_RegisterPortForward, func(r *protocol.TaskControlRequest) {
			r.SetRegisterPortForward(protocol.RegisterPortForwardRequest{Direction: protocol.PortForwardDirection_Remote})
		}), Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_ForwardRemote)}}},
		{"register link", req(protocol.TaskControlKind_RegisterPortForward, func(r *protocol.TaskControlRequest) {
			r.SetRegisterPortForward(protocol.RegisterPortForwardRequest{Direction: protocol.PortForwardDirection_Link})
		}), Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_ForwardLink)}}},
		{"record on", req(protocol.TaskControlKind_RecordSwitch, func(r *protocol.TaskControlRequest) {
			var rs protocol.RecordSwitchRequest
			rs.SetOn(true)
			r.SetRecordSwitch(rs)
		}), Need{Class: NeedBits, Clauses: []Clause{same(cPersist)}}},
		{"record off", reqOf(protocol.TaskControlKind_RecordSwitch),
			Need{Class: NeedBits, Clauses: []Clause{same(cDelete)}}},
		{"restore list only", req(protocol.TaskControlKind_RestoreTasks, func(r *protocol.TaskControlRequest) {
			r.SetRestoreTasks(protocol.RestoreTasksRequest{ListOnly: 1})
		}), Need{Class: NeedNone}},
		{"restore", reqOf(protocol.TaskControlKind_RestoreTasks),
			Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_Prune)}}},
		// The record clause is answered with RecordNotPermitted, not
		// PermissionDenied: Report 0.
		{"spawn attached recorded", open(protocol.TaskID{}, false, false, true),
			Need{Class: NeedBits, Clauses: []Clause{same(cSpawn), same(cControl), cl(cPersist, 0)}}},
		{"exact resume detached", open(someone, true, true, false),
			Need{Class: NeedBits, Clauses: []Clause{same(cResume | cExact)}, Later: []Later{scheduleBinding}}},
		{"resume attached", open(someone, false, false, false),
			Need{Class: NeedBits, Clauses: []Clause{same(cResume), same(cControl)}, Later: []Later{scheduleBinding}}},
		{"await idle reply", reqOf(protocol.TaskControlKind_AwaitIdle), Need{Class: NeedNone}},
		{"await idle notify", req(protocol.TaskControlKind_AwaitIdle, func(r *protocol.TaskControlRequest) {
			r.SetAwaitIdle(protocol.AwaitIdleRequest{Sink: protocol.AwaitSink_Notify})
		}), Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_Notify)}}},
		{"await progress board", req(protocol.TaskControlKind_AwaitProgress, func(r *protocol.TaskControlRequest) {
			r.SetAwaitProgress(protocol.AwaitProgressRequest{Sink: protocol.AwaitSink_Board})
		}), Need{Class: NeedBits, Later: []Later{boardReach(cPeer)}}},
		{"session mode", reqOf(protocol.TaskControlKind_SessionMode),
			Need{Class: NeedBits, Clauses: []Clause{same(protocol.Capability_Cancel)}, Later: []Later{scheduleBinding}}},
		{"kill forward", reqOf(protocol.TaskControlKind_KillPortForward),
			Need{Class: NeedBits, Later: []Later{{What: "forward direction", Possible: cFwdAll}}}},
		// Never PermissionDenied: a missing bit answers NotFound.
		{"exec kill", reqOf(protocol.TaskControlKind_ExecRunKill),
			Need{Class: NeedBits, Clauses: []Clause{cl(protocol.Capability_ExecRun, 0)}}},
		{"record keep", reqOf(protocol.TaskControlKind_RecordKeep),
			Need{Class: NeedBits, Clauses: []Clause{cl(cPersist|cDelete, cPersist)},
				Later: []Later{{What: "record retention", Possible: cPersist | cDelete}}}},
		{"schedule create unbound recorded", req(protocol.TaskControlKind_ScheduleCreate, func(r *protocol.TaskControlRequest) {
			r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: spec(false, true)})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cSchedEd), same(cSpawn), cl(cPersist, 0)}}},
		{"schedule create binding", req(protocol.TaskControlKind_ScheduleCreate, func(r *protocol.TaskControlRequest) {
			r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: spec(true, false)})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cSchedEd), same(cResume)}}},
		{"schedule edit recorded", req(protocol.TaskControlKind_ScheduleEdit, func(r *protocol.TaskControlRequest) {
			r.SetScheduleEdit(protocol.ScheduleEditRequest{Spec: spec(false, true)})
		}), Need{Class: NeedBits, Clauses: []Clause{same(cSchedEd), cl(cPersist, 0)},
			Later: []Later{{What: "schedule state", Possible: cSpawn | cResume}}}},
		{"schedule run", reqOf(protocol.TaskControlKind_ScheduleRun),
			Need{Class: NeedBits, Clauses: []Clause{same(cSchedRun)},
				Later: []Later{{What: "schedule state", Possible: cSpawn | cResume | cExact}}}},
		{"board retain", reqOf(protocol.TaskControlKind_BoardRetain),
			Need{Class: NeedBits, Clauses: []Clause{same(cPersistB)}, Later: []Later{{What: "board reach"}}}},
		{"board membership", reqOf(protocol.TaskControlKind_BoardMembership),
			Need{Class: NeedBits, Clauses: []Clause{same(cMember)}, Later: []Later{{What: "grant reach"}}}},
		{"agent send", reqOf(protocol.TaskControlKind_AgentSend), Need{Class: NeedBits, Later: []Later{boardReach(cPeer)}}},
		{"agent subscribe", reqOf(protocol.TaskControlKind_AgentSubscribe), Need{Class: NeedBits, Later: []Later{boardReach(cObserve)}}},
		{"agent wait", reqOf(protocol.TaskControlKind_AgentWait), Need{Class: NeedBits, Later: []Later{boardReach(cObserve)}}},
	}
	for _, c := range cases {
		if got := RequestNeed(c.req); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: RequestNeed =\n  %+v\nwant\n  %+v", c.name, got, c.want)
		}
	}
}

// Possible is what a consumer with no request in hand reads. A zero-bodied
// OpenFileTransfer is a Push, so building one would answer file_write alone and
// hide `file pull` from a file_read holder.
func TestPossibleCoversEveryBranch(t *testing.T) {
	if got := Possible(protocol.TaskControlKind_OpenFileTransfer); got != cRead|cWrite {
		t.Errorf("Possible(OpenFileTransfer) = %v, want file_read|file_write", got)
	}
	if got := Possible(protocol.TaskControlKind_OpenInteractive); got != cSpawn|cResume|cExact|cControl|cPersist|cSchedEd {
		t.Errorf("Possible(OpenInteractive) = %v", got)
	}
	if got := Possible(protocol.TaskControlKind_List); got != 0 {
		t.Errorf("Possible(List) = %v, want none", got)
	}
	for k := protocol.TaskControlKind(0); k <= lastKind; k++ {
		p := Possible(k)
		for _, r := range variants(k) {
			n := RequestNeed(r)
			for _, c := range n.Clauses {
				if c.AnyOf&^p != 0 {
					t.Errorf("%v: clause %v outside Possible %v", k, c.AnyOf, p)
				}
			}
			for _, l := range n.Later {
				if l.Possible&^p != 0 {
					t.Errorf("%v: later %q %v outside Possible %v", k, l.What, l.Possible, p)
				}
			}
		}
	}
}

// ScopeBit answers only where the REQUEST decides which bit's scope binds the
// target. Elsewhere it says so (ok=false) instead of guessing: a central
// kind's scope is its central bit where it names a target at all, an await is
// bounded by visibility, a schedule bind passes two scopes.
func TestScopeBit(t *testing.T) {
	check := func(name string, r *protocol.TaskControlRequest, held, want protocol.Capability, wantOK bool) {
		t.Helper()
		if got, ok := ScopeBit(r, held); got != want || ok != wantOK {
			t.Errorf("%s: ScopeBit = (%v, %t), want (%v, %t)", name, got, ok, want, wantOK)
		}
	}
	lf := reqOf(protocol.TaskControlKind_ListFiles)
	check("list files, file_write held", lf, cWrite, cWrite, true)
	check("list files, both held", lf, cRead|cWrite, cRead, true)
	check("attach cowrite", req(protocol.TaskControlKind_AttachSession, func(r *protocol.TaskControlRequest) {
		r.SetAttach(protocol.AttachSessionRequest{Mode: protocol.AttachMode_Cowrite})
	}), protocol.Capability_All, cCowrite, true)
	check("file pull", req(protocol.TaskControlKind_OpenFileTransfer, func(r *protocol.TaskControlRequest) {
		r.SetOpenFileTransfer(protocol.OpenFileTransferRequest{Direction: protocol.FileTransferDirection_Pull})
	}), protocol.Capability_All, cRead, true)
	// Listing what could be restored needs no bit, and is still filtered by
	// prune's scope: the halves share one target set.
	check("restore list-only", req(protocol.TaskControlKind_RestoreTasks, func(r *protocol.TaskControlRequest) {
		r.SetRestoreTasks(protocol.RestoreTasksRequest{ListOnly: 1})
	}), 0, protocol.Capability_Prune, true)
	check("record keep", reqOf(protocol.TaskControlKind_RecordKeep), 0, cPersist|cDelete, true)
	check("exact resume", open(protocol.TaskID{Id: [16]byte{7}}, true, true, false), 0, cResume|cExact, true)

	check("cancel (central)", reqOf(protocol.TaskControlKind_Cancel), protocol.Capability_All, 0, false)
	check("fresh spawn (no target)", open(protocol.TaskID{}, false, true, false), protocol.Capability_All, 0, false)
	check("await notify (visibility)", req(protocol.TaskControlKind_AwaitIdle, func(r *protocol.TaskControlRequest) {
		r.SetAwaitIdle(protocol.AwaitIdleRequest{Sink: protocol.AwaitSink_Notify})
	}), protocol.Capability_All, 0, false)
	check("schedule bind (two scopes)", req(protocol.TaskControlKind_ScheduleCreate, func(r *protocol.TaskControlRequest) {
		r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: spec(true, false)})
	}), protocol.Capability_All, 0, false)
	check("kill forward (lookup)", reqOf(protocol.TaskControlKind_KillPortForward), protocol.Capability_All, 0, false)
}

// KindNeed serves a handler that holds only the decoded body. It is the need
// of a request with no body, so it must refuse a kind whose need the body
// decides -- that caller would read the wrong branch.
func TestKindNeed(t *testing.T) {
	if got := KindNeed(protocol.TaskControlKind_ExecRunKill); !reflect.DeepEqual(got, RequestNeed(reqOf(protocol.TaskControlKind_ExecRunKill))) {
		t.Errorf("KindNeed(ExecRunKill) = %+v", got)
	}
	if got := KindNeed(protocol.TaskControlKind_AgentSend).Later[0].Report; got != cPeer {
		t.Errorf("KindNeed(AgentSend) label = %v, want board_peer", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("KindNeed(OpenFileTransfer) did not panic: its need depends on the direction")
		}
	}()
	KindNeed(protocol.TaskControlKind_OpenFileTransfer)
}

func TestLookupHelpers(t *testing.T) {
	keep := []struct {
		name       string
		forever    bool
		ttl        time.Duration
		curForever bool
		curTTL     time.Duration
		want       protocol.Capability
	}{
		{"to forever", true, 0, false, time.Hour, cPersist},
		{"later expiry", false, 2 * time.Hour, false, time.Hour, cPersist},
		{"earlier expiry", false, time.Minute, false, time.Hour, cDelete},
		{"leaving forever", false, 10 * time.Hour, true, 0, cDelete},
	}
	for _, c := range keep {
		if got := RecordKeepCap(c.forever, c.ttl, c.curForever, c.curTTL); got != c.want {
			t.Errorf("RecordKeepCap %s = %v, want %v", c.name, got, c.want)
		}
	}
	// The request facts RequestNeed reads, which the server reads through the
	// same functions (recordForOpen, applySpec, scheduleSpecTask).
	if oi := open(protocol.TaskID{}, false, true, true).OpenInteractive(); !OpenAsksRecord(oi) {
		t.Error("OpenAsksRecord: an open with record on did not ask")
	}
	if oi := open(protocol.TaskID{}, false, true, false).OpenInteractive(); OpenAsksRecord(oi) {
		t.Error("OpenAsksRecord: an open saying nothing asked")
	}
	if s := spec(true, true); !SpecBinds(&s) || !SpecAsksRecord(&s) {
		t.Error("a spec binding a task and recording: SpecBinds / SpecAsksRecord false")
	}
	if s := spec(false, false); SpecBinds(&s) || SpecAsksRecord(&s) {
		t.Error("an empty spec: SpecBinds / SpecAsksRecord true")
	}
	if RestoreCap(1) != 0 || RestoreCap(0) != protocol.Capability_Prune {
		t.Errorf("RestoreCap: list_only %v, restore %v", RestoreCap(1), RestoreCap(0))
	}
	if got := ScheduleRunPower(protocol.ScheduleTaskState_Bound); !reflect.DeepEqual(got, []Clause{same(cResume | cExact)}) {
		t.Errorf("ScheduleRunPower(bound) = %+v", got)
	}
	if got := ScheduleRunPower(protocol.ScheduleTaskState_PendingCreate); !reflect.DeepEqual(got, []Clause{same(cSpawn)}) {
		t.Errorf("ScheduleRunPower(pending) = %+v", got)
	}
	if got := ScheduleRunPower(protocol.ScheduleTaskState_Unbound); got != nil {
		t.Errorf("ScheduleRunPower(unbound) = %+v, want nil", got)
	}
	if got := SchedulePower(false, false, false); got != nil {
		t.Errorf("SchedulePower(unbound, no bind) = %+v, want nil", got)
	}
	if got := SchedulePower(true, false, true); !reflect.DeepEqual(got, []Clause{same(cResume)}) {
		t.Errorf("SchedulePower(new, bind) = %+v, want resume only (a bind creates nothing)", got)
	}
	if got := SchedulePower(false, true, false); !reflect.DeepEqual(got, []Clause{same(cResume)}) {
		t.Errorf("SchedulePower(bound) = %+v", got)
	}
}
