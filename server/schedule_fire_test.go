package server

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// fakeSchedClock drives the schedule loop and the firing function. after
// hands the test each requested wait and its channel; nothing sleeps.
type fakeSchedClock struct {
	mu   sync.Mutex
	t    time.Time
	reqs chan fakeSchedReq
}

type fakeSchedReq struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeSchedClock(t time.Time) *fakeSchedClock {
	return &fakeSchedClock{t: t, reqs: make(chan fakeSchedReq, 64)}
}

func (c *fakeSchedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeSchedClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func (c *fakeSchedClock) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.reqs <- fakeSchedReq{d, ch}
	return ch
}

var fireNow = time.Date(2026, 11, 15, 9, 0, 30, 0, time.UTC)

// newFireServer is a server with a fake clock at fireNow and a file-backed
// schedule store under dir.
func newFireServer(t *testing.T, dir string) (*Server, *fakeSchedClock) {
	t.Helper()
	st, err := LoadScheduleStore(filepath.Join(dir, "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Schedules: st})
	fc := newFakeSchedClock(fireNow)
	s.sched.clock = scheduleClock{now: fc.now, after: fc.after}
	return s, fc
}

// fireTestSchedule is a schedule due at 09:00 UTC today (30s before fireNow).
func fireTestSchedule(n int, state protocol.ScheduleTaskState, taskID string) Schedule {
	return Schedule{
		ID: scheduleID(n), Cron: "0 9 * * *", TZ: "UTC", State: state, TaskID: taskID,
		Repo: "/repo", Agent: "claude", Prompt: "tick", ExtraArgs: []string{"--a"}, ResumeConversation: true,
		Run: RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}, Record: true},
		Grant: authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope(),
			SpawnDepth: authority.LimitOf(2), MaxDescendants: authority.LimitOf(5)},
		CreatedAt: fireNow.Add(-24 * time.Hour), NextAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
	}
}

func putSchedule(t *testing.T, s *Server, sc Schedule) {
	t.Helper()
	if err := s.cfg.Schedules.Put(sc); err != nil {
		t.Fatal(err)
	}
}

// terminalTask is a finished (Succeeded, Run 1) task.
func terminalTask(t *testing.T, s *Server) string {
	t.Helper()
	id := s.tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_FileRead, Scope: authority.DefaultScope()}, "claude")
	s.tasks.Finish(id, 0, nil)
	return id
}

func getSched(t *testing.T, s *Server, id string) Schedule {
	t.Helper()
	sc, ok := s.cfg.Schedules.Get(id)
	if !ok {
		t.Fatalf("schedule %s missing", id)
	}
	return sc
}

