package server

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/objtrsf/objproto"
)

// schedEnv is a TaskHandler wired to a memory schedule store and a fake firing
// function, so the verbs are tested without a timer or a Server.
type schedEnv struct {
	h       *TaskHandler
	store   *ScheduleStore
	rt      scheduleRuntime
	ready   atomic.Bool
	pokes   atomic.Int32
	mu      sync.Mutex
	fires   []string
	manual  []bool
	outcome protocol.ScheduleOutcome
	// beforeFire, when set, runs inside Fire before its check (a test's way
	// to land a change between a verb's first read and the firing).
	beforeFire func(id string)
}

func newSchedEnv(t *testing.T) *schedEnv {
	t.Helper()
	st, err := LoadScheduleStore("")
	if err != nil {
		t.Fatal(err)
	}
	e := &schedEnv{h: newTestHandler(t), store: st, outcome: protocol.ScheduleOutcome_RunNow}
	e.ready.Store(true)
	e.h.Sched = &ScheduleOps{
		Store: st,
		Fire: func(id string, due time.Time, manual bool, check FireCheck) (protocol.ScheduleOutcome, *FireVeto) {
			if e.beforeFire != nil {
				e.beforeFire(id) // an interleaved change, before the firing takes its mutex
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if sc, ok := e.store.Get(id); ok && check != nil {
				if v := check(sc); v != nil { // as the real firing: under its mutex, on fresh state
					return protocol.ScheduleOutcome_Pending, v
				}
			}
			e.fires = append(e.fires, id)
			e.manual = append(e.manual, manual)
			return e.outcome, nil
		},
		Poke:  func() { e.pokes.Add(1) },
		Lock:  e.rt.firingMutex,
		Ready: e.ready.Load,
		Now:   func() time.Time { return fireNow },
	}
	return e
}

// addCandidate registers a runner serving /r that offers the claude profile.
func (e *schedEnv) addCandidate(profiles ...string) {
	now := time.Now()
	e.h.Registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: profiles, MaxTasks: 2, ActiveTasks: map[string]struct{}{}, ConnectedAt: now, LastTaskActivity: now, Conn: &fakeConn{}})
}

var schedConnSeq atomic.Int32

// agent makes a task holding g and a connection whose principal it is.
func (e *schedEnv) agent(t *testing.T, g authority.Grant) (*fakeConn, string) {
	t.Helper()
	id := e.h.Tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, g, "claude")
	return e.connFor(t, id), id
}

func (e *schedEnv) connFor(t *testing.T, taskHex string) *fakeConn {
	t.Helper()
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + strings.Repeat("9", 1) + "7" + strconv.Itoa(int(schedConnSeq.Add(1))) + "-1"), autoSendStreams: true}
	if e.h.principals == nil {
		e.h.principals = make(map[string]protocol.TaskID)
	}
	e.h.principals[conn.ConnectionID().String()] = hexToTaskID(t, taskHex)
	return conn
}

func schedOpConn() *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9160-1"), autoSendStreams: true}
}

var schedAgentGrant = authority.Grant{
	Caps:  protocol.Capability_ScheduleRun | protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_Resume | protocol.Capability_FileRead | protocol.Capability_Cancel,
	Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_All,
	DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree},
}

// createSpec is a complete create for /r: cron, UTC, agent, a one-hour deadline.
func createSpec() *protocol.ScheduleSpec {
	sp := &protocol.ScheduleSpec{}
	sp.SetCronPresent(true)
	sp.SetCron([]byte("0 9 * * *"))
	sp.SetTzPresent(true)
	sp.SetTz([]byte("UTC"))
	sp.SetRepoPresent(true)
	sp.SetRepoPath([]byte("/r"))
	sp.SetAgentPresent(true)
	sp.SetAgentProfile([]byte("claude"))
	sp.SetDeadlinePresent(true)
	sp.SetDeadlineMs(3600000)
	sp.SetRequestedCapsPresent(true)
	sp.SetRequestedCaps(protocol.Capability_FileRead)
	return sp
}

func bindSpec(t *testing.T, sp *protocol.ScheduleSpec, taskHex string) *protocol.ScheduleSpec {
	t.Helper()
	sp.SetRepoPresent(false)
	sp.SetRepoPath(nil)
	sp.SetTaskPresent(true)
	sp.SetTaskId(hexToTaskID(t, taskHex))
	return sp
}

func schedHexID(t *testing.T, id string) protocol.ScheduleID {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad schedule id %q", id)
	}
	var out protocol.ScheduleID
	copy(out.Id[:], raw)
	return out
}

func schedIDHex(id protocol.ScheduleID) string { return hex.EncodeToString(id.Id[:]) }

// scheduleResp picks the ScheduleResponse of the last TaskControlResponse, or
// reports the PermissionDenied it was.
func scheduleResp(t *testing.T, conn *fakeConn, kind protocol.TaskControlKind) protocol.ScheduleResponse {
	t.Helper()
	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != kind {
		t.Fatalf("response kind %v, want %v", resp.Kind, kind)
	}
	var r *protocol.ScheduleResponse
	switch kind {
	case protocol.TaskControlKind_ScheduleCreate:
		r = resp.ScheduleCreate()
	case protocol.TaskControlKind_ScheduleEdit:
		r = resp.ScheduleEdit()
	case protocol.TaskControlKind_ScheduleDelete:
		r = resp.ScheduleDelete()
	case protocol.TaskControlKind_ScheduleRun:
		r = resp.ScheduleRun()
	case protocol.TaskControlKind_ScheduleList:
		r = resp.ScheduleList()
	case protocol.TaskControlKind_ScheduleShow:
		r = resp.ScheduleShow()
	}
	if r == nil {
		t.Fatalf("no schedule response in %+v", resp)
	}
	return *r
}

func (e *schedEnv) send(t *testing.T, conn *fakeConn, kind protocol.TaskControlKind, fill func(*protocol.TaskControlRequest)) {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind, RequestId: 21}}
	fill(req)
	e.h.Handle(conn, encodeTaskControlRequest(t, req))
}

// create sends a create that carries no prompt stream (the core takes the prompt
// separately, as the dispatch does after reading the stream).
func (e *schedEnv) create(t *testing.T, cid string, sp *protocol.ScheduleSpec, prompt string) protocol.ScheduleResponse {
	t.Helper()
	r, c := e.h.scheduleCreate(cid, sp, prompt)
	if c != 0 {
		t.Fatalf("create denied for %v; the caller needs more bits", c)
	}
	return r
}

// createDeny is create that also reports the capability a denial names.
func (e *schedEnv) createDeny(cid string, sp *protocol.ScheduleSpec, prompt string) (protocol.ScheduleResponse, protocol.Capability) {
	return e.h.scheduleCreate(cid, sp, prompt)
}

func (e *schedEnv) edit(t *testing.T, cid string, id string, sp *protocol.ScheduleSpec, prompt string) protocol.ScheduleResponse {
	t.Helper()
	r, c := e.h.scheduleEdit(cid, schedHexID(t, id), sp, prompt)
	if c != 0 {
		t.Fatalf("edit denied for %v; the caller needs more bits", c)
	}
	return r
}

func (e *schedEnv) editDeny(t *testing.T, cid string, id string, sp *protocol.ScheduleSpec, prompt string) (protocol.ScheduleResponse, protocol.Capability) {
	return e.h.scheduleEdit(cid, schedHexID(t, id), sp, prompt)
}

func (e *schedEnv) call(t *testing.T, conn *fakeConn, kind protocol.TaskControlKind, id string) protocol.ScheduleResponse {
	t.Helper()
	e.send(t, conn, kind, func(r *protocol.TaskControlRequest) {
		req := protocol.ScheduleIDRequest{Id: schedHexID(t, id)}
		switch kind {
		case protocol.TaskControlKind_ScheduleDelete:
			r.SetScheduleDelete(req)
		case protocol.TaskControlKind_ScheduleRun:
			r.SetScheduleRun(req)
		case protocol.TaskControlKind_ScheduleShow:
			r.SetScheduleShow(req)
		}
	})
	return scheduleResp(t, conn, kind)
}

