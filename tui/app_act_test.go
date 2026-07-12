package tui

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestActAgeTickRearms verifies the aging tick always re-arms (returns a
// command) regardless of state, so the local re-render loop never dies.
func TestActAgeTickRearms(t *testing.T) {
	a := New(Config{})
	_, cmd := a.Update(actAgeTickMsg{})
	if cmd == nil {
		t.Fatal("actAgeTickMsg must re-arm the tick")
	}
}

func actTaskID(b byte) protocol.TaskID {
	var id protocol.TaskID
	id.Id[0] = b
	return id
}

// TestApplyEventAct_LiveAndTerminal verifies act fields land from a live
// event (with a receipt stamp) and are cleared outright on terminal status
// even when the event still carries the just-stopped session's timestamps.
func TestApplyEventAct_LiveAndTerminal(t *testing.T) {
	a := New(Config{})
	tid := actTaskID(0xaa)
	id := FormatTaskID(tid)

	var ti protocol.TaskInfo
	ti.Id = tid
	ev := protocol.TaskStatusEvent{
		Kind:         protocol.StatusEventKind_TaskActivity,
		TaskId:       tid,
		TaskStatus:   protocol.TaskStatus_Running,
		LastOutputAt: 1770000000000000000,
		OutputIdleMs: 4000,
	}
	a.applyEventAct(&ti, id, ev)
	if ti.LastOutputAt != ev.LastOutputAt || ti.OutputIdleMs != 4000 {
		t.Fatalf("act fields not applied: %+v", ti)
	}
	if _, ok := a.actRecvAt[id]; !ok {
		t.Fatal("receipt time not stamped")
	}

	ev.TaskStatus = protocol.TaskStatus_Succeeded
	ev.Kind = protocol.StatusEventKind_TaskEnded
	a.applyEventAct(&ti, id, ev)
	if ti.LastOutputAt != 0 || ti.OutputIdleMs != 0 {
		t.Fatalf("terminal event must clear act fields: %+v", ti)
	}
	if _, ok := a.actRecvAt[id]; ok {
		t.Fatal("terminal event must drop the receipt stamp")
	}
}

// TestRefreshTasksTable_AgesIdleNotBusy verifies rendering ages an idle
// badge by local elapsed time since receipt, while a busy badge is passed
// through un-aged (it flips only via the server's idle-edge event).
func TestRefreshTasksTable_AgesIdleNotBusy(t *testing.T) {
	a := New(Config{})

	idleID := actTaskID(0x01)
	busyID := actTaskID(0x02)
	idle := protocol.TaskInfo{Id: idleID, Status: protocol.TaskStatus_Running,
		LastOutputAt: 1, OutputIdleMs: 5000, CreatedAt: 2}
	busy := protocol.TaskInfo{Id: busyID, Status: protocol.TaskStatus_Running,
		LastOutputAt: 1, OutputIdleMs: 100, CreatedAt: 1}
	a.tasksByID[FormatTaskID(idleID)] = idle
	a.tasksByID[FormatTaskID(busyID)] = busy
	a.actRecvAt[FormatTaskID(idleID)] = time.Now().Add(-2 * time.Second)
	a.actRecvAt[FormatTaskID(busyID)] = time.Now().Add(-2 * time.Second)

	a.refreshTasksTable()

	rows := a.tasks.rowTasks
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Sorted by descending CreatedAt: idle first.
	if got := rows[0].OutputIdleMs; got < 6900 || got > 8000 {
		t.Errorf("idle row age = %dms, want ~7000ms (5000 wire + ~2000 local)", got)
	}
	if got := rows[1].OutputIdleMs; got != 100 {
		t.Errorf("busy row must not be aged: %dms, want 100ms", got)
	}
}

// TestHasAgingActRow gates the per-tick rebuild: only idle badges age.
func TestHasAgingActRow(t *testing.T) {
	a := New(Config{})
	if a.hasAgingActRow() {
		t.Error("empty table must not age")
	}
	a.tasksByID["busy"] = protocol.TaskInfo{LastOutputAt: 1, OutputIdleMs: 100}
	if a.hasAgingActRow() {
		t.Error("busy-only table must not age")
	}
	a.tasksByID["idle"] = protocol.TaskInfo{LastOutputAt: 1, OutputIdleMs: 5000}
	if !a.hasAgingActRow() {
		t.Error("idle row must age")
	}
}