var wantNextAfterFireNow = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func TestFireOutcomes(t *testing.T) {
	due := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	type tc struct {
		name   string
		manual bool
		setup  func(t *testing.T, s *Server) Schedule
		want   protocol.ScheduleOutcome
		check  func(t *testing.T, s *Server, sc Schedule)
	}
	bound := func(t *testing.T, s *Server) Schedule {
		return fireTestSchedule(1, protocol.ScheduleTaskState_Bound, terminalTask(t, s))
	}
	cases := []tc{
		{name: "unbound", want: protocol.ScheduleOutcome_SkippedNoTask,
			setup: func(t *testing.T, s *Server) Schedule {
				return fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
			}},
		{name: "task gone", want: protocol.ScheduleOutcome_SkippedTaskGone,
			setup: func(t *testing.T, s *Server) Schedule {
				return fireTestSchedule(1, protocol.ScheduleTaskState_Bound, strings.Repeat("ee", 16))
			}},
		{name: "busy", want: protocol.ScheduleOutcome_SkippedBusy,
			setup: func(t *testing.T, s *Server) Schedule {
				id := s.tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
					protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Scope: authority.DefaultScope()}, "claude")
				return fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
			}},
		{name: "fired", want: protocol.ScheduleOutcome_Fired, setup: bound,
			check: func(t *testing.T, s *Server, sc Schedule) {
				e := sc.History[0]
				task, _ := s.tasks.Get(sc.TaskID)
				if task.Run != 2 || e.ExpectRun != 2 || task.Status != protocol.TaskStatus_Queued {
					t.Errorf("task run %d status %v, entry expect %d", task.Run, task.Status, e.ExpectRun)
				}
				if task.ResumedByKind != protocol.ClientKind_Schedule {
					t.Errorf("ResumedByKind %v", task.ResumedByKind)
				}
				if task.Prompt != "tick" || len(task.ExtraArgs) != 1 || !task.ResumeConversation || task.RunMode != protocol.RunMode_Background {
					t.Errorf("resume did not carry the schedule's spec: %+v", task)
				}
				if task.Capabilities != protocol.Capability_FileRead || !task.SpawnDepth.Limited() || task.SpawnDepth.Value() != 2 || task.MaxDescendants.Value() != 5 {
					t.Errorf("GrantUpdate not applied: caps %v depth %v max %v", task.Capabilities, task.SpawnDepth, task.MaxDescendants)
				}
				if !e.DueAt.Equal(due) || !e.At.Equal(fireNow) || !e.QueuedAt.Equal(fireNow) || e.TaskID != sc.TaskID {
					t.Errorf("entry times/task: %+v", e)
				}
				if len(sc.History) != 1 {
					t.Errorf("history %d entries: the schedule's own resume must not append resumed_by", len(sc.History))
				}
			}},
		{name: "run now", manual: true, want: protocol.ScheduleOutcome_RunNow, setup: bound},
		{name: "manual busy", manual: true, want: protocol.ScheduleOutcome_SkippedBusy,
			setup: func(t *testing.T, s *Server) Schedule {
				id := s.tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
					protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Scope: authority.DefaultScope()}, "claude")
				return fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
			}},
		{name: "pending create", want: protocol.ScheduleOutcome_Fired,
			setup: func(t *testing.T, s *Server) Schedule {
				return fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
			},
			check: func(t *testing.T, s *Server, sc Schedule) {
				if sc.State != protocol.ScheduleTaskState_Bound || sc.TaskID == "" || sc.History[0].TaskID != sc.TaskID || !sc.History[0].Creates {
					t.Errorf("state %v task %q entry %+v", sc.State, sc.TaskID, sc.History[0])
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newFireServer(t, t.TempDir())
			sc := c.setup(t, s)
			putSchedule(t, s, sc)
			got := s.fireSchedule(sc.ID, due, c.manual)
			want := c.want
			if got != want {
				t.Fatalf("outcome %v, want %v", got, want)
			}
			after := getSched(t, s, sc.ID)
			if len(after.History) == 0 || after.History[len(after.History)-1].Outcome != want {
				t.Fatalf("history %+v, want last outcome %v", after.History, want)
			}
			if c.manual {
				if !after.NextAt.Equal(sc.NextAt) {
					t.Errorf("manual firing moved next_at to %v", after.NextAt)
				}
			} else if !after.NextAt.Equal(wantNextAfterFireNow) {
				t.Errorf("next_at %v, want %v (recomputed from the firing time)", after.NextAt, wantNextAfterFireNow)
			}
			if c.check != nil {
				c.check(t, s, after)
			}
		})
	}
}

func TestFireConcurrentOnce(t *testing.T) {
	for i := 0; i < 30; i++ {
		s, _ := newFireServer(t, t.TempDir())
		id := terminalTask(t, s)
		sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
		putSchedule(t, s, sc)
		var wg sync.WaitGroup
		start := make(chan struct{})
		got := make([]protocol.ScheduleOutcome, 2)
		for k, manual := range []bool{false, true} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[k] = s.fireSchedule(sc.ID, sc.NextAt, manual)
			}()
		}
		close(start)
		wg.Wait()
		ran, busy := 0, 0
		for _, o := range got {
			switch o {
			case protocol.ScheduleOutcome_Fired, protocol.ScheduleOutcome_RunNow:
				ran++
			case protocol.ScheduleOutcome_SkippedBusy:
				busy++
			}
		}
		if ran != 1 || busy != 1 {
			t.Fatalf("iteration %d: outcomes %v, want one run and one skipped_busy", i, got)
		}
		if task, _ := s.tasks.Get(id); task.Run != 2 {
			t.Fatalf("iteration %d: task run %d, want 2", i, task.Run)
		}
		if h := getSched(t, s, sc.ID).History; len(h) != 2 {
			t.Fatalf("iteration %d: %d history entries, want 2", i, len(h))
		}
	}
}

