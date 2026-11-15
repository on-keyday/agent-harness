package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Firing (pillar S, "Firing", S-D5..S-D10, S-D13, S-D21, S-D22): the one
// function every start of a schedule goes through (the timer, the start-up
// pass, `schedule run`), the timer loop, and the completion of history
// entries from the task events.
//
// Lock order, outermost first:
//
//	per-schedule firing mutex  (scheduleRuntime.firingMutex)
//	  -> ScheduleStore.mu      (inside Update / UpdateForTask / Settle)
//	     -> TaskStore.mu       (only the read-only Get, inside an Update callback)
//
// TaskStore never calls the ScheduleStore while holding its own lock: its
// hooks (OnCreate, OnAssign, OnFinish, ...) run after it unlocks, and
// scheduleTaskEvent takes only ScheduleStore.mu. The mutating TaskStore calls
// of a firing (Resume, CreateWithinLimitsID) run with the ScheduleStore
// unlocked, between the intent write and the outcome write.

// scheduleRetryAfter is how long a firing the server could not carry out at
// all (a zone or an expression that no longer parses) waits before it is
// tried again; without it next_at would stay in the past and the loop spin.
const scheduleRetryAfter = time.Hour

// scheduleMaxWait caps one timer arm (S-D5): a wall-clock jump or a host
// suspend delays a firing by at most this long.
const scheduleMaxWait = time.Hour

// scheduleClock is the loop's and the firing function's view of time. The
// zero value means the real clock.
type scheduleClock struct {
	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
}

// scheduleRuntime is the firing machinery's state inside Server. The zero
// value is ready to use.
type scheduleRuntime struct {
	clock scheduleClock

	mu     sync.Mutex
	firing map[string]*sync.Mutex // per schedule id

	wakeOnce sync.Once
	wake     chan struct{}
}

func (r *scheduleRuntime) now() time.Time {
	if r.clock.now != nil {
		return r.clock.now()
	}
	return time.Now()
}

func (r *scheduleRuntime) after(d time.Duration) <-chan time.Time {
	if r.clock.after != nil {
		return r.clock.after(d)
	}
	return time.After(d)
}

func (r *scheduleRuntime) wakeCh() chan struct{} {
	r.wakeOnce.Do(func() { r.wake = make(chan struct{}, 1) })
	return r.wake
}

// firingMutex serializes the firings of one schedule. A firing cannot hold
// the store's mutex across its task-store action (Update rolls memory back
// when its save fails, and an intent for an action that already happened
// must not be rolled back), so this mutex is what makes "the timer and
// `schedule run` cannot both pass step 3" true.
func (r *scheduleRuntime) firingMutex(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firing == nil {
		r.firing = map[string]*sync.Mutex{}
	}
	m, ok := r.firing[id]
	if !ok {
		m = &sync.Mutex{}
		r.firing[id] = m
	}
	return m
}

// pokeSchedules wakes the timer loop so it re-arms against the store's new
// earliest next_at. Every create, edit and delete of a schedule calls it.
func (s *Server) pokeSchedules() {
	select {
	case s.sched.wakeCh() <- struct{}{}:
	default:
	}
}

func scheduleTerminal(st protocol.TaskStatus) bool {
	switch st {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return true
	}
	return false
}

// nextFor is the schedule's next firing instant after t, from its stored
// expression and zone. Zero with a nil error means the expression has no
// further match.
func nextFor(sc *Schedule, t time.Time) (time.Time, error) {
	c, err := parseCron(sc.Cron)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron %q: %w", sc.Cron, err)
	}
	loc, _, err := loadZone(sc.TZ)
	if err != nil {
		return time.Time{}, fmt.Errorf("time zone %q does not load: %w", sc.TZ, err)
	}
	return nextFiring(c, loc, t), nil
}

// firingAct is what steps 1-4 decided for step 5 to carry out.
type firingAct struct {
	sc         Schedule // snapshot at the intent
	creates    bool
	taskID     string // the bound task, or the pre-chosen id
	due, at    time.Time
	expectRun  uint32
	creatorTID protocol.TaskID
}

