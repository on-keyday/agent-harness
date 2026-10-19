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

func TestCapsAuditModalHolderOpensRegrant(t *testing.T) {
	a, want := auditApp(t)
	a = press(t, a, runes("i"), enter, down, enter)
	if a.capsAuditModal.IsOpen() {
		t.Fatal("the modal stayed open after choosing a holder")
	}
	if !a.authorityPicker.IsOpen() || a.authorityPicker.Mode() != PickerModeRegrant || a.authorityPicker.TargetID() != want {
		t.Fatalf("picker open=%v target=%q, want re-grant on %s", a.authorityPicker.IsOpen(), a.authorityPicker.TargetID(), want)
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
	if !a.authorityPicker.IsOpen() || a.authorityPicker.TargetID() != want {
		t.Fatalf("enter re-granted %q, want %s (the holder the cursor was on)", a.authorityPicker.TargetID(), want)
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
	if a.authorityPicker.IsOpen() {
		t.Fatalf("enter opened re-grant on %s after the cursor's holder ended", a.authorityPicker.TargetID())
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
	if a.authorityPicker.IsOpen() {
		t.Fatalf("enter opened re-grant on %s after the cursor's bit folded away", a.authorityPicker.TargetID())
	}
}