func TestFirstFiringCreatesUnderChosenID(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	putSchedule(t, s, sc)
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("outcome %v", got)
	}
	after := getSched(t, s, sc.ID)
	task, ok := s.tasks.Get(after.TaskID)
	if !ok {
		t.Fatalf("task %q not created", after.TaskID)
	}
	if after.State != protocol.ScheduleTaskState_Bound || after.History[0].TaskID != after.TaskID {
		t.Fatalf("state %v, entry task %q, schedule task %q", after.State, after.History[0].TaskID, after.TaskID)
	}
	if task.OriginKind != protocol.ClientKind_Schedule || task.RunMode != protocol.RunMode_Background ||
		task.Prompt != "tick" || task.RepoPath != "/repo" || task.AgentProfile != "claude" || !task.Record ||
		task.Capabilities != protocol.Capability_FileRead || task.SpawnDepth.Value() != 2 || task.MaxDescendants.Value() != 5 ||
		task.Run != 1 || !task.ResumeConversation || len(task.ExtraArgs) != 1 {
		t.Errorf("created task does not carry the schedule's spec: %+v", task)
	}
	if after.History[0].ExpectRun != 1 || !after.History[0].Creates {
		t.Errorf("entry %+v", after.History[0])
	}
}

func TestRefusedCreationRetries(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	// A creator whose descendant limit is full: one child present, limit one.
	p := s.tasks.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Scope: authority.DefaultScope(), MaxDescendants: authority.LimitOf(1)}, "claude")
	var pid protocol.TaskID
	copy(pid.Id[:], parseIDSlice(t, p))
	child := s.tasks.Create("/repo", "c", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, pid, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Scope: authority.DefaultScope()}, "claude")

	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	sc.Creator = p
	putSchedule(t, s, sc)
	before := len(s.tasks.List(0))
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_SkippedRefused {
		t.Fatalf("outcome %v, want skipped_refused", got)
	}
	after := getSched(t, s, sc.ID)
	if after.State != protocol.ScheduleTaskState_PendingCreate || after.History[0].Message == "" || len(s.tasks.List(0)) != before {
		t.Fatalf("state %v message %q tasks %d->%d", after.State, after.History[0].Message, before, len(s.tasks.List(0)))
	}
	if !after.NextAt.Equal(wantNextAfterFireNow) {
		t.Errorf("a refused creation must still advance next_at: %v", after.NextAt)
	}

	// The limit lifts: the child is pruned. The next firing creates.
	s.tasks.Finish(child, 0, nil)
	if removed, _, _ := s.tasks.PruneByIDs(nil, []string{child}); removed != 1 {
		t.Fatalf("prune removed %d", removed)
	}
	if got := s.fireSchedule(sc.ID, after.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("second outcome %v, want fired", got)
	}
	again := getSched(t, s, sc.ID)
	if again.State != protocol.ScheduleTaskState_Bound || again.TaskID == "" {
		t.Fatalf("state %v task %q", again.State, again.TaskID)
	}
	task, _ := s.tasks.Get(again.TaskID)
	if task.OriginKind != protocol.ClientKind_Schedule || hexID(task.CreatorTaskID) != p {
		t.Errorf("origin %v creator %s, want schedule and %s", task.OriginKind, hexID(task.CreatorTaskID), p)
	}
}

func TestUnboundBeforeFirstFiring(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
	putSchedule(t, s, sc)
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_SkippedNoTask {
		t.Fatalf("outcome %v", got)
	}
	if n := len(s.tasks.List(0)); n != 0 {
		t.Fatalf("%d tasks created for an unbound schedule", n)
	}
	if after := getSched(t, s, sc.ID); after.State != protocol.ScheduleTaskState_Unbound {
		t.Fatalf("state %v", after.State)
	}
}

