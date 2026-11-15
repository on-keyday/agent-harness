package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// S-D17a: a schedule verb also needs the power it exercises.

func powerGrant(caps protocol.Capability, resumeBase protocol.ScopeBase) authority.Grant {
	g := authority.Grant{
		Caps:  caps | protocol.Capability_Schedule | protocol.Capability_FileRead,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}, DelegateCaps: protocol.Capability_All,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global},
	}
	g.Scope.Overrides = []authority.ScopeOverride{{Caps: protocol.Capability_Resume, Base: resumeBase}}
	return g
}

func wantDenied(t *testing.T, what string, r protocol.ScheduleResponse, c, want protocol.Capability) {
	t.Helper()
	if c != want {
		t.Errorf("%s: denied for %v (status %v), want %v", what, c, r.Status, want)
	}
}

func wantAllowed(t *testing.T, what string, r protocol.ScheduleResponse, c protocol.Capability, want protocol.ScheduleStatus) {
	t.Helper()
	if c != 0 || r.Status != want {
		t.Errorf("%s: denied %v, status %v (%s), want %v", what, c, r.Status, r.Message, want)
	}
}

func TestSchedulePowerCreate(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	task := e.terminal(t, protocol.RunMode_Background, "")

	// No --task: spawn.
	conn, _ := e.agent(t, powerGrant(0, protocol.ScopeBase_Global))
	r, c := e.createDeny(e.connIDOf(conn), createSpec(), "go")
	wantDenied(t, "create without spawn", r, c, protocol.Capability_Spawn)
	conn, _ = e.agent(t, powerGrant(protocol.Capability_Spawn, protocol.ScopeBase_Global))
	r, c = e.createDeny(e.connIDOf(conn), createSpec(), "go")
	wantAllowed(t, "create with spawn", r, c, protocol.ScheduleStatus_Ok)

	// --task T: resume, not spawn, and not resume_exact.
	for name, caps := range map[string]protocol.Capability{
		"spawn only":   protocol.Capability_Spawn,
		"resume_exact": protocol.Capability_ResumeExact,
	} {
		conn, _ = e.agent(t, powerGrant(caps, protocol.ScopeBase_Global))
		r, c = e.createDeny(e.connIDOf(conn), bindSpec(t, createSpec(), e.terminal(t, protocol.RunMode_Background, "")), "go")
		wantDenied(t, "create --task, "+name, r, c, protocol.Capability_Resume)
	}
	// resume scoped so T is outside.
	conn, _ = e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_None))
	r, c = e.createDeny(e.connIDOf(conn), bindSpec(t, createSpec(), task), "go")
	if c != 0 || r.Status != protocol.ScheduleStatus_TaskNotReachable {
		t.Errorf("create --task outside resume scope: denied %v status %v, want task_not_reachable", c, r.Status)
	}
	conn, _ = e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_Global))
	r, c = e.createDeny(e.connIDOf(conn), bindSpec(t, createSpec(), task), "go")
	wantAllowed(t, "create --task with resume over T", r, c, protocol.ScheduleStatus_Ok)
}

func TestSchedulePowerEdit(t *testing.T) {
	e := newSchedEnv(t)
	maker := e.terminal(t, protocol.RunMode_Background, "") // creator of the seeded schedules
	e.addCandidate("claude")
	task := e.terminal(t, protocol.RunMode_Background, "")
	other := e.terminal(t, protocol.RunMode_Background, "")
	cronSpec := func() *protocol.ScheduleSpec {
		sp := &protocol.ScheduleSpec{}
		sp.SetCronPresent(true)
		sp.SetCron([]byte("0 10 * * *"))
		return sp
	}
	bind := func() *protocol.ScheduleSpec {
		sp := &protocol.ScheduleSpec{}
		sp.SetTaskPresent(true)
		sp.SetTaskId(hexToTaskID(t, other))
		return sp
	}
	schedOnly, _ := e.agent(t, powerGrant(0, protocol.ScopeBase_Global))
	sid := e.connIDOf(schedOnly)

	// Bound schedule: every edit needs resume over its task.
	bound := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	r, c := e.editDeny(t, sid, bound.ID, cronSpec(), "")
	wantDenied(t, "edit --cron of a bound schedule without resume", r, c, protocol.Capability_Resume)
	outside, _ := e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_None))
	r, c = e.editDeny(t, e.connIDOf(outside), bound.ID, cronSpec(), "")
	if c != 0 || r.Status != protocol.ScheduleStatus_TaskNotReachable {
		t.Errorf("edit of a bound schedule, task outside resume scope: %v %v", c, r.Status)
	}
	withResume, _ := e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_Global))
	r, c = e.editDeny(t, e.connIDOf(withResume), bound.ID, cronSpec(), "")
	wantAllowed(t, "edit --cron of a bound schedule with resume", r, c, protocol.ScheduleStatus_Ok)

	// A rebind needs resume over the new task too (here: over T only).
	oneTask, _ := e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_Global))
	oneTask2 := e.connIDOf(oneTask)
	un := e.seed(t, 2, protocol.ScheduleTaskState_Unbound, "", maker)
	r, c = e.editDeny(t, sid, un.ID, bind(), "")
	wantDenied(t, "edit binding T without resume", r, c, protocol.Capability_Resume)
	r, c = e.editDeny(t, oneTask2, un.ID, bind(), "")
	wantAllowed(t, "edit binding T with resume", r, c, protocol.ScheduleStatus_Ok)

	// Pending create: spawn.
	pend := e.seed(t, 3, protocol.ScheduleTaskState_PendingCreate, "", maker)
	r, c = e.editDeny(t, sid, pend.ID, cronSpec(), "")
	wantDenied(t, "edit of a pending_create schedule without spawn", r, c, protocol.Capability_Spawn)
	withSpawn, _ := e.agent(t, powerGrant(protocol.Capability_Spawn, protocol.ScopeBase_Global))
	r, c = e.editDeny(t, e.connIDOf(withSpawn), pend.ID, cronSpec(), "")
	wantAllowed(t, "edit of a pending_create schedule with spawn", r, c, protocol.ScheduleStatus_Ok)

	// Unbound, nothing bound: schedule alone.
	un2 := e.seed(t, 4, protocol.ScheduleTaskState_Unbound, "", maker)
	r, c = e.editDeny(t, sid, un2.ID, cronSpec(), "")
	wantAllowed(t, "edit of an unbound schedule with only schedule", r, c, protocol.ScheduleStatus_Ok)
}

