package server

import (
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// storeWithWAL builds a TaskStore backed by a real WAL in a temp dir, so a test
// can assert what a NEXT server would replay rather than only what this one
// holds in memory.
func storeWithWAL(t *testing.T) (*TaskStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	wal, err := OpenWAL(path)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	t.Cleanup(func() { _ = wal.Close() })
	s := NewTaskStore()
	s.SetWAL(wal)
	return s, path
}

func replayInto(t *testing.T, path string) *TaskStore {
	t.Helper()
	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	return fresh
}

func runningTask(t *testing.T, s *TaskStore, runner protocol.RunnerID) string {
	t.Helper()
	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	s.Assign(id, runner, "/wt", false)
	return id
}

func TestMarkHoldSurvivesAReplayAsHeld(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)

	deadline := time.Now().Add(90 * time.Second).UnixNano()
	if err := s.MarkHold(id, rid.Hex(), "cafe", deadline); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	fresh := replayInto(t, path)
	got, ok := fresh.Get(id)
	if !ok {
		t.Fatal("task absent after replay")
	}
	if got.Status != protocol.TaskStatus_Held {
		t.Errorf("status = %v, want Held", got.Status)
	}
	if got.HoldID != "cafe" || got.HoldDeadline != deadline {
		t.Errorf("hold = %q/%d, want cafe/%d", got.HoldID, got.HoldDeadline, deadline)
	}
}

// The race the ack window opens: a TaskFinished can land while acks are being
// collected. Because replay is order-sensitive, a task_held written after that
// task's task_finished would win and the next server would offer a task whose
// child has exited. MarkHold refuses instead.
func TestMarkHoldRefusesATaskThatJustFinished(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	s.Finish(id, 0, nil)

	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().UnixNano()); err == nil {
		t.Fatal("MarkHold accepted a finished task")
	}
	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	if got.Status == protocol.TaskStatus_Held {
		t.Error("a finished task replayed as Held — the hold record was written anyway")
	}
}

func TestMarkReadoptedClosesTheHoldForASecondRestart(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	if err := s.MarkReadopted(id, rid.Hex(), "cafe", false); err != nil {
		t.Fatalf("MarkReadopted: %v", err)
	}

	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	// NOT Running: ReplayEvents' own tail sweeps a still-Running task to
	// Failed("server_restart"), and that is correct here — by the time this
	// record is replayed the task was running again with no hold. What must
	// hold is that it is no longer HELD and carries no hold id, because that
	// is what stops a second restart from offering it for re-adoption against
	// the previous hold.
	if got.Status == protocol.TaskStatus_Held {
		t.Errorf("status = Held after a readopt — the hold was never closed")
	}
	if got.HoldID != "" || got.HoldDeadline != 0 {
		t.Errorf("hold not cleared: %q/%d — a second restart would re-offer this task",
			got.HoldID, got.HoldDeadline)
	}
}

// A cancel while held must win on replay, since that is the only thing that
// tells the next server not to offer the task (CancelTask itself can never be
// delivered to a held task's runner).
func TestCancelAfterHoldWinsOnReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}
	s.Cancel(id)

	fresh := replayInto(t, path)
	got, _ := fresh.Get(id)
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Errorf("status = %v, want Cancelled", got.Status)
	}
}

// An older binary ignores an unknown record type rather than failing the file.
// The rollback claim in the spec's §4 rests on this, so it is measured here
// rather than asserted there.
func TestUnknownWALRecordTypeIsIgnoredOnReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.wal.Write(WALEvent{Type: "task_invented_later", TaskID: id, Ts: time.Now().UnixNano()}); err != nil {
		t.Fatalf("write: %v", err)
	}

	fresh := replayInto(t, path)
	got, ok := fresh.Get(id)
	if !ok {
		t.Fatal("an unknown record type cost the whole file")
	}
	// Failed("server_restart") is replay's own verdict on a task that was
	// Running when the server went away; what this test is about is that the
	// unknown record cost nothing — the task is still there and its status is
	// the one the KNOWN records produce.
	if got.Status != protocol.TaskStatus_Failed {
		t.Errorf("status = %v, want Failed (the unknown record must be inert)", got.Status)
	}
}

