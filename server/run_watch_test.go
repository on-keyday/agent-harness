package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// bgFixture is newSignalFixture's live session with its task Running in
// background mode.
func bgFixture(t *testing.T, cond RunConditions) *signalFixture {
	t.Helper()
	f := newSignalFixture(t)
	f.h.Tasks.Assign(f.idHex, testRunnerID("r1"), "", false)
	if _, err := f.h.Tasks.SwitchRunMode(f.idHex, protocol.RunMode_Background, cond.WithDefaults(protocol.TaskKind_Interactive), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // reports below land strictly after the switch
	return f
}

func waitEnded(t *testing.T, h *TaskHandler, id string, within time.Duration) TaskEntry {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if e, _ := h.Tasks.Get(id); e.Status == protocol.TaskStatus_Cancelled {
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	e, _ := h.Tasks.Get(id)
	t.Fatalf("task not ended within %v: status %v reason %v", within, e.Status, e.EndReason)
	return e
}

func TestRunWatcherEndsOnLifecycleIdleAfterWorking(t *testing.T) {
	f := bgFixture(t, RunConditions{Deadline: time.Hour, TurnEnd: protocol.TurnEndSignal_Lifecycle})
	f.h.armRunWatcher(f.idHex, f.mux)
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Idle)
	e := waitEnded(t, f.h, f.idHex, 3*time.Second)
	if e.EndReason != protocol.EndReason_TurnDoneLifecycle {
		t.Fatalf("end reason %v", e.EndReason)
	}
}

func TestRunWatcherBlockedTimeout(t *testing.T) {
	f := bgFixture(t, RunConditions{Deadline: time.Hour, Blocked: time.Second})
	f.h.armRunWatcher(f.idHex, f.mux)
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Waiting)
	e := waitEnded(t, f.h, f.idHex, 4*time.Second)
	if e.EndReason != protocol.EndReason_BlockedTimeout {
		t.Fatalf("end reason %v", e.EndReason)
	}
}

// Disarming (a switch to foreground) stops the watcher: a later turn end does
// not end the run.
func TestRunWatcherDisarmStopsIt(t *testing.T) {
	f := bgFixture(t, RunConditions{Deadline: time.Hour, TurnEnd: protocol.TurnEndSignal_Lifecycle})
	f.h.armRunWatcher(f.idHex, f.mux)
	f.h.disarmRunWatcher(f.idHex)
	f.lifecycle(protocol.LifecycleState_Working)
	time.Sleep(2 * time.Millisecond)
	f.lifecycle(protocol.LifecycleState_Idle)
	time.Sleep(1500 * time.Millisecond)
	if e, _ := f.h.Tasks.Get(f.idHex); e.Status == protocol.TaskStatus_Cancelled {
		t.Fatalf("a disarmed watcher ended the run (%v)", e.EndReason)
	}
}