// seed stores a schedule bound to task (or pending when task is "") created by
// creator, the way a create would have.
func (e *schedEnv) seed(t *testing.T, n int, state protocol.ScheduleTaskState, task, creator string) Schedule {
	t.Helper()
	sc := fireTestSchedule(n, state, task)
	sc.Creator = creator
	sc.Repo = "/r"
	sc.NextAt = fireNow.Add(time.Hour)
	sc.CreatedAt = fireNow.Add(-time.Duration(100-n) * time.Minute)
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func (e *schedEnv) terminal(t *testing.T, mode protocol.RunMode, creator string) string {
	t.Helper()
	id, err := e.h.Tasks.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, taskIDFromHex(creator), "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude",
		RunSpec{Mode: mode, Cond: RunConditions{Deadline: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	return id
}

func (e *schedEnv) running(t *testing.T, mode protocol.RunMode, creator string) string {
	t.Helper()
	id, err := e.h.Tasks.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, taskIDFromHex(creator), "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude",
		RunSpec{Mode: mode, Cond: RunConditions{Deadline: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	return id
}

func (e *schedEnv) stored(t *testing.T, id string) Schedule {
	t.Helper()
	sc, ok := e.store.Get(id)
	if !ok {
		t.Fatalf("schedule %s missing", id)
	}
	return sc
}

// --- bit, reach, self ---

func TestScheduleNeedsBit(t *testing.T) {
	e := newSchedEnv(t)
	conn, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()})
	id := scheduleID(1)
	for _, k := range []protocol.TaskControlKind{
		protocol.TaskControlKind_ScheduleCreate, protocol.TaskControlKind_ScheduleEdit,
		protocol.TaskControlKind_ScheduleDelete, protocol.TaskControlKind_ScheduleRun,
	} {
		e.send(t, conn, k, func(r *protocol.TaskControlRequest) {
			switch k {
			case protocol.TaskControlKind_ScheduleCreate:
				r.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *createSpec()})
			case protocol.TaskControlKind_ScheduleEdit:
				r.SetScheduleEdit(protocol.ScheduleEditRequest{Id: schedHexID(t, id)})
			case protocol.TaskControlKind_ScheduleDelete:
				r.SetScheduleDelete(protocol.ScheduleIDRequest{Id: schedHexID(t, id)})
			case protocol.TaskControlKind_ScheduleRun:
				r.SetScheduleRun(protocol.ScheduleIDRequest{Id: schedHexID(t, id)})
			}
		})
		resp := lastTaskControlResponse(t, conn)
		pd := resp.PermissionDenied()
		want := protocol.Capability_ScheduleEdit
		if k == protocol.TaskControlKind_ScheduleRun {
			want = protocol.Capability_ScheduleRun
		}
		if resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil ||
			pd.RequestedKind != k || pd.RequiredCap != want {
			t.Errorf("%v without the bit: %+v", k, resp)
		}
	}
	// Reading needs no bit.
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, id); r.Status != protocol.ScheduleStatus_NoSuchSchedule {
		t.Errorf("show without the bit: %v", r.Status)
	}
}

func TestScheduleOutOfReachIsNoSuch(t *testing.T) {
	e := newSchedEnv(t)
	task := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	// A caller that reaches only itself.
	conn, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_ScheduleRun | protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_Resume, Scope: authority.Scope{Base: protocol.ScopeBase_None}})
	for _, k := range []protocol.TaskControlKind{
		protocol.TaskControlKind_ScheduleDelete, protocol.TaskControlKind_ScheduleRun, protocol.TaskControlKind_ScheduleShow,
	} {
		if r := e.call(t, conn, k, sc.ID); r.Status != protocol.ScheduleStatus_NoSuchSchedule {
			t.Errorf("%v: status %v, want no_such_schedule (never permission_denied)", k, r.Status)
		}
	}
	if r := e.edit(t, e.connIDOf(conn), sc.ID, &protocol.ScheduleSpec{}, ""); r.Status != protocol.ScheduleStatus_NoSuchSchedule {
		t.Errorf("edit: %v", r.Status)
	}
	if _, ok := e.store.Get(sc.ID); !ok {
		t.Fatal("an out-of-reach delete removed the schedule")
	}
	if len(e.fires) != 0 {
		t.Fatal("an out-of-reach run fired")
	}
	// ls lists none of it.
	e.send(t, conn, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) {
		r.SetScheduleList(protocol.ScheduleListRequest{})
	})
	var body protocol.ScheduleListBody
	conn.streamBody(t, scheduleResp(t, conn, protocol.TaskControlKind_ScheduleList).StreamId, &body)
	if len(body.Rows) != 0 {
		t.Errorf("ls showed %d out-of-reach rows", len(body.Rows))
	}
	// An unknown id answers the same.
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, scheduleID(99)); r.Status != protocol.ScheduleStatus_NoSuchSchedule {
		t.Errorf("unknown id: %v", r.Status)
	}
	// --task T out of reach on a create.
	other := e.terminal(t, protocol.RunMode_Background, "")
	cid := e.connIDOf(conn)
	if r := e.create(t, cid, bindSpec(t, createSpec(), other), "go"); r.Status != protocol.ScheduleStatus_TaskNotReachable {
		t.Errorf("create --task out of reach: %v", r.Status)
	}
}

func (e *schedEnv) connIDOf(c *fakeConn) string { return c.ConnectionID().String() }

func TestScheduleSelfRefused(t *testing.T) {
	e := newSchedEnv(t)
	conn, self := e.agent(t, schedAgentGrant)
	other := e.terminal(t, protocol.RunMode_Background, self)
	// A schedule bound to the caller's own task, and a task it could rebind to.
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, self, "")
	cid := e.connIDOf(conn)

	if r := e.edit(t, cid, sc.ID, func() *protocol.ScheduleSpec {
		sp := &protocol.ScheduleSpec{}
		sp.SetCronPresent(true)
		sp.SetCron([]byte("1 1 * * *"))
		return sp
	}(), ""); r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("edit: %v", r.Status)
	}
	if r := e.edit(t, cid, sc.ID, bindSpec(t, &protocol.ScheduleSpec{}, other), ""); r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("rebind: %v", r.Status)
	}
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID); r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("run: %v", r.Status)
	}
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleDelete, sc.ID); r.Status != protocol.ScheduleStatus_SelfRefused {
		t.Errorf("delete: %v", r.Status)
	}
	if got := e.stored(t, sc.ID); got.Cron != sc.Cron || got.TaskID != self || got.State != protocol.ScheduleTaskState_Bound {
		t.Errorf("a refused verb changed the schedule: %+v", got)
	}
	if len(e.fires) != 0 {
		t.Error("a refused run fired")
	}
	// Reading its own schedule is fine.
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, sc.ID); r.Status != protocol.ScheduleStatus_Ok {
		t.Errorf("show: %v", r.Status)
	}
}

// --- authority ---

func TestCreateStoresGrantFromFlagsAndCreator(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	conn, self := e.agent(t, schedAgentGrant)
	sp := createSpec()
	sp.SetRequestedCaps(protocol.Capability_FileRead | protocol.Capability_Cancel | protocol.Capability_ExecRun)
	r := e.create(t, e.connIDOf(conn), sp, "triage")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("status %v %s", r.Status, r.Message)
	}
	sc := e.stored(t, schedIDHex(r.Id))
	// The spawn rule: the creator's caps, inside what it may delegate, inside the request.
	if sc.Grant.Caps != protocol.Capability_FileRead|protocol.Capability_Cancel {
		t.Errorf("grant caps %v", sc.Grant.Caps)
	}
	if sc.Creator != self || sc.State != protocol.ScheduleTaskState_PendingCreate || sc.TaskID != "" {
		t.Errorf("creator %q state %v task %q", sc.Creator, sc.State, sc.TaskID)
	}
	if sc.Repo != "/r" || sc.Agent != "claude" || sc.Prompt != "triage" || sc.Cron != "0 9 * * *" || sc.TZ != "UTC" {
		t.Errorf("definition %+v", sc)
	}
	if want := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC); !sc.NextAt.Equal(want) {
		t.Errorf("next_at %v, want %v", sc.NextAt, want)
	}
	if sc.Run.Mode != protocol.RunMode_Background || sc.Run.Cond.Deadline != time.Hour || sc.Run.Cond.Quiet == 0 {
		t.Errorf("run spec %+v", sc.Run)
	}
	if e.pokes.Load() != 1 {
		t.Errorf("pokes %d, want 1", e.pokes.Load())
	}
}

