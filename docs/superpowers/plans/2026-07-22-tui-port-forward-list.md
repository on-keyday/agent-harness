# TUI Active Port-Forward List Modal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only full-screen TUI overlay (opened with `f`, closed with `Esc`) that lists every port forward this TUI currently holds.

**Architecture:** Pure presentation over existing state. `App.activeForwards` is already the single source of truth for live forwards (added on `PortForwardStartedMsg`, removed on `PortForwardStoppedMsg`). A new `ForwardsModal` (mirroring the existing `ConnsModal`) renders a sorted snapshot fed to it by the App. No new App state, no new wire messages, no server changes.

**Tech Stack:** Go, bubbletea (`tea`), bubbles `table`, lipgloss. Package `tui`.

## Global Constraints

- Package under change: `tui` (`tui/portforward.go`, `tui/app.go`, `tui/portforward_test.go`).
- Follow existing modal pattern: `tui/conns.go` (`ConnsModal`) is the reference. Reuse `HeaderStyle`, `FooterStyle`, `PanelStyleFocused`, `pfShortID`, `ForwardDirection.flag()` — all already in package `tui`.
- TUI-only. `activeForwards` is process-local client state; no CLI/WebUI equivalent is built.
- Read-only modal: stopping forwards stays on the tasks pane's `P`/`B` keys.
- Verify with `go test ./tui/...` (not ad-hoc). Build check with `go build ./tui/...`.
- Frequent commits: one per task.

---

### Task 1: `ForwardsModal` type + `sortedForwards` helper (self-contained, unit-tested)

**Files:**
- Modify: `tui/portforward.go` (append new type + helper; add one import)
- Test: `tui/portforward_test.go` (append tests)

**Interfaces:**
- Consumes: existing `PortForwardSession` struct (`ID int`, `TaskID string`, `Direction ForwardDirection`, `Spec string`, `Cancel context.CancelFunc`), `ForwardDirection.flag() string`, `pfShortID(string) string` — all already in `tui/portforward.go`.
- Produces (used by Task 2):
  - `sortedForwards(m map[int]*PortForwardSession) []*PortForwardSession`
  - `type ForwardsModal struct { ... sessions []*PortForwardSession ... }`
  - `NewForwardsModal() ForwardsModal`
  - `(*ForwardsModal) IsOpen() bool`, `Open()`, `Close()`, `SetSize(w, h int)`, `SetSessions([]*PortForwardSession)`
  - `(ForwardsModal) Update(tea.Msg) (ForwardsModal, tea.Cmd)`, `View() string`
  - `forwardRow(*PortForwardSession) table.Row`

- [ ] **Step 1: Write the failing tests**

Append to `tui/portforward_test.go`. Note the existing file's import is only `import "testing"` — replace it with the grouped import block shown, and add the `table` import used by the row test.

```go
import (
	"testing"

	"github.com/charmbracelet/bubbles/table"
)

func TestSortedForwards_Order(t *testing.T) {
	// ForwardLocal=0 < ForwardRemote=1, so within a task -L sorts before -R.
	m := map[int]*PortForwardSession{
		3: {ID: 3, TaskID: "b", Direction: ForwardLocal, Spec: "7:h:7"},
		1: {ID: 1, TaskID: "a", Direction: ForwardRemote, Spec: "1:h:2"},
		2: {ID: 2, TaskID: "a", Direction: ForwardLocal, Spec: "8080:h:80"},
		4: {ID: 4, TaskID: "a", Direction: ForwardLocal, Spec: "9090:h:90"},
	}
	got := sortedForwards(m)
	want := []int{2, 4, 1, 3} // a/-L/2, a/-L/4, a/-R/1, b/-L/3
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("pos %d: got ID %d, want %d", i, got[i].ID, id)
		}
	}
}

func TestForwardRow_Cells(t *testing.T) {
	s := &PortForwardSession{ID: 1, TaskID: "abcdef012345aa", Direction: ForwardLocal, Spec: "8080:h:80"}
	row := forwardRow(s)
	if row[0] != "abcdef012345" { // pfShortID truncates to 12
		t.Fatalf("task cell = %q, want abcdef012345", row[0])
	}
	if row[1] != "-L" {
		t.Fatalf("dir cell = %q, want -L", row[1])
	}
	if row[2] != "8080:h:80" {
		t.Fatalf("spec cell = %q, want 8080:h:80", row[2])
	}
	r := forwardRow(&PortForwardSession{TaskID: "x", Direction: ForwardRemote, Spec: "1:h:2"})
	if r[1] != "-R" {
		t.Fatalf("remote dir cell = %q, want -R", r[1])
	}
}

func TestForwardsModal_OpenClose(t *testing.T) {
	m := NewForwardsModal()
	if m.IsOpen() {
		t.Fatal("new modal should be closed")
	}
	m.Open()
	if !m.IsOpen() {
		t.Fatal("after Open: should be open")
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("after Close: should be closed")
	}
}

func TestForwardsModal_SetSessions_CountAndEmpty(t *testing.T) {
	m := NewForwardsModal()
	m.SetSessions(nil)
	if len(m.sessions) != 0 {
		t.Fatalf("empty: sessions = %d, want 0", len(m.sessions))
	}
	m.SetSessions([]*PortForwardSession{
		{ID: 1, TaskID: "abcdef012345aa", Direction: ForwardLocal, Spec: "8080:h:80"},
		{ID: 2, TaskID: "abcdef012345aa", Direction: ForwardRemote, Spec: "9000:h:9000"},
	})
	if len(m.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(m.sessions))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./tui/ -run 'TestSortedForwards_Order|TestForwardRow_Cells|TestForwardsModal_OpenClose|TestForwardsModal_SetSessions_CountAndEmpty' -v`
