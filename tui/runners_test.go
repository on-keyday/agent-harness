package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The runner rows carry the identity. Host is not a key — two slots on one
// machine share a hostname by design — so before this column the rows differed
// only by Agent, and the id an operator needs for `--runner` (and the value
// TaskInfo.assigned_to holds) was reachable only through the detail popup.
func TestRunnerRowsCarryTheIdentity(t *testing.T) {
	m := NewRunners()
	m.SetSize(120, 10) // wide enough for the ID column; see idColumnMinWidth
	a := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{0xAB, 0xCD}}, Status: protocol.RunnerStatus_Idle}
	a.SetHostname([]byte("kvm-ebpf"))
	b := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{0x12, 0x34}}, Status: protocol.RunnerStatus_Idle}
	b.SetHostname([]byte("kvm-ebpf"))
	m.SetRows([]protocol.RunnerInfo{a, b})

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	cols := m.table.Columns()
	if len(rows[0]) != len(cols) {
		t.Fatalf("row has %d cells but the table has %d columns — bubbles indexes "+
			"row[i] per column and panics on a mismatch", len(rows[0]), len(cols))
	}
	idCol := -1
	for i, c := range cols {
		if c.Title == "ID" {
			idCol = i
		}
	}
	if idCol < 0 {
		t.Fatalf("no ID column: %+v", cols)
	}
	if rows[0][idCol] != "abcd0000" || rows[1][idCol] != "12340000" {
		t.Errorf("ID cells = %q / %q, want the two identities", rows[0][idCol], rows[1][idCol])
	}
	// The property the column exists for: same host, and still distinguishable.
	hostCol := -1
	for i, c := range cols {
		if c.Title == "Host" {
			hostCol = i
		}
	}
	if rows[0][hostCol] != rows[1][hostCol] {
		t.Fatal("this test is meaningless unless both rows share a hostname")
	}
	if rows[0][idCol] == rows[1][idCol] {
		t.Error("two slots on one host rendered identically")
	}
}

// The column set varies by width, so the rows must be rebuilt through the same
// decision. bubbles indexes row[i] per COLUMN, and ID is not last: a cell count
// that disagrees either panics or silently renders every following value under
// the wrong header. Both directions of the swap are exercised because only one
// of them empties the rows first.
func TestRunnerColumnSetAndRowsStayInStep(t *testing.T) {
	m := NewRunners()
	r := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{0xAB, 0xCD}}, Status: protocol.RunnerStatus_Idle}
	r.SetHostname([]byte("gmkhost"))
	m.SetRows([]protocol.RunnerInfo{r})

	for _, w := range []int{40, 120, 40, 200, 71, 72} {
		m.SetSize(w, 10)
		cols, rows := m.table.Columns(), m.table.Rows()
		if len(rows) != 1 {
			t.Fatalf("w=%d: got %d rows, want 1", w, len(rows))
		}
		if len(rows[0]) != len(cols) {
			t.Fatalf("w=%d: %d cells against %d columns", w, len(rows[0]), len(cols))
		}
		hasID := false
		for _, c := range cols {
			if c.Title == "ID" {
				hasID = true
			}
		}
		if want := w >= idColumnMinWidth; hasID != want {
			t.Errorf("w=%d: ID column present=%v, want %v", w, hasID, want)
		}
	}
}

// The threshold exists to guarantee a USABLE id, not merely a present column:
// at 3 cells it renders two hex digits and an ellipsis, which is what shipping
// it unconditionally actually did at an 80-column terminal.
func TestIDColumnIsWideEnoughToIdentifyWhenShown(t *testing.T) {
	m := NewRunners()
	m.SetSize(idColumnMinWidth, 10)
	for _, c := range m.table.Columns() {
		if c.Title == "ID" && c.Width < 6 {
			t.Errorf("at the threshold panel width the ID column is %d cells; "+
				"raise idColumnMinWidth rather than show an unusable id", c.Width)
		}
	}
}

// The regression the operator hit: with more runners than fit, the pane showed
// one row fewer than it had room for and the last slot sat blank until the
// cursor moved. Same defect as `db6fa142` fixed for the TASKS table, brought
// back in this pane the moment it gained a conditional column set — because
// swapping columns empties the rows, and the first SetSize runs before any
// runner has arrived.
func TestRunnersTableCursorNeverStaysNegative(t *testing.T) {
	rs := make([]protocol.RunnerInfo, 0, 3)
	for i := 0; i < 3; i++ {
		r := protocol.RunnerInfo{Id: protocol.RunnerID{Id: [16]byte{byte(i + 1)}}}
		r.SetHostname([]byte("gmkhost"))
		rs = append(rs, r)
	}

	// Route 1 (certain): the first SetSize crosses idColumnMinWidth and empties
	// the rows to swap the column set, before any runner has arrived.
	m := NewRunners()
	m.SetSize(120, 10)
	m.SetRows(rs)
	if got := m.table.Cursor(); got < 0 {
		t.Errorf("cursor = %d after the startup resize-then-rows order; "+
			"a negative cursor renders one row short with a blank last slot", got)
	}

	// Route 2 (latent): the list going empty at any point and coming back —
	// every runner disconnecting, or a server restart.
	m2 := NewRunners()
	m2.SetSize(120, 10)
	m2.SetRows(rs)
	m2.SetRows(nil)
	m2.SetRows(rs)
	if got := m2.table.Cursor(); got < 0 {
		t.Errorf("cursor = %d after the list emptied and refilled", got)
	}

	// An empty table legitimately has no selection; do not invent one.
	m3 := NewRunners()
	m3.SetSize(120, 10)
	m3.SetRows(nil)
	if m3.SelectedRunner() != nil {
		t.Error("an empty table reported a selection")
	}
}