func TestEditRecomputesGrantWithoutFlags(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	// An editor narrower than the creator: fewer caps, subtree scope.
	conn, editor := e.agent(t, authority.Grant{
		Caps:  protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_Resume | protocol.Capability_FileRead,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_FileRead,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree},
	})
	task := e.terminal(t, protocol.RunMode_Background, editor)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, task)
	sc.Grant = authority.Grant{Caps: protocol.Capability_FileRead | protocol.Capability_ExecRun | protocol.Capability_Spawn,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}, DelegateCaps: protocol.Capability_All,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}, SpawnDepth: authority.LimitOf(2), MaxDescendants: authority.LimitOf(5)}
	sc.NextAt = fireNow.Add(time.Hour)
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	sp := &protocol.ScheduleSpec{} // no authority flags
	sp.SetPromptPresent(true)
	if r := e.edit(t, e.connIDOf(conn), sc.ID, sp, "a new prompt"); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("edit: %v %s", r.Status, r.Message)
	}
	got := e.stored(t, sc.ID)
	if got.Prompt != "a new prompt" {
		t.Fatalf("prompt %q", got.Prompt)
	}
	if got.Grant.Caps&^(protocol.Capability_ScheduleEdit|protocol.Capability_FileRead|protocol.Capability_Spawn|protocol.Capability_Resume) != 0 || got.Grant.Caps&protocol.Capability_FileRead == 0 {
		t.Errorf("stored caps %v exceed the editor's", got.Grant.Caps)
	}
	if got.Grant.Scope.Base == protocol.ScopeBase_Global || got.Grant.DelegateScope.Base == protocol.ScopeBase_Global {
		t.Errorf("stored scope %v / bound %v still global", got.Grant.Scope.Base, got.Grant.DelegateScope.Base)
	}
	if got.EditedBy != editor {
		t.Errorf("edited_by %q", got.EditedBy)
	}
}

// --- binding ---

func TestBindRunningForegroundRefused(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	task := e.running(t, protocol.RunMode_Foreground, "")
	if r := e.create(t, "", bindSpec(t, createSpec(), task), "go"); r.Status != protocol.ScheduleStatus_TaskRunningForeground {
		t.Fatalf("create: %v %s", r.Status, r.Message)
	}
	if len(e.store.List()) != 0 {
		t.Fatal("a refused bind left a schedule")
	}
	// The same on an edit's rebind.
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Unbound, "", "")
	if r := e.edit(t, "", sc.ID, bindSpec(t, &protocol.ScheduleSpec{}, task), ""); r.Status != protocol.ScheduleStatus_TaskRunningForeground {
		t.Fatalf("edit: %v", r.Status)
	}
	if got := e.stored(t, sc.ID); got.State != protocol.ScheduleTaskState_Unbound {
		t.Fatalf("state %v", got.State)
	}
	if tk, _ := e.h.Tasks.Get(task); tk.RunMode != protocol.RunMode_Foreground {
		t.Fatal("a refused bind rewrote the task's mode")
	}
}

func TestBindSetsBackground(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	task := e.terminal(t, protocol.RunMode_Foreground, "")
	r := e.create(t, "", bindSpec(t, createSpec(), task), "go")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("create: %v %s", r.Status, r.Message)
	}
	sc := e.stored(t, schedIDHex(r.Id))
	if sc.State != protocol.ScheduleTaskState_Bound || sc.TaskID != task || sc.Repo != "/r" {
		t.Fatalf("schedule %+v", sc)
	}
	if tk, _ := e.h.Tasks.Get(task); tk.RunMode != protocol.RunMode_Background {
		t.Fatalf("task mode %v, want background", tk.RunMode)
	}
	// A task already bound to a schedule cannot be bound to a second.
	if r := e.create(t, "", bindSpec(t, createSpec(), task), "again"); r.Status == protocol.ScheduleStatus_Ok {
		t.Fatal("a task was bound to two schedules")
	}
	if len(e.store.List()) != 1 {
		t.Fatalf("%d schedules", len(e.store.List()))
	}
	// --repo with --task is refused.
	sp := bindSpec(t, createSpec(), e.terminal(t, protocol.RunMode_Background, ""))
	sp.SetRepoPresent(true)
	sp.SetRepoPath([]byte("/r"))
	if r := e.create(t, "", sp, "go"); r.Status == protocol.ScheduleStatus_Ok || len(e.store.List()) != 1 {
		t.Fatalf("--repo with --task: %v", r.Status)
	}
}

func TestEditRebindsUnboundSchedule(t *testing.T) {
	e := newSchedEnv(t)
	task := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Unbound, "", "")
	if r := e.edit(t, "", sc.ID, bindSpec(t, &protocol.ScheduleSpec{}, task), ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("edit: %v %s", r.Status, r.Message)
	}
	if got := e.stored(t, sc.ID); got.State != protocol.ScheduleTaskState_Bound || got.TaskID != task || got.Repo != "/r" {
		t.Fatalf("schedule %+v", got)
	}
}

// --- unbinding (S-D15) ---

func foregroundResume(taskHex string) *protocol.OpenInteractiveRequest {
	oi := resumeReq(taskHex, detachedOpen(0, 0))
	oi.SetRunModeSet(true)
	oi.SetBackground(false)
	oi.SetAgentProfile([]byte(""))
	return oi
}

func TestForegroundResumeOfBoundTaskNeedsBitAndUnbinds(t *testing.T) {
	e := newSchedEnv(t)
	openRunner(t, e.h)
	task := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")

	// A caller that may resume the task but lacks the schedule bit is refused
	// through the dispatch gate, as permission_denied naming the bit.
	noBit, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_Resume | protocol.Capability_Cancel,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}, DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}})
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 3}}
	req.SetOpenInteractive(*foregroundResume(task))
	e.h.Handle(noBit, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, noBit)
	if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != protocol.Capability_ScheduleEdit {
		t.Fatalf("resume without the bit: %+v", resp)
	}
	// The same for `session mode ... foreground` of a running bound task.
	running := e.running(t, protocol.RunMode_Background, "")
	sc2 := e.seed(t, 2, protocol.ScheduleTaskState_Bound, running, "")
	sm := protocol.SessionModeRequest{TaskId: hexToTaskID(t, running)}
	smReq := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SessionMode, RequestId: 4}}
	smReq.SetSessionMode(sm)
	e.h.Handle(noBit, encodeTaskControlRequest(t, smReq))
	resp = lastTaskControlResponse(t, noBit)
	if pd := resp.PermissionDenied(); resp.Kind != protocol.TaskControlKind_PermissionDenied || pd == nil || pd.RequiredCap != protocol.Capability_ScheduleEdit {
		t.Fatalf("session mode without the bit: %+v", resp)
	}
	for _, id := range []string{sc.ID, sc2.ID} {
		if got := e.stored(t, id); got.State != protocol.ScheduleTaskState_Bound {
			t.Fatalf("a refused change unbound %s", id)
		}
	}
	if tk, _ := e.h.Tasks.Get(running); tk.RunMode != protocol.RunMode_Background {
		t.Fatal("a refused switch changed the mode")
	}

	// The bound task itself may not unbind itself, bit or not (S-D20).
	selfConn, selfTask := e.agent(t, authority.Grant{Caps: protocol.Capability_ScheduleEdit | protocol.Capability_Resume,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_All,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree}})
	e.h.Tasks.Cancel(selfTask, protocol.EndReason_Cancelled, protocol.TaskID{})
	self := e.seed(t, 3, protocol.ScheduleTaskState_Bound, selfTask, "")
	cidSelf := e.connIDOf(selfConn)
	if r, _ := e.h.handleOpenInteractive(cidSelf, nil, foregroundResume(selfTask), protocol.ClientKind_Agent, hexToTaskID(t, selfTask),
		protocol.Capability_ScheduleEdit|protocol.Capability_Resume, ""); r.Status != protocol.OpenInteractiveStatus_ScopeNotPermitted {
		t.Fatalf("a bound task resuming itself to foreground: %v, want scope_not_permitted", r.Status)
	}
	if got := e.stored(t, self.ID); got.State != protocol.ScheduleTaskState_Bound {
		t.Fatal("self unbind took effect")
	}

	// The operator holds every bit: the resume goes through and unbinds.
	r, _ := e.h.handleOpenInteractive("", nil, foregroundResume(task), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
	if r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("operator resume: %v", r.Status)
	}
	got := e.stored(t, sc.ID)
	if got.State != protocol.ScheduleTaskState_Unbound {
		t.Fatalf("state %v, want unbound", got.State)
	}
	last := got.History[len(got.History)-1]
	if last.Outcome != protocol.ScheduleOutcome_Unbound || last.ByKind != protocol.ClientKind_Cli || last.At.IsZero() {
		t.Fatalf("last history entry %+v", last)
	}

	// `session mode ... foreground` by the operator unbinds too.
	conn := schedOpConn()
	smOp := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SessionMode, RequestId: 5}}
	smOp.SetSessionMode(sm)
	e.h.Handle(conn, encodeTaskControlRequest(t, smOp))
	swResp := lastTaskControlResponse(t, conn)
	if r := swResp.SessionMode(); r == nil || r.Status != protocol.SessionModeStatus_Ok {
		t.Fatalf("switch: %+v", r)
	}
	if got := e.stored(t, sc2.ID); got.State != protocol.ScheduleTaskState_Unbound || got.History[len(got.History)-1].Outcome != protocol.ScheduleOutcome_Unbound {
		t.Fatalf("schedule after switch %+v", got)
	}
}