Expected: compile FAIL — `undefined: sortedForwards`, `undefined: forwardRow`, `undefined: NewForwardsModal`.

- [ ] **Step 3: Implement the type + helpers**

Append to `tui/portforward.go`. Add `"github.com/charmbracelet/bubbles/table"` to the existing import block (which already has `context`, `fmt`, `sort`, `textinput`, `tea`, `lipgloss`).

```go
// sortedForwards returns all active forwards across every task, ordered by
// (TaskID, Direction, ID) for a stable list. Unlike selectForwards it does not
// filter by task/direction — the forwards modal shows the whole set.
func sortedForwards(m map[int]*PortForwardSession) []*PortForwardSession {
	out := make([]*PortForwardSession, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// forwardRow maps one session to its table row (task short id / dir flag / spec).
func forwardRow(s *PortForwardSession) table.Row {
	return table.Row{pfShortID(s.TaskID), s.Direction.flag(), s.Spec}
}

// ForwardsModal is a read-only full-screen overlay listing every port forward
// this TUI currently holds (App.activeForwards). Opened with `f`, closed with
// Esc. It mirrors ConnsModal but has no live subscription of its own: the App
// feeds it a pre-sorted slice via SetSessions on open and whenever the active
// set changes while it is open. It keeps that snapshot (sessions) so its
// contents are inspectable and the header count is exact.
type ForwardsModal struct {
	open     bool
	table    table.Model
	sessions []*PortForwardSession
}

// NewForwardsModal constructs a ForwardsModal with fixed column widths.
func NewForwardsModal() ForwardsModal {
	cols := []table.Column{
		{Title: "task", Width: 12},
		{Title: "dir", Width: 4},
		{Title: "spec", Width: 40},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return ForwardsModal{table: t}
}

func (m *ForwardsModal) IsOpen() bool { return m.open }
func (m *ForwardsModal) Open()        { m.open = true }
func (m *ForwardsModal) Close()       { m.open = false }

// SetSize propagates terminal dimensions into the table (full-screen overlay).
// Reserve 4 rows for border + header + footer (as ConnsModal.SetSize).
func (m *ForwardsModal) SetSize(w, h int) {
	m.table.SetWidth(w - 4)
	m.table.SetHeight(h - 4)
}

// SetSessions replaces the snapshot and rebuilds the table rows.
func (m *ForwardsModal) SetSessions(sessions []*PortForwardSession) {
	m.sessions = sessions
	rows := make([]table.Row, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, forwardRow(s))
	}
	m.table.SetRows(rows)
}

func (m ForwardsModal) Update(msg tea.Msg) (ForwardsModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m ForwardsModal) View() string {
	header := HeaderStyle.Render(fmt.Sprintf("active port forwards (%d)", len(m.sessions)))
	footer := FooterStyle.Render("Esc: close · P/B stop from tasks pane")
	box := PanelStyleFocused.Padding(0, 1)
	if len(m.sessions) == 0 {
		return box.Render(header + "\n" + "no active forwards" + "\n" + footer)
	}
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./tui/ -run 'TestSortedForwards_Order|TestForwardRow_Cells|TestForwardsModal_OpenClose|TestForwardsModal_SetSessions_CountAndEmpty' -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Full package build + test**

Run: `go build ./tui/... && go test ./tui/`
Expected: build OK, all `tui` tests PASS.

- [ ] **Step 6: Commit**

```bash
git add tui/portforward.go tui/portforward_test.go
git commit -m "feat(tui): ForwardsModal + sortedForwards helper for port-forward list"
```

---

### Task 2: Wire `ForwardsModal` into the App (`f` key, Esc/render/resize/live-update)

**Files:**
- Modify: `tui/app.go` (struct field, initializer, key handler, open-modal interception, resize, live update, View overlay)
- Test: `tui/portforward_test.go` (append App-level integration test)

**Interfaces:**
- Consumes (from Task 1): `NewForwardsModal()`, `ForwardsModal` methods, `sortedForwards(...)`.
- Produces: new `App` field `forwardsModal ForwardsModal`, wired to the `f` key.

- [ ] **Step 1: Write the failing integration test**

Append to `tui/portforward_test.go`. Add `tea "github.com/charmbracelet/bubbletea"` to the test import block created in Task 1.

```go
func TestForwardsModal_KeyOpensAndEscCloses(t *testing.T) {
	a := New(Config{})
	// Seed one active forward (default focus is focusTasks, logs not editing,
	// so the `f` guard passes).
	m, _ := a.Update(PortForwardStartedMsg{ID: 1, TaskID: "abcdef012345", Direction: ForwardLocal, Spec: "8080:h:80"})
	a = m.(*App)

	// Press `f` → modal opens, seeded with the active forwards snapshot.
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	a = m.(*App)
	if !a.forwardsModal.IsOpen() {
		t.Fatal("`f` should open the forwards modal")
	}
	if len(a.forwardsModal.sessions) != 1 {
		t.Fatalf("modal sessions = %d, want 1", len(a.forwardsModal.sessions))
	}

	// A forward that stops while the modal is open updates the snapshot live.
	m, _ = a.Update(PortForwardStoppedMsg{ID: 1, TaskID: "abcdef012345"})
	a = m.(*App)
	if len(a.forwardsModal.sessions) != 0 {
		t.Fatalf("after stop while open: sessions = %d, want 0", len(a.forwardsModal.sessions))
	}

	// Esc closes.
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.forwardsModal.IsOpen() {
		t.Fatal("Esc should close the forwards modal")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestForwardsModal_KeyOpensAndEscCloses -v`
Expected: compile FAIL — `a.forwardsModal undefined (type *App has no field or method forwardsModal)`.

- [ ] **Step 3a: Add the struct field**

In `tui/app.go`, immediately after the `connsModal ConnsModal` field (`tui/app.go:75`), add:

```go
	forwardsModal ForwardsModal
```

- [ ] **Step 3b: Initialize it**

In `New(...)` (`tui/app.go`), in the struct literal after `connsModal: NewConnsModal(),` (`tui/app.go:185`), add:

```go
		forwardsModal:   NewForwardsModal(),
```

- [ ] **Step 3c: Handle resize**

In the `tea.WindowSizeMsg` case, after `a.connsModal.SetSize(a.width, a.height)` (`tui/app.go:676`), add:

```go
		a.forwardsModal.SetSize(a.width, a.height)
```

- [ ] **Step 3d: Intercept keys while the modal is open**

In the `tea.KeyMsg` case, immediately after the Connections-modal interception block (the block that ends at `tui/app.go:700`), add:

```go
		// Forwards list modal: Esc closes; arrow keys scroll the table; all
		// other keys are swallowed so they don't leak through to the panels.
		if a.forwardsModal.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.forwardsModal.Close()
				return a, nil
			}
			var cmd tea.Cmd
			a.forwardsModal, cmd = a.forwardsModal.Update(msg)
			return a, cmd
		}
