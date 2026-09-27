package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestIdleWatchersModal_KillTargetsSelectedRowOnly(t *testing.T) {
	m := NewIdleWatchersModal()
	m.SetSize(120, 30)
	ws := []protocol.AwaitIdleWatcherInfo{{WatcherId: 4}, {WatcherId: 9}}
	m.ApplySnapshot(ws)
	m.table.SetCursor(1)
	if !m.BeginKillConfirm() {
		t.Fatal("BeginKillConfirm with a row selected returned false")
	}
	id, ok := m.ConfirmKill()
	if !ok || id != 9 {
		t.Fatalf("ConfirmKill = %d,%v, want 9,true", id, ok)
	}
	if m.IsConfirming() {
		t.Fatal("still confirming after ConfirmKill")
	}
}

func TestIdleWatchersModal_EmptySaysSo(t *testing.T) {
	m := NewIdleWatchersModal()
	m.SetSize(120, 30)
	m.ApplySnapshot(nil)
	if v := stripANSI(m.View()); !strings.Contains(v, "no armed watchers") {
		t.Fatalf("view = %q", v)
	}
}