// fireSchedule fires schedule id (Firing steps 1-6). due is the instant it
// was due, kept in the history entry; manual is `schedule run` (S-D13): the
// entry reads run_now and next_at is left alone. It returns the outcome it
// recorded. A schedule that does not exist records nothing and returns the
// zero outcome (pending); so does a firing whose intent could not be saved,
// which acts on nothing (at most once, S-D22).
func (s *Server) fireSchedule(id string, due time.Time, manual bool) protocol.ScheduleOutcome {
	fm := s.sched.firingMutex(id)
	fm.Lock()
	defer fm.Unlock()
	st := s.cfg.Schedules
	now := s.sched.now()

	var (
		outcome protocol.ScheduleOutcome
		act     *firingAct
	)
	// Steps 1-4 in one write: the decision is made against the schedule as
	// the store holds it under its lock, so an edit cannot slip in between
	// the check and the intent. A skip is its entry plus next_at; an intent
	// is a pending entry plus next_at.
	err := st.Update(id, func(sc *Schedule) error {
		// A failed outcome save rolled an earlier firing's entry back to
		// pending although its action happened: settle it from the task
		// store first, so a creation is not repeated under a new id and
		// step 3 sees the task's real state (S-D22).
		sc.settlePending(func(tid string) (uint32, bool) {
			t, ok := s.tasks.Get(tid)
			return t.Run, ok
		})
		next, nextErr := nextFor(sc, now)
		entry := HistoryEntry{DueAt: due, At: now, TaskID: sc.TaskID}
		record := func(o protocol.ScheduleOutcome, msg string) {
			entry.Outcome, entry.Message = o, msg
			sc.appendHistory(entry)
			outcome = o
		}
		if nextErr != nil && !manual {
			// The schedule cannot be advanced; retry later, never spin .
			record(protocol.ScheduleOutcome_SkippedRefused, nextErr.Error())
			sc.NextAt = now.Add(scheduleRetryAfter)
			return nil
		}
		advance := func() {
			if !manual {
				sc.NextAt = next
			}
		}
		switch sc.State {
		case protocol.ScheduleTaskState_Unbound:
			record(protocol.ScheduleOutcome_SkippedNoTask, "")
			advance()
			return nil
		case protocol.ScheduleTaskState_Bound:
			t, ok := s.tasks.Get(sc.TaskID)
			switch {
			case !ok:
				record(protocol.ScheduleOutcome_SkippedTaskGone, "")
				advance()
				return nil
			case !scheduleTerminal(t.Status):
				record(protocol.ScheduleOutcome_SkippedBusy, "")
				advance()
				return nil
			}
			entry.ExpectRun = t.Run + 1
		default: // pending_create: the creation's id is chosen now
			entry.TaskID = newTaskID()
			entry.ExpectRun = 1
			entry.Creates = true
		}
		entry.Outcome = protocol.ScheduleOutcome_Pending
		sc.appendHistory(entry)
		advance()
		act = &firingAct{sc: sc.clone(), creates: entry.Creates, taskID: entry.TaskID, due: due, at: now,
			expectRun: entry.ExpectRun, creatorTID: taskIDFromHex(sc.Creator)}
		return nil
	})
	if err != nil {
		if err != ErrScheduleNotFound {
			slog.Error("schedule firing: write failed; nothing was done", "schedule", id, "err", err)
		}
		return protocol.ScheduleOutcome_Pending
	}
	if act == nil {
		return outcome
	}

	// Step 5: act, with no schedule-store lock held.
	sc := act.sc
	var actErr error
	if act.creates {
		actErr = s.tasks.CreateWithinLimitsID(act.taskID, sc.Repo, sc.Prompt, protocol.TaskKind_Interactive,
			protocol.ClientKind_Schedule, act.creatorTID, sc.Selector, sc.ExtraArgs, sc.Grant, sc.Agent, sc.Run)
		if actErr == nil {
			s.tasks.SetResumeConversation(act.taskID, sc.ResumeConversation)
		}
	} else {
		u := GrantUpdate{Caps: true, Scope: true, DelegateCaps: true, DelegateScope: true,
			SpawnDepth: true, MaxDescendants: true, To: sc.Grant}
		_, actErr = s.tasks.Resume(act.taskID, sc.Prompt, sc.ExtraArgs, sc.Selector, "", protocol.ClientKind_Schedule,
			u, protocol.TaskKind_Interactive, sc.Agent, act.creatorTID, sc.ResumeConversation, sc.Run)
	}
	queuedAt := s.sched.now()
	if actErr == nil && s.taskHandler != nil && s.taskHandler.OnChange != nil {
		go s.taskHandler.OnChange()
	}

	// Step 6: the outcome, on the entry step 4 wrote. Events may have filled
	// in its started_at / ended_at already; only the outcome fields change.
	if actErr != nil {
		outcome = protocol.ScheduleOutcome_SkippedRefused
	} else if manual {
		outcome = protocol.ScheduleOutcome_RunNow
	} else {
		outcome = protocol.ScheduleOutcome_Fired
	}
	err = st.Update(id, func(sc *Schedule) error {
		for i := len(sc.History) - 1; i >= 0; i-- {
			e := &sc.History[i]
			if e.Outcome != protocol.ScheduleOutcome_Pending || e.TaskID != act.taskID || !e.At.Equal(act.at) || !e.DueAt.Equal(act.due) {
				continue
			}
			e.Outcome = outcome
			if actErr != nil {
				e.Message = actErr.Error()
				return nil
			}
			e.QueuedAt = queuedAt
			// A creation binds the schedule, unless an edit has settled its
			// task state in the meantime (then the entry still reads fired).
			if act.creates && sc.State == protocol.ScheduleTaskState_PendingCreate {
				sc.State, sc.TaskID = protocol.ScheduleTaskState_Bound, act.taskID
			}
			return nil
		}
		return nil // evicted or deleted: nothing to rewrite
	})
	if err != nil && err != ErrScheduleNotFound {
		// The entry stays pending in memory and on disk; the next start
		// settles it from the task store (S-D22).
		slog.Error("schedule firing: outcome write failed", "schedule", id, "err", err)
	}
	return outcome
}