```

- [ ] **Step 3e: Open on `f`**

In the `tea.KeyMsg` case, immediately after the `C` (Connections open) block (which ends at `tui/app.go:910`), add:

```go
		// `f` opens the active port-forward list: a read-only full-screen
		// overlay of every forward this TUI currently holds (App.activeForwards).
		// Esc closes. Stopping stays on the tasks pane's P/B keys.
		if a.focus != focusCmdline && !logsEditing && msg.String() == "f" {
			a.forwardsModal.SetSessions(sortedForwards(a.activeForwards))
			a.forwardsModal.SetSize(a.width, a.height)
			a.forwardsModal.Open()
			return a, nil
		}
```

- [ ] **Step 3f: Live-update the snapshot on start/stop while open**

In the `PortForwardStartedMsg` case, before its `return a, nil` (`tui/app.go:654`), add:

```go
		if a.forwardsModal.IsOpen() {
			a.forwardsModal.SetSessions(sortedForwards(a.activeForwards))
		}
```

In the `PortForwardStoppedMsg` case, before its `return a, nil` (`tui/app.go:665`), add:

```go
		if a.forwardsModal.IsOpen() {
			a.forwardsModal.SetSessions(sortedForwards(a.activeForwards))
		}
```

- [ ] **Step 3g: Render the overlay**

In `App.View()` (the overlay chain), immediately after the `connsModal` block (`tui/app.go:1340-1342`), add:

```go
	if a.forwardsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.forwardsModal.View())
	}
```

- [ ] **Step 4: Run the integration test to verify it passes**

Run: `go test ./tui/ -run TestForwardsModal_KeyOpensAndEscCloses -v`
Expected: PASS.

- [ ] **Step 5: Full package build + test + vet**

Run: `go build ./tui/... && go vet ./tui/... && go test ./tui/`
Expected: build OK, vet clean, all `tui` tests PASS.

- [ ] **Step 6: Commit**

```bash
git add tui/app.go tui/portforward_test.go
git commit -m "feat(tui): 'f' opens read-only active port-forward list modal"
```

---

## Notes for the implementer

- Line numbers reference the pre-change file and drift as you edit. Anchor each insertion to the quoted neighboring code, not the raw line number.
- `HeaderStyle`, `FooterStyle`, `PanelStyleFocused` are defined in `tui/styles.go` and used by `tui/conns.go` — no new styles needed.
- Do not add a stop action inside the modal; that is deliberately out of scope (spec §"Out of scope"). Stopping remains on the tasks pane `P`/`B` keys, which the footer advertises.
- `activeForwards` is client-local; this feature is TUI-only by design — no CLI/WebUI counterpart.