func TestMissedFiresOnce(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, terminalTask(t, s))
	sc.NextAt = fireNow.Add(-48 * time.Hour)
	putSchedule(t, s, sc)
	s.startSchedules()
	after := getSched(t, s, sc.ID)
	if len(after.History) != 1 || after.History[0].Outcome != protocol.ScheduleOutcome_Fired || !after.History[0].DueAt.Equal(sc.NextAt) {
		t.Fatalf("history %+v, want one fired entry due %v", after.History, sc.NextAt)
	}
	if !after.NextAt.Equal(wantNextAfterFireNow) {
		t.Fatalf("next_at %v, want %v", after.NextAt, wantNextAfterFireNow)
	}
	if task, _ := s.tasks.Get(sc.TaskID); task.Run != 2 {
		t.Fatalf("run %d: two days missed must run once", task.Run)
	}
	s.startSchedules() // nothing is due now
	if h := getSched(t, s, sc.ID).History; len(h) != 1 {
		t.Fatalf("a second start fired again: %d entries", len(h))
	}
}

func TestCrashBetweenIntentAndResume(t *testing.T) {
	dir := t.TempDir()
	s1, _ := newFireServer(t, dir)
	id := terminalTask(t, s1)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	// What step 4 writes: a pending entry and an advanced next_at. Then the
	// server dies before step 5.
	sc.NextAt = wantNextAfterFireNow
	sc.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, DueAt: fireNow.Add(-time.Minute), At: fireNow, ExpectRun: 2, TaskID: id}}
	putSchedule(t, s1, sc)

	s2, _ := newFireServer(t, dir) // reload from the file
	s2.tasks = s1.tasks            // the task store survived (Run 1)
	s2.startSchedules()
	after := getSched(t, s2, sc.ID)
	if len(after.History) != 1 || after.History[0].Outcome != protocol.ScheduleOutcome_Lost {
		t.Fatalf("history %+v, want one lost entry", after.History)
	}
	if task, _ := s2.tasks.Get(id); task.Run != 1 {
		t.Fatalf("run %d: nothing may fire on start", task.Run)
	}
}

func TestCrashAfterResume(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := newFireServer(t, dir)
	s1.tasks.SetWAL(wal)
	id := terminalTask(t, s1)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	sc.NextAt = wantNextAfterFireNow
	sc.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, DueAt: fireNow.Add(-time.Minute), At: fireNow, ExpectRun: 2, TaskID: id}}
	putSchedule(t, s1, sc)
	// Step 5 happened, the outcome rewrite did not; the run was Running.
	if _, err := s1.tasks.Resume(id, "tick", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Schedule, GrantUpdate{},
		protocol.TaskKind_Interactive, "claude", protocol.TaskID{}, false, sc.Run); err != nil {
		t.Fatal(err)
	}
	s1.tasks.Assign(id, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	events, _, err := ReadWAL(walPath)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := newFireServer(t, dir)
	s2.tasks.ReplayEvents(events)
	s2.startSchedules()
	after := getSched(t, s2, sc.ID)
	if len(after.History) != 1 {
		t.Fatalf("history %+v: no second run may be recorded", after.History)
	}
	e := after.History[0]
	if e.Outcome != protocol.ScheduleOutcome_Fired || e.EndReason != protocol.EndReason_Failed || e.EndedAt.IsZero() {
		t.Fatalf("entry %+v, want fired, ended, reason failed", e)
	}
	if !e.StartedAt.IsZero() && e.QueuedAt.IsZero() {
		t.Fatalf("entry %+v has started_at without queued_at: its wait would be computed from year 1", e)
	}
	if !e.QueuedAt.Equal(e.At) {
		t.Fatalf("queued_at %v, want the intent time %v", e.QueuedAt, e.At)
	}
	if task, _ := s2.tasks.Get(id); task.Run != 2 || task.Status != protocol.TaskStatus_Failed {
		t.Fatalf("run %d status %v", task.Run, task.Status)
	}
}