// fireDueSchedules fires every schedule whose next_at has come, once each,
// and reports whether any is still due afterwards (a firing whose write
// failed leaves next_at behind).
func (s *Server) fireDueSchedules() (stillDue bool) {
	now := s.sched.now()
	for _, sc := range s.cfg.Schedules.List() {
		if sc.NextAt.IsZero() || sc.NextAt.After(now) {
			continue
		}
		s.fireSchedule(sc.ID, sc.NextAt, false)
	}
	next, ok := s.cfg.Schedules.EarliestNext()
	return ok && !next.After(now)
}

// runSchedules is the timer loop (S-D5): one timer armed for
// min(EarliestNext - now, 1h), re-armed after every pass and on every poke.
func (s *Server) runSchedules(ctx context.Context) {
	wake := s.sched.wakeCh()
	for {
		d := scheduleMaxWait
		now := s.sched.now()
		if next, ok := s.cfg.Schedules.EarliestNext(); ok {
			if w := next.Sub(now); w < d {
				d = w
			}
		}
		if d < 0 {
			d = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-s.sched.after(d):
		}
		if ctx.Err() != nil {
			return
		}
		if s.fireDueSchedules() {
			// Something stayed due (its write failed). Wait a minute rather
			// than spin; a poke or the timer retries it.
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-s.sched.after(time.Minute):
			}
		}
	}
}