func TestSchedulePowerRun(t *testing.T) {
	e := newSchedEnv(t)
	maker := e.terminal(t, protocol.RunMode_Background, "") // creator of the seeded schedules
	task := e.terminal(t, protocol.RunMode_Background, "")
	schedOnly, _ := e.agent(t, powerGrant(0, protocol.ScopeBase_Global))
	sid := e.connIDOf(schedOnly)
	run := func(cid, id string) (protocol.ScheduleResponse, protocol.Capability) {
		return e.h.scheduleRun(cid, schedHexID(t, id))
	}
	bound := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	r, c := run(sid, bound.ID)
	wantDenied(t, "run of a bound schedule without resume", r, c, protocol.Capability_Resume)
	withResume, _ := e.agent(t, powerGrant(protocol.Capability_Resume, protocol.ScopeBase_Global))
	r, c = run(e.connIDOf(withResume), bound.ID)
	wantAllowed(t, "run of a bound schedule with resume", r, c, protocol.ScheduleStatus_Ok)

	pend := e.seed(t, 2, protocol.ScheduleTaskState_PendingCreate, "", maker)
	r, c = run(sid, pend.ID)
	wantDenied(t, "run of a pending_create schedule without spawn", r, c, protocol.Capability_Spawn)
	withSpawn, _ := e.agent(t, powerGrant(protocol.Capability_Spawn, protocol.ScopeBase_Global))
	r, c = run(e.connIDOf(withSpawn), pend.ID)
	wantAllowed(t, "run of a pending_create schedule with spawn", r, c, protocol.ScheduleStatus_Ok)

	un := e.seed(t, 3, protocol.ScheduleTaskState_Unbound, "", maker)
	r, c = run(sid, un.ID)
	wantAllowed(t, "run of an unbound schedule with only schedule", r, c, protocol.ScheduleStatus_Ok)
}

// An out-of-reach schedule answers no_such_schedule before any bit is looked
// at, and the schedule's own task is refused (S-D20) before it too: neither
// reveals which bit is missing.
func TestSchedulePowerAfterReachAndSelf(t *testing.T) {
	e := newSchedEnv(t)
	maker := e.terminal(t, protocol.RunMode_Background, "") // creator of the seeded schedules
	task := e.terminal(t, protocol.RunMode_Background, "")
	none, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_Schedule, Scope: authority.Scope{Base: protocol.ScopeBase_None}})
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	pend := e.seed(t, 2, protocol.ScheduleTaskState_PendingCreate, "", maker)
	cid := e.connIDOf(none)
	for _, id := range []string{sc.ID, pend.ID} {
		if r, c := e.editDeny(t, cid, id, &protocol.ScheduleSpec{}, ""); c != 0 || r.Status != protocol.ScheduleStatus_NoSuchSchedule {
			t.Errorf("edit out of reach: %v %v", c, r.Status)
		}
		if r, c := e.h.scheduleRun(cid, schedHexID(t, id)); c != 0 || r.Status != protocol.ScheduleStatus_NoSuchSchedule {
			t.Errorf("run out of reach: %v %v", c, r.Status)
		}
	}
	selfConn, self := e.agent(t, powerGrant(0, protocol.ScopeBase_Global))
	own := e.seed(t, 3, protocol.ScheduleTaskState_Bound, self, "")
	if r, c := e.editDeny(t, e.connIDOf(selfConn), own.ID, &protocol.ScheduleSpec{}, ""); c != 0 || r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("own-task edit: %v %v", c, r.Status)
	}
	if r, c := e.h.scheduleRun(e.connIDOf(selfConn), schedHexID(t, own.ID)); c != 0 || r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("own-task run: %v %v", c, r.Status)
	}
}

// The S-D20 gap: a bound scheduled task holding schedule but not spawn cannot
// leave a new schedule behind, over the wire as well.
func TestSchedulePowerBoundTaskCannotCreate(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	conn, self := e.agent(t, powerGrant(0, protocol.ScopeBase_Global))
	e.seed(t, 1, protocol.ScheduleTaskState_Bound, self, "")
	e.send(t, conn, protocol.TaskControlKind_ScheduleCreate, func(r *protocol.TaskControlRequest) {
		r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *createSpec()})
	})
	resp := lastTaskControlResponse(t, conn)
	pd := resp.PermissionDenied()
	if resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil ||
		pd.RequestedKind != protocol.TaskControlKind_ScheduleCreate || pd.RequiredCap != protocol.Capability_Spawn {
		t.Fatalf("create by a bound task without spawn: %+v", resp)
	}
	if n := len(e.store.List()); n != 1 {
		t.Fatalf("%d schedules stored, want the one seeded", n)
	}
}
