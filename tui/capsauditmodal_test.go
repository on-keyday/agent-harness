package tui

import (
	"encoding/hex"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func auditApp(t *testing.T) (*App, string) {
	t.Helper()
	a := New(Config{})
	a.width, a.height = 160, 40
	var sp, obs, done protocol.TaskID
	sp.Id[0], obs.Id[0], done.Id[0] = 0xab, 0xcd, 0xef
	setAuditTasks(a,
		protocol.TaskInfo{Id: sp, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_Spawn | protocol.Capability_ExecCowrite},
		protocol.TaskInfo{Id: obs, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_BoardObserve},
		protocol.TaskInfo{Id: done, Status: protocol.TaskStatus_Succeeded, Capabilities: protocol.Capability_Purge},
	)
	return a, hex.EncodeToString(sp.Id[:])
}

// setAuditTasks adds tasks to the listing the way a snapshot does, through
// tasksByID and the table rebuild.
func setAuditTasks(a *App, ts ...protocol.TaskInfo) {
	for _, t := range ts {
		a.tasksByID[FormatTaskID(t.Id)] = t
	}
	a.refreshTasksTable()
}

func press(t *testing.T, a *App, keys ...tea.KeyMsg) *App {
	t.Helper()
	for _, k := range keys {
		m, _ := a.Update(k)
		a = m.(*App)
	}
	return a
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var enter = tea.KeyMsg{Type: tea.KeyEnter}
var down = tea.KeyMsg{Type: tea.KeyDown}

func TestIOpensCapsAudit(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"))
	if !a.capsAuditModal.IsOpen() {
		t.Fatal("i did not open the caps audit modal")
	}
	if v := a.capsAuditModal.View(); !strings.Contains(v, "spawn") || strings.Contains(v, "purge") {
		t.Fatalf("modal view lacks spawn or shows the ended task's purge:\n%s", v)
	}
}

func TestCapsAuditModalExpandsBit(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), enter) // cursor starts on the first row: spawn
	if !strings.Contains(a.capsAuditModal.View(), "ab000000") {
		t.Fatalf("enter on spawn did not list its holder:\n%s", a.capsAuditModal.View())
	}
}

func TestCapsAuditModalToggles(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), runes("a"))
	if !strings.Contains(a.capsAuditModal.View(), "purge") {
		t.Fatal("a did not add the ended task's bit")
	}
	a = press(t, a, runes("e"))
	if !strings.Contains(a.capsAuditModal.View(), "notify") {
		t.Fatal("e did not add the unheld bits")
	}
}

func TestCapsAuditModalScopeNone(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"))
	// expand every row
	for i := 0; i < 6; i++ {
		a = press(t, a, enter, down)
	}
	if !strings.Contains(a.capsAuditModal.View(), "global（固定）") {
		t.Fatalf("board_observe holder lacks the unbounded-scope mark:\n%s", a.capsAuditModal.View())
	}
}

// surface-parity item 32: the frame's width comes from the terminal, so
// expanding a bit (longer holder lines) neither moves the frame nor wraps.
func TestCapsAuditModalFrameIsFixed(t *testing.T) {
	a, _ := auditApp(t)
	a.width = 90
	a = press(t, a, runes("i"))
	before := lipgloss.Width(a.capsAuditModal.View())
	a = press(t, a, enter)
	after := a.capsAuditModal.View()
	if w := lipgloss.Width(after); w != before || w != 90 {
		t.Fatalf("frame width %d before expanding, %d after; want both 90", before, w)
	}
	if h := lipgloss.Height(after); h > a.height {
		t.Fatalf("modal is %d lines on a %d-line terminal", h, a.height)
	}
}

// surface-parity item 5: a fast `jj` arrives as ONE KeyRunes message.
func TestCapsAuditModalBatchedRunes(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), runes("jj"))
	if got := a.capsAuditModal.cursor; got != 2 {
		t.Fatalf("cursor after a batched jj = %d, want 2", got)
	}
}

// The task table keeps the 100 newest rows; the audit must not. An old
// supervisor still holding spawn is the case the feature exists for.
func TestCapsAuditModalSeesTasksPastTheTable(t *testing.T) {
	a, _ := auditApp(t)
	var old protocol.TaskID
	old.Id[0] = 0x01
	setAuditTasks(a, protocol.TaskInfo{Id: old, Status: protocol.TaskStatus_Running, CreatedAt: 1, Capabilities: protocol.Capability_Notify})
	for i := 0; i < 120; i++ {
		var id protocol.TaskID
		id.Id[0], id.Id[1] = 0x99, byte(i)
		setAuditTasks(a, protocol.TaskInfo{Id: id, Status: protocol.TaskStatus_Succeeded, CreatedAt: uint64(1000 + i)})
	}
	a = press(t, a, runes("i"))
	if !strings.Contains(a.capsAuditModal.View(), "01000000") {
		t.Fatalf("the oldest task's notify is missing from the audit:\n%s", a.capsAuditModal.View())
	}
}