// The sweep at the end of ReplayEvents forces every still-Running task to
// Failed("server_restart"). Held must be outside it: that status is the one
// claim the sweep exists to deny for everything else — the child is alive on a
// runner that agreed to keep it. If this ever goes red, every held task comes
// back Failed and the whole feature is inert while looking implemented.
func TestReplaySweepDoesNotFailAHeldTask(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	held := runningTask(t, s, rid)
	interrupted := runningTask(t, s, rid)
	deadline := time.Now().Add(90 * time.Second).UnixNano()
	if err := s.MarkHold(held, rid.Hex(), "cafe", deadline); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	fresh := replayInto(t, path)
	if got, _ := fresh.Get(held); got.Status != protocol.TaskStatus_Held {
		t.Errorf("held task status = %v, want Held", got.Status)
	}
	// The control: a task that was merely Running IS swept, so the test proves
	// an exemption rather than the absence of a sweep.
	if got, _ := fresh.Get(interrupted); got.Status != protocol.TaskStatus_Failed {
		t.Errorf("un-held task status = %v, want Failed — the sweep did not run at all", got.Status)
	}
}

// MarkFailed must refuse a Held task. The guard is inside MarkFailed rather
// than at its one current call site precisely so the NEXT caller inherits it:
// a runner-disconnected failure written over a live hold leaves the WAL saying
// Failed while the child runs on, and the disconnect is the CAUSE of a hold,
// never a reason to end one.
func TestMarkFailedRefusesAHeldTask(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	deadline := time.Now().Add(time.Minute).UnixNano()
	if err := s.MarkHold(id, rid.Hex(), "cafe", deadline); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	s.MarkFailed(id, "runner_disconnected")

	if got, _ := s.Get(id); got.Status != protocol.TaskStatus_Held {
		t.Errorf("in-memory status = %v, want Held", got.Status)
	}
	fresh := replayInto(t, path)
	if got, _ := fresh.Get(id); got.Status != protocol.TaskStatus_Held {
		t.Errorf("replayed status = %v, want Held — a task_failed was written over the hold", got.Status)
	}
}

// FailHeld is the one sanctioned exit from Held, and it must be reachable —
// the pair with the test above is what makes "refused" a guard rather than a
// dead end.
func TestFailHeldIsTheDoorOutOfHeld(t *testing.T) {
	s, path := storeWithWAL(t)
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	id := runningTask(t, s, rid)
	if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatalf("MarkHold: %v", err)
	}

	s.FailHeld(id, "hold_expired")

	got, _ := s.Get(id)
	if got.Status != protocol.TaskStatus_Failed {
		t.Fatalf("status = %v, want Failed", got.Status)
	}
	if string(got.ErrorMsg) != "hold_expired" {
		t.Errorf("reason = %q, want hold_expired", got.ErrorMsg)
	}
	if got.HoldID != "" || got.HoldDeadline != 0 {
		t.Errorf("hold fields not cleared: %q/%d", got.HoldID, got.HoldDeadline)
	}
	fresh := replayInto(t, path)
	if g, _ := fresh.Get(id); g.Status != protocol.TaskStatus_Failed {
		t.Errorf("replayed status = %v, want Failed", g.Status)
	}
}

