package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestExecsModalStatsEventUpdatesTheRow(t *testing.T) {
	m := NewExecsModal()
	m.SetSize(200, 20)
	m.ApplySnapshot([]protocol.ExecRunInfo{{ExecId: 4}})
	m.ApplyEvent(protocol.ExecStatusEvent{Kind: protocol.StatusEventKind_ExecStats,
		Info: protocol.ExecRunInfo{ExecId: 4, StdoutBytes: 2048, Taps: 1}})
	row := m.table.Rows()[0]
	if row[4] != "2.0kB" || row[7] != "1" {
		t.Fatalf("row after stats: %v", row)
	}
	if len(m.table.Rows()) != 1 {
		t.Fatal("a stats event for a known exec must update, not append")
	}
}

func TestExecsModalRowPrintsZeroCounters(t *testing.T) {
	row := execRunInfoRow(&protocol.ExecRunInfo{ExecId: 4}, time.Now())
	if row[3] != "0" || row[5] != "0" || row[6] != "never" || row[7] != "0" {
		t.Fatalf("an idle exec must print zeros, not blanks: %v", row)
	}
}

// Through the App's real key routing (updateKey -> inExecsModal): `t` on the
// execs modal opens the tap on the SELECTED exec, drawn over the modal, and
// Esc closes the tap and leaves the modal up.
func TestApp_TOnTheExecsModalTapsTheSelectedExec(t *testing.T) {
	a := &App{execsModal: NewExecsModal(), cmdresult: NewCmdResult(), width: 160, height: 45}
	a.client = &cli.Client{} // non-nil is enough; the returned cmd is never run
	a.execsModal.SetSize(160, 45)
	a.execsModal.ApplySnapshot([]protocol.ExecRunInfo{{ExecId: 4}, {ExecId: 9}})
	a.execsModal.Open()
	a.updateKey(tea.KeyMsg{Type: tea.KeyDown})

	_, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if cmd == nil || !a.tap.IsOpen() {
		t.Fatal("`t` on the execs modal did not open a tap")
	}
	if got := a.tap.Subject(); got != (tapSubject{"exec", 9}) {
		t.Fatalf("tapped %v, want the selected exec #9", got)
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyEsc})
	if a.tap.IsOpen() || !a.execsModal.IsOpen() {
		t.Fatalf("Esc: tap open=%v, modal open=%v; want the tap closed over a modal still up", a.tap.IsOpen(), a.execsModal.IsOpen())
	}
}