// A refresh that removes a line above the cursor must not move enter onto a
// different task.
func TestCapsAuditModalCursorFollowsItsHolder(t *testing.T) {
	a, _ := auditApp(t)
	var second protocol.TaskID
	second.Id[0] = 0xb0
	setAuditTasks(a, protocol.TaskInfo{Id: second, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_Spawn})
	a = press(t, a, runes("i"), enter, down, down) // spawn ▾, ab…, b0…
	sp := a.tasksByID[FormatTaskID(protocol.TaskID{Id: [16]byte{0xab}})]
	sp.Status = protocol.TaskStatus_Succeeded // ab ends: its line above the cursor goes
	setAuditTasks(a, sp)
	a = press(t, a, enter)
	want := hex.EncodeToString(second.Id[:])
	if a.capsAuditModal.IsOpen() || a.tasks.SelectedID() != want {
		t.Fatalf("enter showed %q, want %s (the holder the cursor was on)", a.tasks.SelectedID(), want)
	}
}

// --cap on a bit nobody holds still shows that bit, as the CLI does.
func TestCapsAuditCmdlineFocusOnAnUnheldBit(t *testing.T) {
	a, _ := auditApp(t)
	tuiVerbs{a}.CapsAudit(verb.CapsAuditAction{Cap: "notify"})
	if v := a.capsAuditModal.View(); !strings.Contains(v, "notify") {
		t.Fatalf("caps audit --cap notify shows no notify row:\n%s", v)
	}
}

// When the holder under the cursor itself goes, the cursor falls back to its
// capability's line, never onto a neighbouring holder.
func TestCapsAuditModalCursorOnAGoneHolder(t *testing.T) {
	a, _ := auditApp(t)
	var second protocol.TaskID
	second.Id[0] = 0xb0
	setAuditTasks(a, protocol.TaskInfo{Id: second, Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_Spawn})
	a = press(t, a, runes("i"), enter, down) // spawn ▾, cursor on ab…
	sp := a.tasksByID[FormatTaskID(protocol.TaskID{Id: [16]byte{0xab}})]
	sp.Status = protocol.TaskStatus_Succeeded
	setAuditTasks(a, sp)
	a = press(t, a, enter)
	if !a.capsAuditModal.IsOpen() {
		t.Fatalf("enter jumped to %s after the cursor's holder ended", a.tasks.SelectedID())
	}
}

// When the cursor's whole capability folds away, it lands on a capability
// line, never on another bit's holder.
func TestCapsAuditModalCursorOnAFoldedBit(t *testing.T) {
	a, _ := auditApp(t)
	// rows: spawn, exec_cowrite, board_observe. Expand exec_cowrite (ab) and
	// board_observe (cd), sit on cd, then end cd: board_observe folds.
	a = press(t, a, runes("i"), down, enter, down, down, enter, down)
	cd := a.tasksByID[FormatTaskID(protocol.TaskID{Id: [16]byte{0xcd}})]
	cd.Status = protocol.TaskStatus_Succeeded
	setAuditTasks(a, cd)
	a = press(t, a, enter)
	if !a.capsAuditModal.IsOpen() {
		t.Fatalf("enter jumped to %s after the cursor's bit folded away", a.tasks.SelectedID())
	}
}

// D16/D20: enter on a holder shows it in the tree instead of opening re-grant.
func TestCapsAuditEnterJumpsToTree(t *testing.T) {
	a, want := auditApp(t)
	a.setFocus(focusLogs)
	a = press(t, a, runes("i"), enter, down, enter)
	if a.capsAuditModal.IsOpen() || a.authorityPicker.IsOpen() {
		t.Fatalf("modal open=%v picker open=%v; want both closed", a.capsAuditModal.IsOpen(), a.authorityPicker.IsOpen())
	}
	if !a.tasks.TreeMode() || a.tasks.SelectedID() != want || a.focus != focusTasks {
		t.Fatalf("tree=%v selected=%q focus=%v; want tree order on %s with the tasks pane focused",
			a.tasks.TreeMode(), a.tasks.SelectedID(), a.focus, want)
	}
}

// oldTasks adds an old Notify holder created by an old parent, then 120 newer
// ended tasks, so both old ones fall outside the table's 100 rows.
func oldTasks(a *App, parentPresent bool) (holder, parent protocol.TaskID) {
	holder.Id[0], parent.Id[0] = 0x01, 0x02
	h := protocol.TaskInfo{Id: holder, CreatorTaskId: parent, Status: protocol.TaskStatus_Running, CreatedAt: 2, Capabilities: protocol.Capability_Notify}
	setAuditTasks(a, h)
	if parentPresent {
		setAuditTasks(a, protocol.TaskInfo{Id: parent, Status: protocol.TaskStatus_Running, CreatedAt: 1})
	}
	for i := 0; i < 120; i++ {
		var id protocol.TaskID
		id.Id[0], id.Id[1] = 0x99, byte(i)
		setAuditTasks(a, protocol.TaskInfo{Id: id, Status: protocol.TaskStatus_Succeeded, CreatedAt: uint64(1000 + i)})
	}
	return holder, parent
}