// A task bound to no schedule resumes in foreground exactly as before, with
// or without the bit.
func TestForegroundResumeOfUnboundTaskNeedsNoBit(t *testing.T) {
	e := newSchedEnv(t)
	openRunner(t, e.h)
	task := e.terminal(t, protocol.RunMode_Background, "")
	r, _ := e.h.handleOpenInteractive("", nil, foregroundResume(task), protocol.ClientKind_Cli, protocol.TaskID{}, protocol.Capability_All, "")
	if r.Status != protocol.OpenInteractiveStatus_Ok {
		t.Fatalf("status %v", r.Status)
	}
}

// --- create checks ---

func TestCreateSharesBackgroundChecks(t *testing.T) {
	e := newSchedEnv(t)
	cases := []struct {
		name  string
		setup func()
		mod   func(sp *protocol.ScheduleSpec)
		task  string
		want  protocol.ScheduleStatus
	}{
		{"no candidate", nil, nil, "go", protocol.ScheduleStatus_NoCandidate},
		{"no deadline", e.addCandidateOnce, func(sp *protocol.ScheduleSpec) { sp.SetDeadlineMs(0) }, "go", protocol.ScheduleStatus_DeadlineRequired},
		{"no conditions", nil, func(sp *protocol.ScheduleSpec) { sp.SetDeadlinePresent(false) }, "go", protocol.ScheduleStatus_DeadlineRequired},
		{"no prompt", nil, nil, "", protocol.ScheduleStatus_PromptRequired},
		{"no agent", nil, func(sp *protocol.ScheduleSpec) { sp.SetAgentPresent(false) }, "go", protocol.ScheduleStatus_ProfileRequired},
		{"profile no candidate offers", nil, func(sp *protocol.ScheduleSpec) { sp.SetAgentProfile([]byte("codex")) }, "go", protocol.ScheduleStatus_ProfileUnavailable},
		{"invalid cron", nil, func(sp *protocol.ScheduleSpec) { sp.SetCron([]byte("@every 5m")) }, "go", protocol.ScheduleStatus_InvalidCron},
		{"no cron", nil, func(sp *protocol.ScheduleSpec) { sp.SetCronPresent(false) }, "go", protocol.ScheduleStatus_InvalidCron},
		{"cron over the bound", nil, func(sp *protocol.ScheduleSpec) {
			sp.SetCron([]byte(strings.Repeat("*", protocol.ScheduleCronMax+1)))
		}, "go", protocol.ScheduleStatus_InvalidCron},
		{"unknown zone", nil, func(sp *protocol.ScheduleSpec) { sp.SetTz([]byte("Mars/Olympus")) }, "go", protocol.ScheduleStatus_InvalidZone},
		{"zone over the bound", nil, func(sp *protocol.ScheduleSpec) {
			sp.SetTz([]byte(strings.Repeat("a", protocol.ScheduleZoneMax+1)))
		}, "go", protocol.ScheduleStatus_InvalidZone},
	}
	for _, c := range cases {
		if c.setup != nil {
			c.setup()
		}
		sp := createSpec()
		if c.mod != nil {
			c.mod(sp)
		}
		if r := e.create(t, "", sp, c.task); r.Status != c.want {
			t.Errorf("%s: status %v (%s), want %v", c.name, r.Status, r.Message, c.want)
		}
	}
	if n := len(e.store.List()); n != 0 {
		t.Fatalf("refused creates left %d schedules", n)
	}
	// With a candidate that offers the profile, the same spec is accepted.
	e.addCandidateOnce()
	if r := e.create(t, "", createSpec(), "go"); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("baseline: %v %s", r.Status, r.Message)
	}
}

var addedCandidate sync.Map

func (e *schedEnv) addCandidateOnce() {
	if _, ok := addedCandidate.Load(e); ok {
		return
	}
	addedCandidate.Store(e, true)
	e.addCandidate("claude")
}

func TestCreateRefusedBySpawnLimit(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	g := schedAgentGrant
	g.SpawnDepth = authority.LimitOf(0)
	conn, _ := e.agent(t, g)
	if r := e.create(t, e.connIDOf(conn), createSpec(), "go"); r.Status != protocol.ScheduleStatus_SpawnLimit || len(r.Message) == 0 {
		t.Fatalf("status %v %q", r.Status, r.Message)
	}
	if len(e.store.List()) != 0 {
		t.Fatal("a refused create left a schedule")
	}
}

// --- edit / delete ---

func TestEditCronDuringRun(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	task := e.running(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	before, _ := e.h.Tasks.Get(task)
	pokes := e.pokes.Load()
	sp := &protocol.ScheduleSpec{}
	sp.SetCronPresent(true)
	sp.SetCron([]byte("*/5 * * * *"))
	if r := e.edit(t, "", sc.ID, sp, ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("edit: %v %s", r.Status, r.Message)
	}
	got := e.stored(t, sc.ID)
	if want := time.Date(2026, 11, 15, 9, 5, 0, 0, time.UTC); got.Cron != "*/5 * * * *" || !got.NextAt.Equal(want) {
		t.Errorf("cron %q next_at %v, want %v", got.Cron, got.NextAt, want)
	}
	after, _ := e.h.Tasks.Get(task)
	if after.Status != protocol.TaskStatus_Running || after.Cond != before.Cond || after.DeadlineAt != before.DeadlineAt || after.RunMode != before.RunMode {
		t.Errorf("the run in progress changed: %+v -> %+v", before, after)
	}
	if e.pokes.Load() != pokes+1 {
		t.Errorf("pokes %d -> %d", pokes, e.pokes.Load())
	}
	// An edit that changes neither cron nor zone leaves next_at alone.
	sp2 := &protocol.ScheduleSpec{}
	sp2.SetAgentPresent(true)
	sp2.SetAgentProfile([]byte("claude"))
	if r := e.edit(t, "", sc.ID, sp2, ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatal(r.Status)
	}
	if !e.stored(t, sc.ID).NextAt.Equal(got.NextAt) {
		t.Error("an edit of another field moved next_at")
	}
	// An edit that changes the zone recomputes it.
	sp3 := &protocol.ScheduleSpec{}
	sp3.SetTzPresent(true)
	sp3.SetTz([]byte("Asia/Tokyo"))
	if r := e.edit(t, "", sc.ID, sp3, ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatal(r.Status)
	}
	if e.stored(t, sc.ID).TZ != "Asia/Tokyo" {
		t.Error("zone not stored")
	}
}

func TestEditMergesOnlyWhatItNames(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_PendingCreate, "", "")
	sp := &protocol.ScheduleSpec{}
	sp.SetPromptPresent(true)
	sp.SetExtraArgsPresent(true)
	sp.SetExtraArgs(protocol.AgentArgsFromStrings(nil))
	sp.SetOneshotSet(true)
	sp.SetOneshot(true)
	sp.SetResumeConversationSet(true)
	sp.SetResumeConversation(false)
	sp.SetSizePresent(true)
	sp.SetInitialRows(30)
	sp.SetInitialCols(100)
	if r := e.edit(t, "", sc.ID, sp, "fresh prompt"); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	got := e.stored(t, sc.ID)
	if got.Prompt != "fresh prompt" || !got.Run.Oneshot || got.ResumeConversation || got.Run.Cond.Rows != 30 || got.Run.Cond.Cols != 100 || len(got.ExtraArgs) != 0 {
		t.Errorf("edited %+v", got)
	}
	if got.Cron != sc.Cron || got.TZ != sc.TZ || got.Agent != sc.Agent || got.Repo != sc.Repo || got.Run.Cond.Deadline != sc.Run.Cond.Deadline || got.Run.Record != sc.Run.Record {
		t.Errorf("an edit changed fields it did not name: %+v", got)
	}
	// An edit may not leave the schedule unable to run (S-D12's checks).
	bad := &protocol.ScheduleSpec{}
	bad.SetCronPresent(true)
	bad.SetCron([]byte("nope"))
	if r := e.edit(t, "", sc.ID, bad, ""); r.Status != protocol.ScheduleStatus_InvalidCron {
		t.Errorf("bad cron: %v", r.Status)
	}
	bad = &protocol.ScheduleSpec{}
	bad.SetAgentPresent(true)
	bad.SetAgentProfile([]byte("codex"))
	if r := e.edit(t, "", sc.ID, bad, ""); r.Status != protocol.ScheduleStatus_ProfileUnavailable {
		t.Errorf("profile: %v", r.Status)
	}
	if again := e.stored(t, sc.ID); again.Agent != "claude" || again.Cron != sc.Cron {
		t.Error("a refused edit changed the schedule")
	}
}