func TestUnloadableZoneSkipsRefused(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	sc.TZ = "Mars/Olympus"
	putSchedule(t, s, sc)
	// A second schedule with a good zone, due in the same pass.
	goodTask := terminalTask(t, s)
	good := fireTestSchedule(2, protocol.ScheduleTaskState_Bound, goodTask)
	putSchedule(t, s, good)
	s.fireDueSchedules()
	after := getSched(t, s, sc.ID)
	if len(after.History) != 1 || after.History[0].Outcome != protocol.ScheduleOutcome_SkippedRefused {
		t.Fatalf("history %+v, want one skipped_refused", after.History)
	}
	if !strings.Contains(after.History[0].Message, "Mars/Olympus") {
		t.Errorf("message %q does not name the zone", after.History[0].Message)
	}
	if !after.NextAt.Equal(fireNow.Add(time.Hour)) {
		t.Errorf("next_at %v, want now+1h so the loop does not spin", after.NextAt)
	}
	if task, _ := s.tasks.Get(id); task.Run != 1 {
		t.Errorf("a refused firing resumed the task (run %d)", task.Run)
	}
	g := getSched(t, s, good.ID)
	if len(g.History) != 1 || g.History[0].Outcome != protocol.ScheduleOutcome_Fired {
		t.Fatalf("the good-zone schedule did not fire in the same pass: %+v", g.History)
	}
	if task, _ := s.tasks.Get(goodTask); task.Run != 2 {
		t.Errorf("good schedule's task run %d, want 2", task.Run)
	}
}

// failNthWrite makes the schedule store's nth file write from now fail once.
func failNthWrite(st *ScheduleStore, n int) {
	count := 0
	st.writeFn = func(path string, data []byte) error {
		count++
		if count == n {
			return errors.New("injected save failure")
		}
		return writeFileAtomic(path, data)
	}
}

// A failed outcome save rolls memory back to the pending intent. The next
// firing must settle it from the task store before it chooses a new id.
func TestFailedOutcomeSaveCreation(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	putSchedule(t, s, sc)
	failNthWrite(s.cfg.Schedules, 2) // 1 = the intent, 2 = the outcome
	s.fireSchedule(sc.ID, sc.NextAt, false)
	mid := getSched(t, s, sc.ID)
	if len(mid.History) != 1 || mid.History[0].Outcome != protocol.ScheduleOutcome_Pending || mid.State != protocol.ScheduleTaskState_PendingCreate {
		t.Fatalf("the failed save did not leave the intent pending: %+v state %v", mid.History, mid.State)
	}
	x := mid.History[0].TaskID
	if _, ok := s.tasks.Get(x); !ok {
		t.Fatalf("task %s was not created", x)
	}
	got := s.fireSchedule(sc.ID, wantNextAfterFireNow, false)
	after := getSched(t, s, sc.ID)
	if n := len(s.tasks.List(0)); n != 1 {
		t.Fatalf("%d tasks: the second firing created a duplicate", n)
	}
	if after.State != protocol.ScheduleTaskState_Bound || after.TaskID != x {
		t.Fatalf("state %v task %q, want bound to the first creation %s", after.State, after.TaskID, x)
	}
	if after.History[0].Outcome != protocol.ScheduleOutcome_Fired || got != protocol.ScheduleOutcome_SkippedBusy {
		t.Fatalf("first entry %v, second outcome %v; want fired and skipped_busy (the task is queued)", after.History[0].Outcome, got)
	}
}

func TestFailedOutcomeSaveResume(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	putSchedule(t, s, sc)
	failNthWrite(s.cfg.Schedules, 2)
	s.fireSchedule(sc.ID, sc.NextAt, false)
	if mid := getSched(t, s, sc.ID); mid.History[0].Outcome != protocol.ScheduleOutcome_Pending {
		t.Fatalf("intent not pending: %+v", mid.History)
	}
	got := s.fireSchedule(sc.ID, wantNextAfterFireNow, false)
	after := getSched(t, s, sc.ID)
	if after.History[0].Outcome != protocol.ScheduleOutcome_Fired || got != protocol.ScheduleOutcome_SkippedBusy {
		t.Fatalf("first entry %v, second %v; want fired and skipped_busy", after.History[0].Outcome, got)
	}
	if task, _ := s.tasks.Get(id); task.Run != 2 {
		t.Fatalf("run %d, want 2", task.Run)
	}
	// The run ends and the schedule fires again normally: the old entry
	// stays fired, nothing is double-counted.
	s.tasks.Finish(id, 0, nil)
	if got := s.fireSchedule(sc.ID, wantNextAfterFireNow, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("third firing %v", got)
	}
	if task, _ := s.tasks.Get(id); task.Run != 3 {
		t.Fatalf("run %d, want 3", task.Run)
	}
}