func idCell(a *App, id string) (string, bool) {
	for i, r := range a.tasks.table.Rows() {
		if a.tasks.rowIDs[i] == id {
			return r[1], true
		}
	}
	return "", false
}

func TestCapsAuditJumpReachesAnOldTask(t *testing.T) {
	a, _ := auditApp(t)
	holder, _ := oldTasks(a, true)
	a.jumpToTask(FormatTaskID(holder))
	if got := a.tasks.SelectedID(); got != FormatTaskID(holder) {
		t.Fatalf("selected %q, want the old holder", got)
	}
	cell, _ := idCell(a, FormatTaskID(holder))
	if strings.Contains(cell, "\u2020") || !strings.Contains(cell, "─") {
		t.Fatalf("old holder's ID cell %q: want it under its creator, not an orphan", cell)
	}
}

// Review Focus 2.
func TestCapsAuditJumpChainStopsAtAMissingCreator(t *testing.T) {
	a, _ := auditApp(t)
	holder, _ := oldTasks(a, false)
	a.jumpToTask(FormatTaskID(holder))
	if got := a.tasks.SelectedID(); got != FormatTaskID(holder) {
		t.Fatalf("selected %q, want the old holder", got)
	}
	if cell, _ := idCell(a, FormatTaskID(holder)); !strings.Contains(cell, "\u2020") {
		t.Fatalf("ID cell %q: a holder whose creator is gone should show as an orphan", cell)
	}
}

func markedCells(a *App) int {
	n := 0
	for _, r := range a.tasks.table.Rows() {
		if strings.Contains(r[1], "◆") {
			n++
		}
	}
	return n
}

// D18-1: t on a bit marks exactly its holders, ended ones included (D22).
func TestCapsAuditTMarksHolders(t *testing.T) {
	a, _ := auditApp(t)
	var ended protocol.TaskID
	ended.Id[0] = 0xe0
	setAuditTasks(a, protocol.TaskInfo{Id: ended, Status: protocol.TaskStatus_Failed, Capabilities: protocol.Capability_Spawn})
	a = press(t, a, runes("i"), runes("t")) // cursor on spawn
	if a.capsAuditModal.IsOpen() || !a.tasks.TreeMode() {
		t.Fatalf("modal open=%v tree=%v after t", a.capsAuditModal.IsOpen(), a.tasks.TreeMode())
	}
	if title := a.tasks.table.Columns()[1].Title; !strings.Contains(title, "◆ spawn") {
		t.Fatalf("ID column title %q does not name the mark", title)
	}
	if n := markedCells(a); n != 2 {
		t.Fatalf("%d cells marked, want 2 (ab, and the ended e0)", n)
	}
	a = press(t, a, runes("T"))
	if n := markedCells(a); n != 0 {
		t.Fatalf("T left %d cells marked", n)
	}
}

// Review Focus 3.
func TestCapsAuditTOnAnUnheldBit(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), runes("e"))
	for i := 0; i < 40 && !strings.HasPrefix(stripCursorLine(a), "notify"); i++ {
		a = press(t, a, down)
	}
	a = press(t, a, runes("t"))
	if title := a.tasks.table.Columns()[1].Title; !strings.Contains(title, "◆ notify") {
		t.Fatalf("ID column title %q", title)
	}
	if n := markedCells(a); n != 0 {
		t.Fatalf("%d cells marked for a bit nobody holds", n)
	}
}

func stripCursorLine(a *App) string {
	c, _ := a.capsAuditModal.SelectedCap()
	return c.String()
}

func TestCapsAuditTMarksAnOldHolder(t *testing.T) {
	a, _ := auditApp(t)
	holder, _ := oldTasks(a, true)
	a.markBit(protocol.Capability_Notify)
	if cell, ok := idCell(a, FormatTaskID(holder)); !ok || !strings.Contains(cell, "◆") {
		t.Fatalf("old notify holder: listed=%v cell=%q; want it pinned and marked", ok, cell)
	}
}

// A jump must not drop the rows a still-active mark pinned: the header keeps
// claiming ◆ notify, so its old holder has to stay listed.
func TestCapsAuditJumpKeepsTheMarkedHolders(t *testing.T) {
	a, sp := auditApp(t)
	holder, _ := oldTasks(a, true)
	a.markBit(protocol.Capability_Notify)
	a.jumpToTask(sp)
	if cell, ok := idCell(a, FormatTaskID(holder)); !ok || !strings.Contains(cell, "◆") {
		t.Fatalf("after a jump the old notify holder is listed=%v cell=%q; want it still pinned and marked", ok, cell)
	}
}

func TestCapsAuditModalShowsAuthority(t *testing.T) {
	a, _ := auditApp(t)
	a = press(t, a, runes("i"), enter)
	if !strings.Contains(a.capsAuditModal.View(), "auth=unknown") {
		t.Fatalf("holder line lacks auth=:\n%s", a.capsAuditModal.View())
	}
}