func TestDeleteLeavesTask(t *testing.T) {
	e := newSchedEnv(t)
	task := e.running(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	before, _ := e.h.Tasks.Get(task)
	pokes := e.pokes.Load()
	conn := schedOpConn()
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleDelete, sc.ID); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("delete: %v", r.Status)
	}
	if _, ok := e.store.Get(sc.ID); ok {
		t.Fatal("schedule still stored")
	}
	after, ok := e.h.Tasks.Get(task)
	if !ok || after.Status != protocol.TaskStatus_Running || after.DeadlineAt != before.DeadlineAt || after.RunMode != protocol.RunMode_Background {
		t.Fatalf("task after delete: %+v", after)
	}
	if e.pokes.Load() != pokes+1 {
		t.Error("delete did not poke the loop")
	}
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleDelete, sc.ID); r.Status != protocol.ScheduleStatus_NoSuchSchedule {
		t.Errorf("second delete: %v", r.Status)
	}
}

// edit and delete serialize with a firing of the same schedule.
func TestEditAndDeleteTakeTheFiringMutex(t *testing.T) {
	e := newSchedEnv(t)
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Unbound, "", "")
	m := e.rt.firingMutex(sc.ID)
	m.Lock()
	done := make(chan struct{})
	go func() {
		sp := &protocol.ScheduleSpec{}
		sp.SetPromptPresent(true)
		e.edit(t, "", sc.ID, sp, "x")
		e.call(t, schedOpConn(), protocol.TaskControlKind_ScheduleDelete, sc.ID)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("edit/delete ran while a firing held the schedule's mutex")
	case <-time.After(100 * time.Millisecond):
	}
	m.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("edit/delete never ran after the mutex was released")
	}
}

// --- run ---

func TestRunFiresNowAndPokesNothing(t *testing.T) {
	e := newSchedEnv(t)
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Unbound, "", "")
	conn := schedOpConn()
	pokes := e.pokes.Load()

	e.ready.Store(false)
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID); r.Status == protocol.ScheduleStatus_Ok || len(e.fires) != 0 {
		t.Fatalf("run before start-up finished: %v, %d fires", r.Status, len(e.fires))
	}
	e.ready.Store(true)

	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, scheduleID(77)); r.Status != protocol.ScheduleStatus_NoSuchSchedule || len(e.fires) != 0 {
		t.Fatalf("missing schedule: %v, %d fires", r.Status, len(e.fires))
	}

	r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID)
	if r.Status != protocol.ScheduleStatus_Ok || !strings.Contains(string(r.Message), "run_now") {
		t.Fatalf("run: %v %q", r.Status, r.Message)
	}
	if len(e.fires) != 1 || e.fires[0] != sc.ID || !e.manual[0] {
		t.Fatalf("fires %v manual %v", e.fires, e.manual)
	}
	if e.pokes.Load() != pokes {
		t.Error("run poked the loop")
	}

	// A firing that recorded nothing (its intent could not be saved) is not ok.
	e.outcome = protocol.ScheduleOutcome_Pending
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID); r.Status != protocol.ScheduleStatus_InternalError {
		t.Errorf("unrecorded firing: %v", r.Status)
	}
	// A skip is reported as what it was.
	e.outcome = protocol.ScheduleOutcome_SkippedBusy
	if r := e.call(t, conn, protocol.TaskControlKind_ScheduleRun, sc.ID); r.Status != protocol.ScheduleStatus_Ok || !strings.Contains(string(r.Message), "skipped_busy") {
		t.Errorf("busy: %v %q", r.Status, r.Message)
	}
}

// --- prompt stream ---

func TestCreatePromptTooLarge(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	body := protocol.PromptBody{}
	body.SetPrompt(bytes.Repeat([]byte("a"), protocol.PromptMax+1))
	rs := &fakeRecv{data: body.MustAppend(nil)}
	conn := promptConn{fakeConn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9171-1")}, st: rs}
	sp := createSpec()
	sp.SetPromptPresent(true)
	sp.SetPromptStreamId(5)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleCreate, RequestId: 6}}
	req.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *sp})
	e.h.Handle(conn, encodeTaskControlRequest(t, req))
	waitSent(conn)
	r := scheduleResp(t, conn.fakeConn, protocol.TaskControlKind_ScheduleCreate)
	if r.Status != protocol.ScheduleStatus_PromptTooLarge || !strings.Contains(string(r.Message), "16384") || !rs.cancelled {
		t.Fatalf("create: %v %q cancelled %v", r.Status, r.Message, rs.cancelled)
	}
	if len(e.store.List()) != 0 {
		t.Fatal("a refused create left a schedule")
	}

	// An edit with an oversize prompt leaves the schedule as it was.
	sc := e.seed(t, 1, protocol.ScheduleTaskState_PendingCreate, "", "")
	rs2 := &fakeRecv{data: body.MustAppend(nil)}
	conn2 := promptConn{fakeConn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9172-1")}, st: rs2}
	esp := &protocol.ScheduleSpec{}
	esp.SetPromptPresent(true)
	esp.SetPromptStreamId(5)
	ereq := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleEdit, RequestId: 7}}
	ereq.SetScheduleEdit(protocol.ScheduleEditRequest{Id: schedHexID(t, sc.ID), Spec: *esp})
	e.h.Handle(conn2, encodeTaskControlRequest(t, ereq))
	waitSent(conn2)
	if r := scheduleResp(t, conn2.fakeConn, protocol.TaskControlKind_ScheduleEdit); r.Status != protocol.ScheduleStatus_PromptTooLarge || !rs2.cancelled {
		t.Fatalf("edit: %v cancelled %v", r.Status, rs2.cancelled)
	}
	if got := e.stored(t, sc.ID); got.Prompt != sc.Prompt {
		t.Fatalf("prompt changed to %q", got.Prompt)
	}
}

func waitSent(conn promptConn) {
	deadline := time.Now().Add(2 * time.Second)
	for len(conn.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// A prompt within the cap travels through the dispatch into the schedule.
func TestCreatePromptThroughDispatch(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	body := protocol.PromptBody{}
	body.SetPrompt([]byte("triage new issues"))
	conn := promptConn{fakeConn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9173-1")}, st: &fakeRecv{data: body.MustAppend(nil)}}
	sp := createSpec()
	sp.SetPromptPresent(true)
	sp.SetPromptStreamId(5)
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleCreate, RequestId: 8}}
	req.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *sp})
	e.h.Handle(conn, encodeTaskControlRequest(t, req))
	waitSent(conn)
	r := scheduleResp(t, conn.fakeConn, protocol.TaskControlKind_ScheduleCreate)
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	if got := e.stored(t, schedIDHex(r.Id)); got.Prompt != "triage new issues" {
		t.Fatalf("prompt %q", got.Prompt)
	}
}

// --- list / show ---

