package tui

import (
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func connEvent(cid string, role protocol.ConnRole) protocol.ConnStatusEvent {
	ev := protocol.ConnStatusEvent{Kind: protocol.StatusEventKind_ConnOpened}
	ev.Info.SetCid([]byte(cid))
	ev.Info.Role = role
	ev.Info.SetIdentified(true)
	return ev
}

func trsfConnState(cid string, role protocol.ConnRole, loss uint64) protocol.TrsfConnState {
	var s protocol.TrsfConnState
	s.SetCid([]uint8(cid))
	s.Role = role
	s.SetCounters([]protocol.TrsfCounter{
		{Key: protocol.TrsfCounterKey_Cwnd, Value: 131072},
		{Key: protocol.TrsfCounterKey_LossEvents, Value: loss},
	})
	return s
}

// TestConnsModalToggleDoesNotPanicAtAnyWidth is the item-34 shape: bubbles
// re-renders on each of SetRows and SetColumns and indexes row[i] PER COLUMN,
// so a moment holding twelve columns against six-cell rows is an
// index-out-of-range. The runners pane hit exactly this within minutes of
// gaining a conditional column set; this modal swaps 6 for 12 on a keystroke.
//
// Widths included below 80 on purpose: fitColumns squeezes to a floor, and the
// squeezed path is the one that has to survive too.
func TestConnsModalToggleDoesNotPanicAtAnyWidth(t *testing.T) {
	for _, w := range []int{40, 80, 120, 200} {
		for _, rows := range []int{0, 1, 5} {
			m := NewConnsModal()
			m.Open()
			m.SetSize(w, 24)
			for i := 0; i < rows; i++ {
				m.ApplyEvent(connEvent(string(rune('a'+i))+":conn", protocol.ConnRole_Cli))
			}
			// Both directions, twice, with a reading landing in between —
			// the sequence an operator produces by leaning on 't'.
			m.ToggleTrsf()
			m.ApplyTrsf([]protocol.TrsfConnState{
				trsfConnState("a:conn", protocol.ConnRole_Cli, 1),
			}, 1)
			m.SetSize(w, 24)
			m.ToggleTrsf()
			m.SetSize(w, 24)
			m.ToggleTrsf()
			_ = m.View()
			if t.Failed() {
				t.Fatalf("width=%d rows=%d", w, rows)
			}
		}
	}
}

// TestConnsModalToggleSwapsTheColumnSet: the two modes are different questions
// and must not share a header.
func TestConnsModalToggleSwapsTheColumnSet(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	if got := len(m.table.Columns()); got != len(identityColumns()) {
		t.Fatalf("identity columns = %d, want %d", got, len(identityColumns()))
	}
	if !m.ToggleTrsf() {
		t.Fatal("ToggleTrsf reported identity mode after switching to the reading")
	}
	if got := len(m.table.Columns()); got != len(trsfColumns()) {
		t.Errorf("trsf columns = %d, want %d", got, len(trsfColumns()))
	}
	if m.ToggleTrsf() {
		t.Error("ToggleTrsf reported the reading after switching back")
	}
	if got := len(m.table.Columns()); got != len(identityColumns()) {
		t.Errorf("back to identity: columns = %d, want %d", got, len(identityColumns()))
	}
}

// TestConnsModalToggleKeepsTheSelectedConnection: the row INDEX is meaningless
// across the swap — the two modes are fed by different sources and can hold
// different rows — so the cursor follows the cid.
func TestConnsModalToggleKeepsTheSelectedConnection(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	for _, cid := range []string{"c1", "c2", "c3"} {
		m.ApplyEvent(connEvent(cid, protocol.ConnRole_Cli))
	}
	m.table.SetCursor(2) // c3

	m.ToggleTrsf()
	// The reading arrives in a different order, and carries an extra row the
	// event stream has not seen.
	m.ApplyTrsf([]protocol.TrsfConnState{
		trsfConnState("c9", protocol.ConnRole_Runner, 0),
		trsfConnState("c3", protocol.ConnRole_Cli, 0),
		trsfConnState("c1", protocol.ConnRole_Cli, 0),
	}, 1)
	if got := m.selectedCID(); got != "c3" {
		t.Errorf("after the switch the cursor is on %q, want c3", got)
	}
	m.ToggleTrsf()
	if got := m.selectedCID(); got != "c3" {
		t.Errorf("after switching back the cursor is on %q, want c3", got)
	}
}

// TestConnsModalRetargetResetsTheSeries: the counters of one answerer are not
// the counters of another, and the elapsed between the last reading and the
// first from a new target measures nothing that happened on it. Carrying
// either over would print a delta over an interval the new answerer never
// lived through.
func TestConnsModalRetargetResetsTheSeries(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	m.EnterTrsf("")
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Runner, 1)}, 1_000_000_000)
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Runner, 4)}, 2_000_000_000)
	if got := m.trsfRows[0].LossD; got != "3" {
		t.Fatalf("second reading on one target: LossD = %q, want 3", got)
	}

	m.TargetSelectedRunner() // the row is a runner, so this retargets
	if m.TrsfTarget() != "c1" {
		t.Fatalf("target = %q, want c1", m.TrsfTarget())
	}
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Runner, 9)}, 3_000_000_000)
	if got := m.trsfRows[0].LossD; got != "-" {
		t.Errorf("first reading from a NEW target: LossD = %q, want %q", got, "-")
	}
}