// S-D11: a wait is started_at - queued_at, so an entry that settles to fired
// or is backfilled must carry queued_at (the intent time) too.
func TestSettledEntriesHaveQueuedAt(t *testing.T) {
	st, _ := LoadScheduleStore("")
	ta := strings.Repeat("a1", 16)
	sc := fullSchedule(t, scheduleID(1), 0)
	sc.State, sc.TaskID = protocol.ScheduleTaskState_Bound, ta
	at := time.Date(2026, 11, 15, 9, 0, 1, 0, time.UTC)
	sc.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, TaskID: ta, ExpectRun: 2, At: at}}
	if err := st.Put(sc); err != nil {
		t.Fatal(err)
	}
	if err := st.Settle(func(string) (uint32, bool) { return 2, true }); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(sc.ID)
	if e := got.History[0]; e.Outcome != protocol.ScheduleOutcome_Fired || !e.QueuedAt.Equal(at) {
		t.Fatalf("settled entry %+v: want fired with queued_at = at", e)
	}
}

func TestCompletionOfEvictedEntryIsNoop(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	for i := 0; i < historyMax; i++ {
		sc.History = append(sc.History, HistoryEntry{Outcome: protocol.ScheduleOutcome_Fired, ExpectRun: uint32(i + 1), TaskID: id,
			EndedAt: fireNow.Add(-time.Hour)})
	}
	sc.History[0].EndedAt = time.Time{} // the oldest has no end yet, and run 1's entry is about to be evicted
	putSchedule(t, s, sc)
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("outcome %v", got)
	}
	before := getSched(t, s, sc.ID)
	if len(before.History) != historyMax || before.History[0].ExpectRun != 2 {
		t.Fatalf("eviction did not happen as the test assumes: %d entries, first expects %d", len(before.History), before.History[0].ExpectRun)
	}
	var ev protocol.TaskStatusEvent
	ev.Kind = protocol.StatusEventKind_TaskEnded
	ev.Run = 1 // the evicted entry's run
	ev.EndReason = protocol.EndReason_Deadline
	ev.Ts = uint64(fireNow.UnixNano())
	copy(ev.TaskId.Id[:], parseIDSlice(t, id))
	s.scheduleTaskEvent(ev) // must neither panic nor touch another entry
	after := getSched(t, s, sc.ID)
	if len(after.History) != len(before.History) {
		t.Fatalf("history changed length")
	}
	for i := range before.History {
		if before.History[i] != after.History[i] {
			t.Errorf("entry %d changed: %+v -> %+v", i, before.History[i], after.History[i])
		}
	}
}