// expireHeldTasks fails what is past its deadline and reports the earliest
// deadline still outstanding, which is what the single timer arms on. A
// sweeper's interval would be re-deriving a schedule the replay already knows.
func TestExpireHeldTasksFailsThePastAndReportsTheNext(t *testing.T) {
	s, _ := storeWithWAL(t)
	srv := &Server{tasks: s, cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
	var rid protocol.RunnerID
	rid.Id[0] = 0xab

	past := runningTask(t, s, rid)
	future := runningTask(t, s, rid)
	futureDeadline := time.Now().Add(time.Hour).UnixNano()
	if err := s.MarkHold(past, rid.Hex(), "cafe", time.Now().Add(-time.Second).UnixNano()); err != nil {
		t.Fatalf("MarkHold past: %v", err)
	}
	if err := s.MarkHold(future, rid.Hex(), "cafe", futureDeadline); err != nil {
		t.Fatalf("MarkHold future: %v", err)
	}

	next := srv.expireHeldTasks(time.Now().UnixNano())

	if got, _ := s.Get(past); got.Status != protocol.TaskStatus_Failed || string(got.ErrorMsg) != "hold_expired" {
		t.Errorf("expired task = %v/%q, want Failed/hold_expired", got.Status, got.ErrorMsg)
	}
	if got, _ := s.Get(future); got.Status != protocol.TaskStatus_Held {
		t.Errorf("unexpired task = %v, want Held", got.Status)
	}
	if next != futureDeadline {
		t.Errorf("next deadline = %d, want %d", next, futureDeadline)
	}
}

// A capture is deleted on read, and one whose task is no longer Held is swept.
// Together they are what keeps a stale snapshot from repainting a LATER
// session with a screen from before the restart.
func TestHeldScreenIsConsumedOnceAndSweptWhenOrphaned(t *testing.T) {
	dir := t.TempDir()
	s, _ := storeWithWAL(t)
	srv := &Server{tasks: s, cfg: Config{Logger: slog.Default(), DataDir: dir}}
	if err := os.MkdirAll(holdScreenDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	var rid protocol.RunnerID
	rid.Id[0] = 0xab
	held := runningTask(t, s, rid)
	orphan := runningTask(t, s, rid)
	if err := s.MarkHold(held, rid.Hex(), "cafe", time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{held, orphan} {
		if err := os.WriteFile(holdScreenPath(dir, id), []byte("\x1b[H"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	srv.sweepHeldScreens()
	if _, err := os.Stat(holdScreenPath(dir, orphan)); err == nil {
		t.Error("a capture whose task is not Held survived the sweep")
	}
	if got := srv.readHeldScreen(held); string(got) != "\x1b[H" {
		t.Errorf("read = %q, want the capture", got)
	}
	if got := srv.readHeldScreen(held); got != nil {
		t.Errorf("second read = %q, want nil — the capture must be consumed once", got)
	}
}

// readoptHeldTasks accepts only when all three conditions hold, and the three
// refusals are separate tests' worth of behaviour in one table because they
// share every line of setup.
func TestReadoptRequiresTaskHoldIdAndIdentity(t *testing.T) {
	var mine, other protocol.RunnerID
	mine.Id[0] = 0xaa
	other.Id[0] = 0xbb

	cases := []struct {
		name    string
		mangle  func(s *TaskStore, id string, report *protocol.HeldTasksReport)
		accepts bool
	}{
		{"everything matches", func(*TaskStore, string, *protocol.HeldTasksReport) {}, true},
		{"hold id from a previous shutdown", func(s *TaskStore, id string, r *protocol.HeldTasksReport) {
			r.HoldId.Id[0] ^= 0xff
		}, false},
		{"task was never held", func(s *TaskStore, id string, r *protocol.HeldTasksReport) {
			s.FailHeld(id, "hold_expired")
		}, false},
		{"unknown task id", func(s *TaskStore, id string, r *protocol.HeldTasksReport) {
			r.Tasks[0].TaskId.Id[0] ^= 0xff
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := storeWithWAL(t)
			srv := &Server{tasks: store, registry: NewRegistry(), cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
			id := runningTask(t, store, mine)
			var hold protocol.HoldID
			hold.Id[0] = 0xc0
			holdHex := hex.EncodeToString(hold.Id[:])
			if err := store.MarkHold(id, mine.Hex(), holdHex, time.Now().Add(time.Hour).UnixNano()); err != nil {
				t.Fatal(err)
			}
			var tid protocol.TaskID
			raw, _ := hex.DecodeString(id)
			copy(tid.Id[:], raw)
			report := protocol.HeldTasksReport{HoldId: hold}
			report.SetTasks([]protocol.HeldTask{{TaskId: tid, Ticket: [16]byte{1}}})
			tc.mangle(store, id, &report)

			res := srv.readoptHeldTasks(mine, report)

			if got := len(res.Accepted) == 1; got != tc.accepts {
				t.Errorf("accepted=%v, want %v", got, tc.accepts)
			}
			if !tc.accepts {
				// A refusal must also end the hold, or the child lingers for
				// the whole window with nobody coming for it.
				if st, _ := store.Get(id); st.Status == protocol.TaskStatus_Held {
					t.Error("task is still Held after a refusal")
				}
			}
		})
	}
}

// A runner reporting a task the log says belongs to somebody else is refused —
// the identity comparison is the only thing standing between a report and
// another runner's task.
func TestReadoptRefusesAnotherRunnersTask(t *testing.T) {
	store, _ := storeWithWAL(t)
	srv := &Server{tasks: store, registry: NewRegistry(), cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
	var owner, thief protocol.RunnerID
	owner.Id[0] = 0xaa
	thief.Id[0] = 0xbb
	id := runningTask(t, store, owner)
	var hold protocol.HoldID
	hold.Id[0] = 0xc0
	if err := store.MarkHold(id, owner.Hex(), hex.EncodeToString(hold.Id[:]), time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(id)
	copy(tid.Id[:], raw)
	report := protocol.HeldTasksReport{HoldId: hold}
	report.SetTasks([]protocol.HeldTask{{TaskId: tid, Ticket: [16]byte{9}}})

	res := srv.readoptHeldTasks(thief, report)

	if len(res.Accepted) != 0 {
		t.Fatal("a runner re-adopted a task assigned to another identity")
	}
	if st, _ := store.Get(id); st.Status != protocol.TaskStatus_Held {
		t.Errorf("the OWNER's hold was ended by a stranger's report: %v", st.Status)
	}
}

// A held task the reconnecting runner does NOT name is dead: the runner is the
// authority on what it kept, and it just enumerated it.
func TestReadoptFailsHeldTasksTheReportOmits(t *testing.T) {
	store, _ := storeWithWAL(t)
	srv := &Server{tasks: store, registry: NewRegistry(), cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
	var rid protocol.RunnerID
	rid.Id[0] = 0xaa
	kept := runningTask(t, store, rid)
	dropped := runningTask(t, store, rid)
	var hold protocol.HoldID
	hold.Id[0] = 0xc0
	holdHex := hex.EncodeToString(hold.Id[:])
	for _, id := range []string{kept, dropped} {
		if err := store.MarkHold(id, rid.Hex(), holdHex, time.Now().Add(time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(kept)
	copy(tid.Id[:], raw)
	report := protocol.HeldTasksReport{HoldId: hold}
	report.SetTasks([]protocol.HeldTask{{TaskId: tid, Ticket: [16]byte{1}}})

	srv.readoptHeldTasks(rid, report)

	if st, _ := store.Get(dropped); st.Status != protocol.TaskStatus_Failed ||
		string(st.ErrorMsg) != "not_held_by_runner" {
		t.Errorf("omitted task = %v/%q, want Failed/not_held_by_runner", st.Status, st.ErrorMsg)
	}
	if st, _ := store.Get(kept); st.Status == protocol.TaskStatus_Failed {
		t.Error("the reported task was failed too")
	}
}

// The hold's two edges must PUBLISH. Nothing else does it for them: every other
// non-terminal transition is repaired incidentally by the next task_activity,
// and a held task has no mux to produce one — so between the hold and the
// re-adoption an event-driven client would be told nothing at all.
//
// What the readopt event carries is the other half of the claim. A session's
// settled status is Detached (a live child, no client), and the store used to
// reach it through Running via a second call; publishing that intermediate
// would announce a state the session was never in.
func TestHoldAndReadoptPublishTheSettledStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind protocol.TaskKind
		want protocol.TaskStatus
	}{
		{"a session lands detached", protocol.TaskKind_Interactive, protocol.TaskStatus_Detached},
		{"a oneshot lands running", protocol.TaskKind_Oneshot, protocol.TaskStatus_Running},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := storeWithWAL(t)
			var rid protocol.RunnerID
			rid.Id[0] = 0xab
			id := s.Create("/repo", "p", tc.kind, protocol.ClientKind_Unspecified,
				protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
			s.Assign(id, rid, "/wt", false)

			var holds []string
			var readopts []protocol.TaskStatus
			s.OnHold = func(gotID string) { holds = append(holds, gotID) }
			s.OnReadopt = func(_ string, st protocol.TaskStatus) { readopts = append(readopts, st) }

			if err := s.MarkHold(id, rid.Hex(), "cafe", time.Now().Add(time.Hour).UnixNano()); err != nil {
				t.Fatalf("MarkHold: %v", err)
			}
			if len(holds) != 1 || holds[0] != id {
				t.Fatalf("OnHold fired %v, want exactly [%s]", holds, id)
			}

			session := tc.kind != protocol.TaskKind_Oneshot
			if err := s.MarkReadopted(id, rid.Hex(), "cafe", session); err != nil {
				t.Fatalf("MarkReadopted: %v", err)
			}
			// Exactly one, and it is the settled status — not a Running flash
			// followed by a correction.
			if len(readopts) != 1 || readopts[0] != tc.want {
				t.Fatalf("OnReadopt fired %v, want exactly [%v]", readopts, tc.want)
			}
			got, _ := s.Get(id)
			if got.Status != tc.want {
				t.Errorf("stored status = %v, want %v", got.Status, tc.want)
			}
			if tc.want == protocol.TaskStatus_Detached {
				if got.IsAttached {
					t.Error("a re-adopted session reads as attached; no client is on it")
				}
				if got.DetachedAt == 0 {
					t.Error("DetachedAt not stamped — the fused transition dropped what SetDetached did")
				}
			}
		})
	}
}

// The re-adoption path's own end of the same claim: what readoptHeldTasks leaves
// behind for a session is Detached, in one event.
func TestReadoptLeavesASessionDetached(t *testing.T) {
	store, _ := storeWithWAL(t)
	srv := &Server{tasks: store, registry: NewRegistry(), cfg: Config{Logger: slog.Default(), DataDir: t.TempDir()}}
	var rid protocol.RunnerID
	rid.Id[0] = 0xaa
	id := store.Create("/repo", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, Scope{}, "")
	store.Assign(id, rid, "/wt", false)
	var hold protocol.HoldID
	hold.Id[0] = 0xc0
	if err := store.MarkHold(id, rid.Hex(), hex.EncodeToString(hold.Id[:]), time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	var published []protocol.TaskStatus
	store.OnReadopt = func(_ string, st protocol.TaskStatus) { published = append(published, st) }
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(id)
	copy(tid.Id[:], raw)
	report := protocol.HeldTasksReport{HoldId: hold}
	report.SetTasks([]protocol.HeldTask{{TaskId: tid, Ticket: [16]byte{1}}})

	res := srv.readoptHeldTasks(rid, report)

	if len(res.Accepted) != 1 {
		t.Fatalf("accepted %d, want 1", len(res.Accepted))
	}
	if len(res.Rebind) != 1 || res.Rebind[0] != id {
		t.Errorf("rebind = %v, want [%s] — a session needs a fresh stream", res.Rebind, id)
	}
	if got, _ := store.Get(id); got.Status != protocol.TaskStatus_Detached {
		t.Errorf("status = %v, want Detached", got.Status)
	}
	if len(published) != 1 || published[0] != protocol.TaskStatus_Detached {
		t.Errorf("published %v, want exactly [Detached]", published)
	}
}
