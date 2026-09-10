package runner

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A held child survives the gap on purpose, and three things can end that:
// the server re-adopts it (rebind), the server comes back and refuses it
// (killHeldExcept), or the window passes with no server (killAllHeld). The
// fourth outcome had no branch — the server came back, asked for a rebind, and
// the rebind FAILED — so the child stayed alive with nothing able to reach it
// while a fresh one was spawned in its place.
//
// Observed on the live fleet 2026-09-10: one `hold: rebind failed
// reason="stream lookup failed"` against 24 successful rebinds left a claude
// holding ~440MB and a playwright-mcp child, listed by no task row, with the
// runner still holding its PTY.

// heldEntry is a registered task whose child is running, as liveHeldTasks
// requires: an entry exists from registration, which is before anything is
// spawned.
func heldEntry(cancelled *bool) *taskEntry {
	e := &taskEntry{cancel: func() { *cancelled = true }}
	e.started.Store(true)
	return e
}

func taskIDOf(t *testing.T, hexID string) protocol.TaskID {
	t.Helper()
	var tid protocol.TaskID
	raw, err := hex.DecodeString(hexID)
	if err != nil || len(raw) != len(tid.Id) {
		t.Fatalf("bad task id %q", hexID)
	}
	copy(tid.Id[:], raw)
	return tid
}

// nopSender swallows what reportRebindFailed sends; this test is about the
// CHILD, and the server-facing half already worked.
type nopSender struct{}

func (nopSender) Publish(string, []byte) error { return nil }
func (nopSender) Send([]byte) error            { return nil }
func (nopSender) ID() objproto.ConnectionID    { return objproto.ConnectionID{} }

func TestRebindFailureKillsTheHeldChild(t *testing.T) {
	const id = "70fbad4a6eb6f1e992be8a669f1bcefd"
	killed := false
	reg := NewTaskRegistry()
	reg.put(id, heldEntry(&killed))

	s := &Session{reg: reg, Sender: nopSender{}}
	s.reportRebindFailed(taskIDOf(t, id), "stream lookup failed")

	if !killed {
		t.Error("the child of a failed rebind is still alive; nothing can reach it and a new one takes its place")
	}
	if _, ok := reg.get(id); ok {
		t.Error("the registry still holds the entry, so heldReport would promise it to the next server")
	}
}

// The hold may still cover OTHER tasks — the server rebinds them one at a time
// — so one failure must not end the rest.
func TestRebindFailureLeavesTheOtherHeldChildrenAlone(t *testing.T) {
	const failing = "70fbad4a6eb6f1e992be8a669f1bcefd"
	const other = "c96af19d4f59ff295553120d402b84f0"
	failedKilled, otherKilled := false, false
	reg := NewTaskRegistry()
	reg.put(failing, heldEntry(&failedKilled))
	reg.put(other, heldEntry(&otherKilled))
	reg.arm(protocol.HoldID{}, time.Hour, []string{failing, other}, func() {})
	defer reg.disarm()

	s := &Session{reg: reg, Sender: nopSender{}}
	s.reportRebindFailed(taskIDOf(t, failing), "stream lookup failed")

	if !failedKilled {
		t.Error("the failing task's child survived")
	}
	if otherKilled {
		t.Error("a rebind failure on one task killed another's child")
	}
	// And the hold must stop promising the dead one, or the next report offers
	// a child that no longer exists.
	if reg.heldReportNames(t)[failing] {
		t.Error("the hold still covers the killed task")
	}
	if !reg.heldReportNames(t)[other] {
		t.Error("the hold dropped a task that is still held")
	}
}

// heldReportNames is what the next RunnerHello would promise, by task id.
func (r *TaskRegistry) heldReportNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, ht := range r.heldReport().Tasks {
		out[hex.EncodeToString(ht.TaskId.Id[:])] = true
	}
	return out
}

// The other two refusal reasons reach the same funnel and must not panic: one
// has no entry at all, the other's child is already gone.
func TestRebindFailureToleratesAMissingOrDeadEntry(t *testing.T) {
	const id = "70fbad4a6eb6f1e992be8a669f1bcefd"
	s := &Session{reg: NewTaskRegistry(), Sender: nopSender{}}
	s.reportRebindFailed(taskIDOf(t, id), "no held session for this task")

	killed := false
	reg := NewTaskRegistry()
	e := heldEntry(&killed)
	e.exited.Store(true)
	reg.put(id, e)
	s2 := &Session{reg: reg, Sender: nopSender{}}
	s2.reportRebindFailed(taskIDOf(t, id), "child exited during the gap")
	if _, ok := reg.get(id); ok {
		t.Error("a dead child's entry stayed in the registry")
	}
}