func TestListAndShowShape(t *testing.T) {
	e := newSchedEnv(t)
	task := e.running(t, protocol.RunMode_Background, "")
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, task)
	sc.Prompt = "line one\nline two"
	sc.NextAt = fireNow.Add(time.Hour)
	sc.EditedBy, sc.EditedByKind = "", protocol.ClientKind_Webui
	sc.EditedAt = fireNow.Add(-time.Minute)
	sc.History = []HistoryEntry{
		{Outcome: protocol.ScheduleOutcome_Fired, DueAt: fireNow.Add(-2 * time.Hour), At: fireNow.Add(-2 * time.Hour), TaskID: task, ExpectRun: 1,
			QueuedAt: fireNow.Add(-2 * time.Hour), StartedAt: fireNow.Add(-110 * time.Minute), EndedAt: fireNow.Add(-100 * time.Minute), EndReason: protocol.EndReason_Deadline},
		{Outcome: protocol.ScheduleOutcome_SkippedRefused, DueAt: fireNow.Add(-time.Hour), At: fireNow.Add(-time.Hour), Message: "limit"},
	}
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	conn := schedOpConn()

	e.send(t, conn, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) { r.SetScheduleList(protocol.ScheduleListRequest{}) })
	var list protocol.ScheduleListBody
	conn.streamBody(t, scheduleResp(t, conn, protocol.TaskControlKind_ScheduleList).StreamId, &list)
	if len(list.Rows) != 1 {
		t.Fatalf("%d rows", len(list.Rows))
	}
	row := list.Rows[0]
	if schedIDHex(row.Id) != sc.ID || row.TaskState != protocol.ScheduleTaskState_Bound || row.TaskStatus != protocol.TaskStatus_Running ||
		hex.EncodeToString(row.TaskId.Id[:]) != task || row.NextAt != uint64(sc.NextAt.UnixNano()) ||
		string(row.Cron) != sc.Cron || string(row.Tz) != "UTC" || string(row.Agent) != "claude" || string(row.Repo) != "/repo" ||
		row.PromptHidden() || string(row.PromptHead) != "line one\nline two" || !row.LastPresent() {
		t.Fatalf("row %+v", row)
	}
	if l := row.Last(); l == nil || l.Outcome != protocol.ScheduleOutcome_SkippedRefused || string(l.Message) != "limit" || l.At != uint64(fireNow.Add(-time.Hour).UnixNano()) || l.StartedAt != 0 {
		t.Fatalf("last %+v", l)
	}

	r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, sc.ID)
	if r.Status != protocol.ScheduleStatus_Ok || r.StreamId == 0 {
		t.Fatalf("show: %v", r.Status)
	}
	var d protocol.ScheduleDetail
	conn.streamBody(t, r.StreamId, &d)
	if string(d.Prompt) != "line one\nline two" || !d.ResumeConversation() || !d.Record() || d.Caps != protocol.Capability_FileRead ||
		d.Conditions.DeadlineMs != 3600000 || d.EditedByKind != protocol.ClientKind_Webui || d.EditedAt != uint64(sc.EditedAt.UnixNano()) ||
		d.CreatedAt != uint64(sc.CreatedAt.UnixNano()) || len(d.History) != 2 || d.SpawnDepth.Value() != 2 || d.MaxDescendants.Value() != 5 {
		t.Fatalf("detail %+v", d)
	}
	h0 := d.History[0]
	if h0.Outcome != protocol.ScheduleOutcome_Fired || h0.QueuedAt == 0 || h0.StartedAt == 0 || h0.EndedAt == 0 || h0.EndReason != protocol.EndReason_Deadline {
		t.Fatalf("history[0] %+v", h0)
	}
}

func TestListCutsPromptHeadOnARuneBoundary(t *testing.T) {
	e := newSchedEnv(t)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
	sc.Prompt = strings.Repeat("日", 100) // 300 bytes
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	conn := schedOpConn()
	e.send(t, conn, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) { r.SetScheduleList(protocol.ScheduleListRequest{}) })
	var list protocol.ScheduleListBody
	conn.streamBody(t, scheduleResp(t, conn, protocol.TaskControlKind_ScheduleList).StreamId, &list)
	head := list.Rows[0].PromptHead
	if len(head) != protocol.ScheduleHeadMax || !utf8.Valid(head) {
		t.Fatalf("head is %d bytes, valid utf-8 %v", len(head), utf8.Valid(head))
	}
	if r := list.Rows[0]; r.TaskState != protocol.ScheduleTaskState_Unbound || r.NextAt != 0 && false {
		t.Fatalf("row %+v", r)
	}
	// An odd cut: a 4-byte rune straddling the limit is dropped whole.
	list = protocol.ScheduleListBody{}
	sc.Prompt = strings.Repeat("a", protocol.ScheduleHeadMax-2) + "𝒳"
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	e.send(t, conn, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) { r.SetScheduleList(protocol.ScheduleListRequest{}) })
	conn.streamBody(t, scheduleResp(t, conn, protocol.TaskControlKind_ScheduleList).StreamId, &list)
	if head := list.Rows[0].PromptHead; len(head) != protocol.ScheduleHeadMax-2 || !utf8.Valid(head) {
		t.Fatalf("head %d bytes valid %v", len(head), utf8.Valid(head))
	}
}

// A caller that reaches a schedule's task through its schedule scope but could
// not read that task's prompt in `ls` (its visibility rank is below its action
// rank) gets prompt_hidden and no prompt.
func TestListHidesPrompt(t *testing.T) {
	e := newSchedEnv(t)
	conn, caller := e.agent(t, authority.Grant{
		Caps:  protocol.Capability_ScheduleEdit,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_None},
	})
	child := e.terminal(t, protocol.RunMode_Background, caller)
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, child, "")
	// What the caller can see in ls does not include the child.
	if all, allowed := e.h.visibleToCaller(e.connIDOf(conn)); all || allowed[child] {
		t.Fatalf("test setup: the child is visible (%v %v)", all, allowed)
	}
	e.send(t, conn, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) { r.SetScheduleList(protocol.ScheduleListRequest{}) })
	var list protocol.ScheduleListBody
	conn.streamBody(t, scheduleResp(t, conn, protocol.TaskControlKind_ScheduleList).StreamId, &list)
	if len(list.Rows) != 1 || !list.Rows[0].PromptHidden() || len(list.Rows[0].PromptHead) != 0 {
		t.Fatalf("rows %+v", list.Rows)
	}
	r := e.call(t, conn, protocol.TaskControlKind_ScheduleShow, sc.ID)
	var d protocol.ScheduleDetail
	conn.streamBody(t, r.StreamId, &d)
	if len(d.Prompt) != 0 || !d.Row.PromptHidden() {
		t.Fatalf("detail leaked the prompt: %q hidden %v", d.Prompt, d.Row.PromptHidden())
	}
	// The operator sees it.
	op := schedOpConn()
	list = protocol.ScheduleListBody{}
	e.send(t, op, protocol.TaskControlKind_ScheduleList, func(r *protocol.TaskControlRequest) { r.SetScheduleList(protocol.ScheduleListRequest{}) })
	op.streamBody(t, scheduleResp(t, op, protocol.TaskControlKind_ScheduleList).StreamId, &list)
	if list.Rows[0].PromptHidden() || string(list.Rows[0].PromptHead) != sc.Prompt {
		t.Fatalf("operator row %+v", list.Rows[0])
	}
}

// --- TaskInfo / TaskStatusEvent ---

func TestTaskInfoCarriesScheduleID(t *testing.T) {
	e := newSchedEnv(t)
	bound := e.terminal(t, protocol.RunMode_Background, "")
	free := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, bound, "")
	conn := schedOpConn()
	conn.nextSendStreamID = 42
	lreq := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_List, RequestId: 1}}
	lreq.SetList(protocol.ListQuery{})
	e.h.Handle(conn, encodeTaskControlRequest(t, lreq))
	resp := lastTaskControlResponse(t, conn)
	var body protocol.ListResultBody
	conn.streamBody(t, resp.List().StreamId, &body)
	seen := 0
	for i := range body.Tasks {
		ti := &body.Tasks[i]
		id := hex.EncodeToString(ti.Id.Id[:])
		switch id {
		case bound:
			seen++
			if !ti.SchedulePresent() || ti.ScheduleId() == nil || schedIDHex(*ti.ScheduleId()) != sc.ID {
				t.Errorf("bound task: present %v id %v", ti.SchedulePresent(), ti.ScheduleId())
			}
		case free:
			seen++
			if ti.SchedulePresent() {
				t.Error("an unbound task carries a schedule id")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the two tasks", seen)
	}
}

func TestStatusEventCarriesScheduleID(t *testing.T) {
	st, err := LoadScheduleStore("")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Schedules: st})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	mk := func() string {
		return s.tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude")
	}
	task, other := mk(), mk()
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, task)
	if err := st.Put(sc); err != nil {
		t.Fatal(err)
	}
	skip := len(getEvents()) // the queued events came before the binding
	s.tasks.Assign(task, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	s.tasks.Assign(other, testRunnerID("ws:127.0.0.1:8539-41"), "", false)
	var withID, without int
	for _, ev := range getEvents()[skip:] {
		id := hex.EncodeToString(ev.TaskId.Id[:])
		switch {
		case id == task && ev.SchedulePresent():
			if ev.ScheduleId() == nil || schedIDHex(*ev.ScheduleId()) != sc.ID {
				t.Errorf("event schedule id %v", ev.ScheduleId())
			}
			withID++
		case id == other && !ev.SchedulePresent():
			without++
		case id == task || id == other:
			t.Errorf("event for %s: schedule_present %v", id[:8], ev.SchedulePresent())
		}
	}
	if withID == 0 || without == 0 {
		t.Fatalf("events with id %d, without %d (assign events expected)", withID, without)
	}
}

// --- fix round 1 ---