// TestConnsModalSelectedRunnerOnlyOnRunnerRows: any other role is one end of a
// connection the server already reports, so there is nothing separate to ask.
func TestConnsModalSelectedRunnerOnlyOnRunnerRows(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	m.EnterTrsf("")
	m.ApplyTrsf([]protocol.TrsfConnState{
		trsfConnState("cli-conn", protocol.ConnRole_Cli, 0),
		trsfConnState("runner-conn", protocol.ConnRole_Runner, 0),
	}, 1)

	m.table.SetCursor(0)
	if _, ok := m.SelectedRunnerCID(); ok {
		t.Error("a cli row offered itself as a trsf target")
	}
	m.table.SetCursor(1)
	cid, ok := m.SelectedRunnerCID()
	if !ok || cid != "runner-conn" {
		t.Errorf("runner row: got (%q,%v), want (runner-conn,true)", cid, ok)
	}
}

// TestConnsModalCloseEndsTheSeries: reopening must not show a delta across the
// gap the modal was shut for. The counters advanced during it and nothing
// measured that interval.
func TestConnsModalCloseEndsTheSeries(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	m.EnterTrsf("")
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Cli, 1)}, 1_000_000_000)
	m.Close()

	if m.IsTrsf() {
		t.Error("a closed modal is still in the reading; reopening would show stale rows")
	}
	m.Open()
	m.EnterTrsf("")
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Cli, 7)}, 9_000_000_000)
	if got := m.trsfRows[0].LossD; got != "-" {
		t.Errorf("first reading after reopening: LossD = %q, want %q", got, "-")
	}
}

// TestConnsModalErrorKeepsTheRows: a refused or failed read says nothing about
// whether the numbers already on screen were true when they were taken.
func TestConnsModalErrorKeepsTheRows(t *testing.T) {
	m := NewConnsModal()
	m.Open()
	m.SetSize(200, 24)
	m.EnterTrsf("")
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Cli, 1)}, 1)

	m.SetTrsfError(errors.New("trsf: the runner did not answer"))
	if len(m.trsfRows) != 1 {
		t.Errorf("rows dropped on an error: %d", len(m.trsfRows))
	}
	if got := m.View(); got == "" {
		t.Error("empty view")
	}
	m.ApplyTrsf([]protocol.TrsfConnState{trsfConnState("c1", protocol.ConnRole_Cli, 2)}, 2)
	if m.trsfErr != "" {
		t.Errorf("a successful reading left the error up: %q", m.trsfErr)
	}
}

// TestConnsModalWatchIntervalRejectsNonPositive: a zero or negative interval
// would spin the poll, so the default stands instead.
func TestConnsModalWatchIntervalRejectsNonPositive(t *testing.T) {
	m := NewConnsModal()
	if m.TrsfEvery() != DefaultTrsfInterval {
		t.Fatalf("default = %v, want %v", m.TrsfEvery(), DefaultTrsfInterval)
	}
	m.SetTrsfEvery(0)
	if m.TrsfEvery() != DefaultTrsfInterval {
		t.Errorf("zero interval took effect: %v", m.TrsfEvery())
	}
	m.SetTrsfEvery(200 * time.Millisecond)
	if m.TrsfEvery() != 200*time.Millisecond {
		t.Errorf("interval = %v, want 200ms", m.TrsfEvery())
	}
}