func TestResumedByRecorded(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	sc.NextAt = wantNextAfterFireNow
	putSchedule(t, s, sc)
	if _, err := s.tasks.Resume(id, "by hand", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli, GrantUpdate{},
		protocol.TaskKind_Interactive, "claude", protocol.TaskID{}, false, sc.Run); err != nil {
		t.Fatal(err)
	}
	after := getSched(t, s, sc.ID)
	if len(after.History) != 1 {
		t.Fatalf("history %+v, want one resumed_by entry", after.History)
	}
	e := after.History[0]
	if e.Outcome != protocol.ScheduleOutcome_ResumedBy || e.ByKind != protocol.ClientKind_Cli || e.By != "" || e.TaskID != id || e.ExpectRun != 2 {
		t.Fatalf("entry %+v", e)
	}
	if !e.QueuedAt.IsZero() || !e.StartedAt.IsZero() {
		t.Errorf("a resumed_by wait is not the schedule's, yet entry carries %v / %v", e.QueuedAt, e.StartedAt)
	}
	// The run's own start must not complete this entry (it is not the schedule's wait).
	s.tasks.Assign(id, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	if got := getSched(t, s, sc.ID).History[0]; !got.StartedAt.IsZero() {
		t.Errorf("resumed_by entry got a started_at: %+v", got)
	}
}

func TestHistoryCompletedFromEvents(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	putSchedule(t, s, sc)
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("outcome %v", got)
	}
	s.tasks.Assign(id, testRunnerID("ws:127.0.0.1:8539-40"), "", false)
	e := getSched(t, s, sc.ID).History[0]
	if e.StartedAt.IsZero() || !e.EndedAt.IsZero() {
		t.Fatalf("after assign: %+v", e)
	}
	s.tasks.Finish(id, 0, nil)
	e = getSched(t, s, sc.ID).History[0]
	if e.EndedAt.IsZero() || e.EndReason != protocol.EndReason_ProcessExit {
		t.Fatalf("after finish: %+v", e)
	}
}

// A creation's Assigned event can arrive before the firing has bound the
// schedule to the new task (the entry is still pending): it must land.
func TestCreationEventsBeforeOutcomeWrite(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	taskID := strings.Repeat("dd", 16)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	sc.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, Creates: true, ExpectRun: 1, TaskID: taskID, At: fireNow}}
	putSchedule(t, s, sc)
	var ev protocol.TaskStatusEvent
	ev.Kind = protocol.StatusEventKind_TaskAssigned
	ev.Run = 1
	ev.Ts = uint64(fireNow.Add(time.Second).UnixNano())
	copy(ev.TaskId.Id[:], parseIDSlice(t, taskID))
	s.scheduleTaskEvent(ev)
	if e := getSched(t, s, sc.ID).History[0]; e.StartedAt.IsZero() {
		t.Fatalf("entry %+v: started_at not recorded", e)
	}
}

func TestRunSchedulesLoop(t *testing.T) {
	s, fc := newFireServer(t, t.TempDir())
	id := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Bound, id)
	sc.NextAt = fireNow.Add(10 * time.Minute)
	putSchedule(t, s, sc)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runSchedules(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	recv := func() fakeSchedReq {
		t.Helper()
		select {
		case r := <-fc.reqs:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("loop did not arm a timer")
			return fakeSchedReq{}
		}
	}
	r := recv()
	if r.d != 10*time.Minute {
		t.Fatalf("armed for %v, want 10m (EarliestNext - now)", r.d)
	}
	r.ch <- fc.advance(10 * time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for len(getSched(t, s, sc.ID).History) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timer did not fire the schedule")
		}
		time.Sleep(time.Millisecond)
	}
	// Next firing is a day away: the wait is capped at an hour (S-D5).
	r = recv()
	if r.d != time.Hour {
		t.Fatalf("armed for %v, want 1h cap", r.d)
	}
	// A store change pokes the loop, which re-arms for the new earliest time.
	sooner := fireTestSchedule(2, protocol.ScheduleTaskState_Unbound, "")
	sooner.NextAt = fc.now().Add(5 * time.Minute)
	putSchedule(t, s, sooner)
	s.pokeSchedules()
	if r = recv(); r.d != 5*time.Minute {
		t.Fatalf("after a poke armed for %v, want 5m", r.d)
	}
}

func parseIDSlice(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		t.Fatalf("bad task id %q", h)
	}
	return b
}

func hexID(id protocol.TaskID) string { return hex.EncodeToString(id.Id[:]) }

// taskScheduleEvents are the task_schedule events captured for task that carry
// a schedule id.
func taskScheduleEventsWithID(evs []protocol.TaskStatusEvent, task string) int {
	n := 0
	for _, ev := range evs {
		if hex.EncodeToString(ev.TaskId.Id[:]) == task && ev.Kind == protocol.StatusEventKind_TaskSchedule && ev.SchedulePresent() {
			n++
		}
	}
	return n
}