// startSchedules is the start-up pass, after the WAL replay: settle every
// pending entry from the task store (S-D22), complete the entries of runs that
// ended while the server was down, then fire what was missed, once (S-D9).
func (s *Server) startSchedules() {
	st := s.cfg.Schedules
	if err := st.Settle(func(id string) (uint32, bool) {
		t, ok := s.tasks.Get(id)
		return t.Run, ok
	}); err != nil {
		slog.Error("schedules: settle failed", "err", err)
	}
	for _, sc := range st.List() {
		err := st.Update(sc.ID, func(cur *Schedule) error {
			for i := range cur.History {
				e := &cur.History[i]
				if (e.Outcome != protocol.ScheduleOutcome_Fired && e.Outcome != protocol.ScheduleOutcome_RunNow) || !e.EndedAt.IsZero() {
					continue
				}
				t, ok := s.tasks.Get(e.TaskID)
				if !ok || t.Run != e.ExpectRun || !scheduleTerminal(t.Status) || t.EndedAt == nil {
					continue
				}
				e.EndedAt, e.EndReason = t.EndedAt.UTC(), t.EndReason
				if e.QueuedAt.IsZero() {
					e.QueuedAt = e.At
				}
				if e.StartedAt.IsZero() && t.StartedAt != nil {
					e.StartedAt = t.StartedAt.UTC()
				}
			}
			return nil
		})
		if err != nil {
			slog.Error("schedules: completing history failed", "schedule", sc.ID, "err", err)
		}
	}
	s.fireDueSchedules()
}

// scheduleTaskEvent completes history from the task status events the server
// publishes (S-D7, S-D10). It runs outside the TaskStore's lock and takes only
// the schedule store's.
//
//   - assigned: started_at on the entry of that run.
//   - ended: ended_at and the end reason on the entry of that run.
//   - queued by a resume whose ResumedByKind is not schedule: a resumed_by
//     entry. Its wait is not the schedule's, so no later event completes it.
//
// An event whose entry was evicted from the last historyMax matches nothing.
func (s *Server) scheduleTaskEvent(ev protocol.TaskStatusEvent) {
	switch ev.Kind {
	case protocol.StatusEventKind_TaskQueued, protocol.StatusEventKind_TaskAssigned, protocol.StatusEventKind_TaskEnded:
	default:
		return
	}
	tid := hex.EncodeToString(ev.TaskId.Id[:])
	at := s.sched.now()
	if ev.Ts != 0 {
		at = time.Unix(0, int64(ev.Ts)).UTC()
	}
	var f func(*Schedule) bool
	switch ev.Kind {
	case protocol.StatusEventKind_TaskAssigned:
		f = func(sc *Schedule) bool {
			i := sc.runEntry(tid, ev.Run)
			if i < 0 || !sc.History[i].StartedAt.IsZero() {
				return false
			}
			sc.History[i].StartedAt = at
			return true
		}
	case protocol.StatusEventKind_TaskEnded:
		f = func(sc *Schedule) bool {
			i := sc.runEntry(tid, ev.Run)
			if i < 0 || !sc.History[i].EndedAt.IsZero() {
				return false
			}
			sc.History[i].EndedAt, sc.History[i].EndReason = at, ev.EndReason
			return true
		}
	default: // queued
		if ev.Run <= 1 { // a creation, not a resume
			return
		}
		t, ok := s.tasks.Get(tid)
		if !ok || t.ResumedByKind == protocol.ClientKind_Schedule {
			return
		}
		by := t.ResumedByKind
		f = func(sc *Schedule) bool {
			if sc.State != protocol.ScheduleTaskState_Bound || sc.TaskID != tid {
				return false
			}
			for _, e := range sc.History {
				if e.TaskID == tid && e.ExpectRun == ev.Run && e.Outcome != protocol.ScheduleOutcome_Lost {
					return false // already recorded for this run
				}
			}
			sc.appendHistory(HistoryEntry{Outcome: protocol.ScheduleOutcome_ResumedBy, At: at, TaskID: tid,
				ExpectRun: ev.Run, ByKind: by})
			return true
		}
	}
	if _, err := s.cfg.Schedules.UpdateForTask(tid, f); err != nil {
		slog.Error("schedule history: write failed", "task", tid, "err", err)
	}
}

// runEntry is the index of the newest entry for run of task tid that a run
// of the schedule's own started (pending, fired or run_now), or -1.
func (sc *Schedule) runEntry(tid string, run uint32) int {
	for i := len(sc.History) - 1; i >= 0; i-- {
		e := sc.History[i]
		if e.TaskID != tid || e.ExpectRun != run {
			continue
		}
		switch e.Outcome {
		case protocol.ScheduleOutcome_Pending, protocol.ScheduleOutcome_Fired, protocol.ScheduleOutcome_RunNow:
			return i
		}
	}
	return -1
}