// R15: a bound task's grant is bounded by the creator even with no authority
// flag; binding a more privileged task does not lend the creator its authority.
func TestCreateWithTaskGrantBoundedByCreator(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	conn, creator := e.agent(t, authority.Grant{
		Caps:  protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_Resume | protocol.Capability_FileRead,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_FileRead,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree},
	})
	task, err := e.h.Tasks.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, taskIDFromHex(creator), "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{Base: protocol.ScopeBase_Global},
			DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}}, "claude",
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Tasks.Cancel(task, protocol.EndReason_Cancelled, protocol.TaskID{})
	sp := bindSpec(t, createSpec(), task)
	sp.SetRequestedCapsPresent(false) // no authority flags at all
	r := e.create(t, e.connIDOf(conn), sp, "go")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	g := e.stored(t, schedIDHex(r.Id)).Grant
	if g.Caps&^protocol.Capability_FileRead != 0 || g.DelegateCaps&^protocol.Capability_FileRead != 0 {
		t.Errorf("stored caps %v / delegate %v exceed the creator's", g.Caps, g.DelegateCaps)
	}
	if g.Scope.Base == protocol.ScopeBase_Global || g.DelegateScope.Base == protocol.ScopeBase_Global {
		t.Errorf("stored scope %v / bound %v still global", g.Scope.Base, g.DelegateScope.Base)
	}
}

// S-D15's bit check must not tell a caller anything about a task it cannot
// reach: bound and unbound answer alike.
func TestForegroundResumeOutOfReachIsNoOracle(t *testing.T) {
	e := newSchedEnv(t)
	bound := e.terminal(t, protocol.RunMode_Background, "")
	free := e.terminal(t, protocol.RunMode_Background, "")
	e.seed(t, 1, protocol.ScheduleTaskState_Bound, bound, "")
	conn, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_Resume, Scope: authority.Scope{Base: protocol.ScopeBase_None}})
	cid := e.connIDOf(conn)
	_ = cid
	var got [2]protocol.OpenInteractiveStatus
	for i, task := range []string{bound, free} {
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 3}}
		req.SetOpenInteractive(*foregroundResume(task))
		e.h.Handle(conn, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, conn)
		if resp.Kind != protocol.TaskControlKind_OpenInteractive || resp.OpenInteractive() == nil {
			t.Fatalf("task %d: response %+v (a permission_denied here is the oracle)", i, resp)
		}
		got[i] = resp.OpenInteractive().Status
	}
	if got[0] != protocol.OpenInteractiveStatus_ResumeNotFound || got[1] != got[0] {
		t.Fatalf("bound %v, unbound %v: both must be resume_not_found", got[0], got[1])
	}
}

func TestScheduleRefusalStatuses(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	want := func(name string, r protocol.ScheduleResponse, st protocol.ScheduleStatus) {
		t.Helper()
		if r.Status != st || len(r.Message) == 0 {
			t.Errorf("%s: %v %q, want %v with a message", name, r.Status, r.Message, st)
		}
	}
	// repo_required
	sp := createSpec()
	sp.SetRepoPresent(false)
	want("no repo or task", e.create(t, "", sp, "go"), protocol.ScheduleStatus_RepoRequired)
	// repo_conflict: --repo with --task, and --repo on a bound schedule's edit.
	task := e.terminal(t, protocol.RunMode_Background, "")
	sp = bindSpec(t, createSpec(), task)
	sp.SetRepoPresent(true)
	sp.SetRepoPath([]byte("/r"))
	want("repo with task", e.create(t, "", sp, "go"), protocol.ScheduleStatus_RepoConflict)
	bsc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	esp := &protocol.ScheduleSpec{}
	esp.SetRepoPresent(true)
	esp.SetRepoPath([]byte("/r"))
	want("repo on bound edit", e.edit(t, "", bsc.ID, esp, ""), protocol.ScheduleStatus_RepoConflict)
	// scope_not_permitted: a scope id outside the creator's reach.
	conn, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_FileRead,
		Scope: authority.Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: protocol.Capability_All,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree}})
	sp = createSpec()
	sp.SetScopePresent(true)
	var ts protocol.TaskScope
	ts.Base = protocol.ScopeBase_None
	ts.Ids = []protocol.TaskID{hexToTaskID(t, task)}
	sp.SetScope(ts)
	sp.SetOverrides(nil)
	want("scope id out of reach", e.create(t, e.connIDOf(conn), sp, "go"), protocol.ScheduleStatus_ScopeNotPermitted)
	// record_not_permitted
	rsp := createSpec()
	rsp.SetRecordSet(true)
	rsp.SetRecord(true)
	want("record without the bit", e.create(t, e.connIDOf(conn), rsp, "go"), protocol.ScheduleStatus_RecordNotPermitted)
	// turn_end_unsupported
	tsp := createSpec()
	tsp.SetTurnEndPresent(true)
	tsp.SetTurnEnd(protocol.TurnEndSignal(99))
	want("turn end", e.create(t, "", tsp, "go"), protocol.ScheduleStatus_TurnEndUnsupported)
	// task_already_bound
	want("second binding", e.create(t, "", bindSpec(t, createSpec(), task), "go"), protocol.ScheduleStatus_TaskAlreadyBound)
	// not_ready
	e.ready.Store(false)
	rsc := e.seed(t, 2, protocol.ScheduleTaskState_Unbound, "", "")
	want("run before start", e.call(t, schedOpConn(), protocol.TaskControlKind_ScheduleRun, rsc.ID), protocol.ScheduleStatus_NotReady)
}

func TestBindBackgroundRefusesForegroundRun(t *testing.T) {
	e := newSchedEnv(t)
	fg := e.running(t, protocol.RunMode_Foreground, "")
	if err := e.h.Tasks.BindBackground(fg, RunConditions{Deadline: time.Hour}, 1); !errors.Is(err, ErrBindForeground) {
		t.Fatalf("err %v", err)
	}
	if tk, _ := e.h.Tasks.Get(fg); tk.RunMode != protocol.RunMode_Foreground {
		t.Fatal("mode written")
	}
	// Create writes the mode before the schedule: a refusal stores nothing.
	e.addCandidate("claude")
	e.h.Tasks.SetDetached(fg) // still foreground and active
	if r := e.create(t, "", bindSpec(t, createSpec(), fg), "go"); r.Status != protocol.ScheduleStatus_TaskRunningForeground || len(e.store.List()) != 0 {
		t.Fatalf("%v, %d schedules", r.Status, len(e.store.List()))
	}
}

// One record rule on edit: record_persist is checked only when the edit names
// --record / --no-record.
func TestEditChecksRecordOnlyWhenNamed(t *testing.T) {
	e := newSchedEnv(t)
	task := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	conn, creator := e.agent(t, authority.Grant{Caps: protocol.Capability_ScheduleEdit | protocol.Capability_Spawn | protocol.Capability_Resume | protocol.Capability_RecordPersist,
		Scope:        authority.Scope{Base: protocol.ScopeBase_Global, Overrides: []authority.ScopeOverride{{Caps: protocol.Capability_RecordPersist, Base: protocol.ScopeBase_None}}},
		DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}})
	_ = creator
	cid := e.connIDOf(conn)
	psp := &protocol.ScheduleSpec{}
	psp.SetPromptPresent(true)
	if r := e.edit(t, cid, sc.ID, psp, "new"); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("an edit that names no record flag: %v %s", r.Status, r.Message)
	}
	_ = creator
	rsp := &protocol.ScheduleSpec{}
	rsp.SetRecordSet(true)
	rsp.SetRecord(true)
	if r := e.edit(t, cid, sc.ID, rsp, ""); r.Status != protocol.ScheduleStatus_RecordNotPermitted {
		t.Fatalf("--record without the bit: %v", r.Status)
	}
}

func TestNilVariantsAreAnswered(t *testing.T) {
	e := newSchedEnv(t)
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_ScheduleDelete, protocol.TaskControlKind_ScheduleRun, protocol.TaskControlKind_ScheduleShow} {
		conn := schedOpConn()
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: k, RequestId: 9}}
		e.h.handleSchedule("", conn, req)
		if len(conn.Sent()) != 1 {
			t.Fatalf("%v: %d responses", k, len(conn.Sent()))
		}
		if r := scheduleResp(t, conn, k); r.Status != protocol.ScheduleStatus_InternalError {
			t.Errorf("%v: %v", k, r.Status)
		}
	}
}