// The first firing's task_queued is published before the schedule binds, so
// the binding must be published afterwards (kind=task_schedule, schedule_id).
func TestFirstFiringPublishesTheBinding(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	putSchedule(t, s, sc)
	s.fireSchedule(sc.ID, sc.NextAt, false)
	after := getSched(t, s, sc.ID)
	if n := taskScheduleEventsWithID(getEvents(), after.TaskID); n != 1 {
		t.Fatalf("%d task_schedule events with a schedule id for the created task; events: %+v", n, getEvents())
	}
}

// A creation whose outcome save failed binds inside the next firing's settle;
// that binding is published too.
func TestSettleBindingInsideAFiringIsPublished(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	getEvents := captureTaskEvents(s, topics.TasksStatus())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	putSchedule(t, s, sc)
	failNthWrite(s.cfg.Schedules, 2)
	s.fireSchedule(sc.ID, sc.NextAt, false)
	x := getSched(t, s, sc.ID).History[0].TaskID
	if n := taskScheduleEventsWithID(getEvents(), x); n != 0 {
		t.Fatalf("bound event before any binding was stored: %d", n)
	}
	s.fireSchedule(sc.ID, wantNextAfterFireNow, false)
	if n := taskScheduleEventsWithID(getEvents(), x); n < 1 {
		t.Fatalf("settle's binding was not published")
	}
}

// S-D17a: a verb's check runs inside the firing, on the state it acts on —
// after a pending creation settles — and a veto records and starts nothing.
func TestFireCheckSeesSettledStateAndVetoRecordsNothing(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	taskID := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	sc.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, Creates: true, ExpectRun: 1, TaskID: taskID, At: fireNow}}
	putSchedule(t, s, sc)
	var seen protocol.ScheduleTaskState
	veto := &FireVeto{Deny: protocol.Capability_Resume}
	got, v := s.fireScheduleChecked(sc.ID, sc.NextAt, true, func(x Schedule) *FireVeto {
		seen = x.State
		if x.State == protocol.ScheduleTaskState_Bound {
			return veto
		}
		return nil
	})
	if seen != protocol.ScheduleTaskState_Bound {
		t.Fatalf("check saw state %v, want the settled bound", seen)
	}
	if v != veto || got != protocol.ScheduleOutcome_Pending {
		t.Fatalf("outcome %v veto %v, want the veto", got, v)
	}
	after := getSched(t, s, sc.ID)
	if after.State != protocol.ScheduleTaskState_PendingCreate || len(after.History) != 1 || after.History[0].Outcome != protocol.ScheduleOutcome_Pending {
		t.Fatalf("a veto changed the schedule: %+v", after)
	}
	if tk, _ := s.tasks.Get(taskID); tk.Run != 1 {
		t.Fatalf("a vetoed run resumed the task: run %d", tk.Run)
	}
}

// A schedule bound between a verb's first read and the firing is checked as
// bound.
func TestFireCheckSeesABindThatLandedFirst(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	taskID := terminalTask(t, s)
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_Unbound, "")
	putSchedule(t, s, sc)
	check := func(x Schedule) *FireVeto {
		if x.State == protocol.ScheduleTaskState_Bound {
			return &FireVeto{Deny: protocol.Capability_Resume}
		}
		return nil
	}
	if check(getSched(t, s, sc.ID)) != nil {
		t.Fatal("the first read must pass")
	}
	if err := s.cfg.Schedules.Update(sc.ID, func(x *Schedule) error {
		x.State, x.TaskID = protocol.ScheduleTaskState_Bound, taskID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, v := s.fireScheduleChecked(sc.ID, sc.NextAt, true, check); v == nil || got != protocol.ScheduleOutcome_Pending {
		t.Fatalf("outcome %v veto %v, want a veto", got, v)
	}
	if tk, _ := s.tasks.Get(taskID); tk.Run != 1 {
		t.Fatalf("a vetoed run resumed the task: run %d", tk.Run)
	}
	if h := getSched(t, s, sc.ID).History; len(h) != 0 {
		t.Fatalf("history %+v", h)
	}
}
