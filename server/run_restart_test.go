package server

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The progress snapshot carries LifecycleWorkingAt across a deliberate stop,
// so a background run that had worked is known to have worked (A-D13).
func TestProgressSnapshotCarriesLifecycleWorkingAt(t *testing.T) {
	s, path := storeWithWAL(t)
	id := createBG(t, s, protocol.TaskKind_Interactive, bgSpec(time.Hour))
	s.Assign(id, testRunnerID("r1"), "", false)
	now := time.Now().UnixNano()
	s.SetLifecycle(id, protocol.LifecycleState_Working, now, now)
	s.SetLifecycle(id, protocol.LifecycleState_Idle, now+1, now+1)
	want, _ := s.Get(id)
	dir := t.TempDir()
	if err := writeProgressSnapshot(dir, s); err != nil {
		t.Fatal(err)
	}
	fresh := replayInto(t, path)
	if _, err := loadProgressSnapshot(dir, fresh); err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.Get(id); got.LifecycleWorkingAt != want.LifecycleWorkingAt || got.LifecycleWorkingAt == 0 {
		t.Fatalf("LifecycleWorkingAt = %d, want %d", got.LifecycleWorkingAt, want.LifecycleWorkingAt)
	}
}

func TestHeldRunFactsRoundTripAndSweep(t *testing.T) {
	dir := t.TempDir()
	s, _ := storeWithWAL(t)
	srv := &Server{tasks: s, cfg: Config{Logger: slog.Default(), DataDir: dir}}
	if err := os.MkdirAll(holdScreenDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	held := runningTask(t, s, testRunnerID("r1"))
	orphan := runningTask(t, s, testRunnerID("r1"))
	if err := s.MarkHold(held, testRunnerID("r1").Hex(), "cafe", time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	f := heldRunFacts{ProgramWorkingAt: 42, SawOutput: true}
	for _, id := range []string{held, orphan} {
		if err := srv.writeHeldRunFacts(id, f); err != nil {
			t.Fatal(err)
		}
	}
	srv.sweepHeldScreens()
	if _, err := os.Stat(holdRunFactsPath(dir, orphan)); err == nil {
		t.Error("run facts whose task is not Held survived the sweep")
	}
	if got, ok := srv.readHeldRunFacts(held); !ok || got != f {
		t.Fatalf("read = %+v, %v", got, ok)
	}
	if _, ok := srv.readHeldRunFacts(held); ok {
		t.Fatal("run facts read twice")
	}
}

// readoptedRun builds what rebindHeldSessions builds for a background task:
// a new mux loaded with the previous server's capture, with (or without) the
// carried run facts, its run watcher armed.
func readoptedRun(t *testing.T, states []string, carry bool) (*TaskHandler, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	id := createBG(t, h.Tasks, protocol.TaskKind_Interactive,
		RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{Deadline: time.Hour}.WithDefaults(protocol.TaskKind_Interactive)})
	h.Tasks.Assign(id, testRunnerID("r1"), "", false)
	time.Sleep(2 * time.Millisecond)

	before := newFakeStream(t)
	old := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, before, NewRingBuffer(1<<20), SessionHooks{})
	t.Cleanup(old.Stop)
	for _, st := range states {
		before.QueueRead(makeWireFrame(1, []byte("\x1b]7501;state="+st+"\x1b\\")))
		want := st
		waitFor(t, func() bool { s, _, _ := old.Program(); return programStateOf(want) == s })
		time.Sleep(2 * time.Millisecond)
	}
	start := time.Unix(0, 0)
	if e, _ := h.Tasks.Get(id); e.StartedAt != nil {
		start = *e.StartedAt
	}
	facts := heldRunFacts{SawOutput: old.LastOutputUnixNano() > start.UnixNano()}
	facts.ProgramWorkingAt, _ = func() (int64, int64) { at, w := old.ProgramStamps(); return w, at }()

	fresh := NewSessionMux(ctx, id, protocol.TaskKind_Interactive, newFakeStream(t), NewRingBuffer(1<<20), SessionHooks{})
	t.Cleanup(fresh.Stop)
	if _, _, err := fresh.loadHeldCapture(old.heldCapture()); err != nil {
		t.Fatal(err)
	}
	if carry {
		fresh.carryRunFacts(facts)
	}
	h.Sessions.Add(id, fresh)
	h.armRunWatcher(id, fresh)
	return h, id
}

func TestReadoptedBackgroundEndsOnRestoredIdleWhenItHadWorked(t *testing.T) {
	h, id := readoptedRun(t, []string{"working", "done"}, true)
	e := waitEnded(t, h, id, 3*time.Second)
	if e.EndReason != protocol.EndReason_TurnDoneProgram {
		t.Fatalf("end reason %v", e.EndReason)
	}
}

func TestReadoptedBackgroundDoesNotEndWhenItNeverWorked(t *testing.T) {
	h, id := readoptedRun(t, []string{"idle"}, true)
	time.Sleep(1500 * time.Millisecond)
	if e, _ := h.Tasks.Get(id); e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("a run that never worked ended (%v)", e.EndReason)
	}
}

// Without the carried facts (no file, or unreadable) the re-adopted run falls
// back to the per-mux floor: it does not end on the restored state, and runs
// on to its deadline — failing toward not ending a run.
func TestReadoptedBackgroundWithMissingFactsRunsToDeadline(t *testing.T) {
	h, id := readoptedRun(t, []string{"working", "done"}, false)
	time.Sleep(1500 * time.Millisecond)
	e, _ := h.Tasks.Get(id)
	if e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("ended on the restored state without the carried facts (%v)", e.EndReason)
	}
	if e.DeadlineAt == 0 {
		t.Fatal("the run lost its deadline")
	}
}