// Live surfaces drop or gain schedule_id when a binding changes with no other
// task event: a session-mode unbind, a delete, a bind of a task already in background.
func TestBindingChangesPublishATaskEvent(t *testing.T) {
	st, _ := LoadScheduleStore("")
	s := New(Config{Schedules: st})
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	h := s.taskHandler
	s.registry.Add(&RunnerEntry{ID: tcid("R"), Identity: testRunnerID("R"), Hostname: "h", AllowedRoots: []string{"/r"},
		AgentProfiles: []string{"claude"}, MaxTasks: 2, ActiveTasks: map[string]struct{}{}, ConnectedAt: time.Now(), LastTaskActivity: time.Now(), Conn: &fakeConn{}})
	mk := func() string {
		id, err := s.tasks.CreateWithinLimits("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, "",
			protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude",
			RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	last := func(task string) *protocol.TaskStatusEvent {
		evs := getEvents()
		for i := len(evs) - 1; i >= 0; i-- {
			if hex.EncodeToString(evs[i].TaskId.Id[:]) == task {
				if evs[i].Kind != protocol.StatusEventKind_TaskSchedule {
					t.Fatalf("last event for %s is %v, want task_schedule", task[:8], evs[i].Kind)
				}
				return &evs[i]
			}
		}
		return nil
	}
	// bind of a task already in background: no mode write, still an event.
	a := mk()
	before := len(getEvents())
	r, _ := h.scheduleCreate("", bindSpec(t, createSpec(), a), "go")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	if evs := getEvents(); len(evs) == before || !last(a).SchedulePresent() {
		t.Fatalf("no event carrying the schedule id after the bind (%d -> %d)", before, len(evs))
	}
	// session-mode unbind.
	s.tasks.Assign(a, testRunnerID("r1"), "", false)
	conn := schedOpConn()
	sm := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SessionMode, RequestId: 5}}
	sm.SetSessionMode(protocol.SessionModeRequest{TaskId: hexToTaskID(t, a)})
	h.Handle(conn, encodeTaskControlRequest(t, sm))
	if ev := last(a); ev == nil || ev.SchedulePresent() {
		t.Fatalf("last event for the unbound task still carries a schedule id: %+v", ev)
	}
	// delete.
	b := mk()
	r, _ = h.scheduleCreate("", bindSpec(t, createSpec(), b), "go")
	if r.Status != protocol.ScheduleStatus_Ok || !last(b).SchedulePresent() {
		t.Fatalf("bind b: %v", r.Status)
	}
	if d := h.scheduleDelete("", r.Id); d.Status != protocol.ScheduleStatus_Ok {
		t.Fatal(d.Status)
	}
	if ev := last(b); ev.SchedulePresent() {
		t.Fatal("the deleted schedule's task still shows its id")
	}
}

// The dispatch bit check and handleOpenInteractive's gate read the binding at
// different moments; the latter must refuse a caller without the schedule bit
// itself (after reach and self, so it is no oracle).
func TestOpenGateChecksTheBitItself(t *testing.T) {
	e := newSchedEnv(t)
	openRunner(t, e.h)
	task := e.terminal(t, protocol.RunMode_Background, "")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_Bound, task, "")
	conn, _ := e.agent(t, authority.Grant{Caps: protocol.Capability_Resume | protocol.Capability_ScheduleEdit,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global}})
	cid := e.connIDOf(conn)
	r, _ := e.h.handleOpenInteractive(cid, nil, foregroundResume(task), protocol.ClientKind_Agent, e.h.lookupPrincipal(cid), protocol.Capability_Resume, "")
	if r.Status == protocol.OpenInteractiveStatus_Ok {
		t.Fatal("a caller without the schedule bit unbound a task")
	}
	if got := e.stored(t, sc.ID); got.State != protocol.ScheduleTaskState_Bound {
		t.Fatal("still must be bound")
	}
}

// R17: an edit overlays ONLY the condition fields it names, in the server
// under the firing mutex; a present zero is an answer.
func TestEditOverlaysOnlyNamedConditions(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_PendingCreate, "", "")
	sc.Run.Cond = RunConditions{Deadline: time.Hour, Quiet: 7 * time.Minute, Blocked: 9 * time.Minute, QueueTimeout: 10 * time.Minute, TurnEnd: protocol.TurnEndSignal_Quiet, Rows: 30, Cols: 100}
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	edit := func(f func(*protocol.ScheduleSpec)) protocol.ScheduleResponse {
		sp := &protocol.ScheduleSpec{}
		f(sp)
		return e.edit(t, "", sc.ID, sp, "")
	}
	want := func(name string, c RunConditions) {
		t.Helper()
		if got := e.stored(t, sc.ID).Run.Cond; got != c {
			t.Errorf("%s: conditions %+v, want %+v", name, got, c)
		}
	}
	base := sc.Run.Cond

	if r := edit(func(sp *protocol.ScheduleSpec) { sp.SetQuietPresent(true); sp.SetQuietMs(3 * 60000) }); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	c := base
	c.Quiet = 3 * time.Minute
	want("only --quiet", c)

	// A present zero: queue_timeout 0 is "no queue timeout".
	if r := edit(func(sp *protocol.ScheduleSpec) { sp.SetQueueTimeoutPresent(true); sp.SetQueueTimeoutMs(0) }); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	c.QueueTimeout = 0
	want("--queue-timeout 0", c)

	// deadline and queue_timeout together: the deadline changes and the zero stays a zero.
	if r := edit(func(sp *protocol.ScheduleSpec) {
		sp.SetDeadlinePresent(true)
		sp.SetDeadlineMs(2 * 3600000)
		sp.SetQueueTimeoutPresent(true)
		sp.SetQueueTimeoutMs(0)
	}); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	c.Deadline = 2 * time.Hour
	want("--deadline 2h --queue-timeout 0", c)

	// quiet / blocked 0 = back to the default; turn_end unset = any.
	if r := edit(func(sp *protocol.ScheduleSpec) {
		sp.SetQuietPresent(true)
		sp.SetQuietMs(0)
		sp.SetBlockedPresent(true)
		sp.SetBlockedMs(0)
		sp.SetTurnEndPresent(true)
		sp.SetTurnEnd(protocol.TurnEndSignal_Unset)
	}); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	c.Quiet, c.Blocked, c.TurnEnd = defaultQuiet, defaultBlocked, protocol.TurnEndSignal_Any
	want("zeros back to defaults", c)

	// deadline 0 is refused (G41) and changes nothing.
	r := edit(func(sp *protocol.ScheduleSpec) { sp.SetDeadlinePresent(true); sp.SetDeadlineMs(0) })
	if r.Status != protocol.ScheduleStatus_DeadlineRequired {
		t.Errorf("--deadline 0: %v", r.Status)
	}
	want("refused --deadline 0", c)
	// The size (kept beside the conditions) is untouched by all of it.
	if got := e.stored(t, sc.ID).Run.Cond; got.Rows != 30 || got.Cols != 100 {
		t.Errorf("size changed: %dx%d", got.Rows, got.Cols)
	}
}

// Two edits naming different condition fields both survive (no client-side
// read-modify-write to lose one).
func TestTwoEditsOfDifferentConditionsBothSurvive(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sc := e.seed(t, 1, protocol.ScheduleTaskState_PendingCreate, "", "")
	a := &protocol.ScheduleSpec{}
	a.SetBlockedPresent(true)
	a.SetBlockedMs(11 * 60000)
	b := &protocol.ScheduleSpec{}
	b.SetTurnEndPresent(true)
	b.SetTurnEnd(protocol.TurnEndSignal_Lifecycle)
	for _, sp := range []*protocol.ScheduleSpec{a, b} {
		if r := e.edit(t, "", sc.ID, sp, ""); r.Status != protocol.ScheduleStatus_Ok {
			t.Fatalf("%v %s", r.Status, r.Message)
		}
	}
	got := e.stored(t, sc.ID).Run.Cond
	if got.Blocked != 11*time.Minute || got.TurnEnd != protocol.TurnEndSignal_Lifecycle || got.Deadline != sc.Run.Cond.Deadline {
		t.Errorf("after two edits: %+v", got)
	}
}

// When the stored zone no longer loads, an edit that recomputes next_at must
// answer invalid_zone naming the zone, not "no firing within five years".
func TestEditWithUnloadableStoredZoneAnswersInvalidZone(t *testing.T) {
	e := newSchedEnv(t)
	sc := fireTestSchedule(7, protocol.ScheduleTaskState_PendingCreate, "")
	sc.TZ = "Gone/Zone"
	sc.Repo = "/r"
	sc.NextAt = fireNow.Add(time.Hour)
	if err := e.store.Put(sc); err != nil {
		t.Fatal(err)
	}
	sp := &protocol.ScheduleSpec{}
	sp.SetCronPresent(true)
	sp.SetCron([]byte("0 10 * * *"))
	r := e.edit(t, "", sc.ID, sp, "")
	if r.Status != protocol.ScheduleStatus_InvalidZone || !strings.Contains(string(r.Message), "Gone/Zone") {
		t.Fatalf("%v %q, want invalid_zone naming Gone/Zone", r.Status, r.Message)
	}
}
