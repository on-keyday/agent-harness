package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func taskWithSkills(profile string, injected bool) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = 0x11
	t.Status = protocol.TaskStatus_Running
	t.SetRepoPath([]uint8("/repo"))
	t.SetAgentProfile([]uint8(profile))
	t.SetSkillsInjected(injected)
	return t
}

func TestTaskAgentCell(t *testing.T) {
	cases := []struct {
		profile  string
		injected bool
		want     string
	}{
		{"claude", true, "claude+skills"},
		{"claude", false, "claude"},
		{"codex", true, "codex+skills"},
		// No profile: the caller supplies its own placeholder ("-" in the task
		// table, a blank slot in the authority picker), so this must NOT become
		// agentDescriptor's "?" for an unknown binary.
		{"", true, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := taskAgentCell(taskWithSkills(c.profile, c.injected)); got != c.want {
			t.Errorf("taskAgentCell(%q,%v)=%q want %q", c.profile, c.injected, got, c.want)
		}
	}
}

// The Agent column took its cell from the task's own profile without the
// marker, so the whole column lost it once every task carried a profile.
func TestTasksTableAgentColumnCarriesMarker(t *testing.T) {
	var m TasksModel = NewTasks()
	m.SetRows([]protocol.TaskInfo{taskWithSkills("claude", true)}, nil)
	rows := m.table.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// Column order: Status, ID, From, Agent, Act, Repo, Prompt.
	if rows[0][3] != "claude+skills" {
		t.Errorf("Agent column = %q, want claude+skills", rows[0][3])
	}
}

// A confined caller is served no runners at all, so the column must not depend
// on the runner slice being populated.
func TestTasksTableAgentColumnWithoutRunners(t *testing.T) {
	var m TasksModel = NewTasks()
	m.SetRows([]protocol.TaskInfo{taskWithSkills("codex", true)}, []protocol.RunnerInfo{})
	if got := m.table.Rows()[0][3]; got != "codex+skills" {
		t.Errorf("Agent column = %q, want codex+skills without any runner rows", got)
	}
}

// The detail popup spells the bit out instead of relying on the suffix: the
// table cannot distinguish "runner declares no injection" from "no runner has
// been assigned yet", and that difference is what a detail view is for.
func TestTaskDetailSpellsOutSkills(t *testing.T) {
	injected := taskWithSkills("claude", true)
	injected.StartedAt = 1
	if body := formatTaskDetail(injected); !strings.Contains(body, "skills:        injected") {
		t.Errorf("detail popup omits the injected state:\n%s", body)
	}

	assignedBare := taskWithSkills("bash", false)
	assignedBare.StartedAt = 1
	if body := formatTaskDetail(assignedBare); !strings.Contains(body, "not declared by the assigned runner") {
		t.Errorf("detail popup omits the not-declared state:\n%s", body)
	}

	// Never started: there is no runner to describe, so "not declared" would be
	// a claim about a runner that was never chosen.
	queued := taskWithSkills("claude", false)
	if body := formatTaskDetail(queued); !strings.Contains(body, "unknown (not assigned to a runner yet)") {
		t.Errorf("detail popup reports an unassigned task as if a runner had answered:\n%s", body)
	}
}

// --- observer counts in the detail popup ---

// The TUI task table has no Obs column (see tasks.go: an eighth column breaks
// the 80-cell frame), so the detail popup is the TUI's only surface for this.
// It must therefore be unambiguous where the table cannot be.
func TestTaskDetailSpellsOutWhoIsAttached(t *testing.T) {
	// Detached with observers: the case the counts exist for.
	watched := taskWithSkills("claude", true)
	watched.Status = protocol.TaskStatus_Detached
	watched.Viewers = 2
	watched.Cowriters = 1
	body := formatTaskDetail(watched)
	if !strings.Contains(body, "attached:      no control, 1 cowrite, 2 viewer") {
		t.Errorf("detail popup does not say who is on a Detached session:\n%s", body)
	}

	// Control attached, nobody spectating.
	solo := taskWithSkills("claude", true)
	solo.Status = protocol.TaskStatus_Running
	solo.SetIsAttached(true)
	if body := formatTaskDetail(solo); !strings.Contains(body, "attached:      control, 0 cowrite, 0 viewer\n") {
		t.Errorf("detail popup mis-words a plain control attach:\n%s", body)
	}

	// A terminal task has no session, so the line is absent rather than a
	// hollow "no control" about a session that does not exist.
	done := taskWithSkills("claude", true)
	done.Status = protocol.TaskStatus_Succeeded
	if body := formatTaskDetail(done); strings.Contains(body, "attached:") {
		t.Errorf("detail popup describes attachment on a finished task:\n%s", body)
	}
}

// --- the Obs column and its width-conditional column set ---

func liveTask(id byte, cowriters, viewers uint16) protocol.TaskInfo {
	t := taskWithSkills("claude", true)
	t.Id.Id[0] = id
	t.Status = protocol.TaskStatus_Running
	t.Cowriters = cowriters
	t.Viewers = viewers
	return t
}

// Resizing across the threshold changes the column COUNT, and bubbles renders
// on every SetRows/SetColumns — so a moment with N columns and N-1-cell rows
// panics inside renderRow. This walks both directions with rows present, which
// is exactly the sequence that paniced before applyColumns existed.
func TestTasksTableSurvivesResizeAcrossObsThreshold(t *testing.T) {
	var m TasksModel = NewTasks()
	m.SetRows([]protocol.TaskInfo{liveTask(1, 1, 2), liveTask(2, 0, 0)}, nil)
	for _, w := range []int{40, 80, 40, 120, 59, 60, 200, 20} {
		m.SetSize(w, 10)
		for i, row := range m.table.Rows() {
			if len(row) != len(m.baseCols) {
				t.Fatalf("panel=%d row %d has %d cells for %d columns", w, i, len(row), len(m.baseCols))
			}
		}
	}
	// And with the tree toggled, which swaps the column set independently.
	for _, w := range []int{200, 40} {
		m.SetSize(w, 10)
		for _, tree := range []bool{true, false, true} {
			m.SetTree(tree)
			for i, row := range m.table.Rows() {
				if len(row) != len(m.baseCols) {
					t.Fatalf("panel=%d tree=%v row %d has %d cells for %d columns", w, tree, i, len(row), len(m.baseCols))
				}
			}
		}
	}
}

// Above the threshold the column exists and carries the counts; below it the
// column is gone entirely rather than squeezed to noise.
func TestObsColumnAppearsOnlyWhenThePanelAffordsIt(t *testing.T) {
	var m TasksModel = NewTasks()
	m.SetRows([]protocol.TaskInfo{liveTask(1, 1, 2)}, nil)

	m.SetSize(120, 10)
	if !m.showsObs() {
		t.Fatal("a 120-cell panel must afford the Obs column")
	}
	titles := make([]string, len(m.baseCols))
	for i, c := range m.baseCols {
		titles[i] = c.Title
	}
	if titles[5] != "Obs" {
		t.Errorf("Obs must sit next to Act, before Repo; columns = %v", titles)
	}
	if got := m.table.Rows()[0][5]; got != "1c 2v" {
		t.Errorf("Obs cell = %q, want %q", got, "1c 2v")
	}

	m.SetSize(40, 10)
	if m.showsObs() {
		t.Fatal("a 40-cell panel must drop the Obs column")
	}
	for _, c := range m.baseCols {
		if c.Title == "Obs" {
			t.Error("Obs column survived below the threshold — it must be dropped, not squeezed")
		}
	}
}

// A live session nobody is on renders 0c 0v, not a blank cell: blank is
// reserved for "this task has no session", which the Status column names.
func TestObsCellDistinguishesEmptyFromNoSession(t *testing.T) {
	live := liveTask(1, 0, 0)
	if got := observerCell(live); got != "0c 0v" {
		t.Errorf("live session with nobody on it = %q, want %q", got, "0c 0v")
	}
	done := liveTask(2, 0, 0)
	done.Status = protocol.TaskStatus_Succeeded
	if got := observerCell(done); got != "" {
		t.Errorf("finished task = %q, want blank (it has no session to describe)", got)
	}
}

// A negative cursor renders one row short — bubbles computes the viewport
// window as cursor+height, so at -1 the last visible slot is blank until the
// first keypress. Two routes get there and both must be neutralised.
func TestTasksTableCursorNeverStaysNegative(t *testing.T) {
	rows := []protocol.TaskInfo{liveTask(1, 0, 0), liveTask(2, 0, 0), liveTask(3, 0, 0)}

	// Route 1 (certain): the first SetSize runs before any task arrives, and
	// applyColumns empties the rows to swap the columns safely.
	var m TasksModel = NewTasks()
	m.SetSize(120, 10)
	m.SetRows(rows, nil)
	if got := m.table.Cursor(); got < 0 {
		t.Errorf("cursor = %d after the startup resize-then-rows order; a negative cursor renders one row short", got)
	}

	// Route 2 (latent, predates applyColumns): the task list going empty at
	// any point — a fresh server, everything pruned — and coming back.
	var m2 TasksModel = NewTasks()
	m2.SetSize(120, 10)
	m2.SetRows(rows, nil)
	m2.SetRows(nil, nil)
	m2.SetRows(rows, nil)
	if got := m2.table.Cursor(); got < 0 {
		t.Errorf("cursor = %d after the list emptied and refilled", got)
	}

	// An empty table legitimately has no selection; do not invent one.
	var m3 TasksModel = NewTasks()
	m3.SetSize(120, 10)
	m3.SetRows(nil, nil)
	if m3.SelectedID() != "" {
		t.Error("an empty table reported a selection")
	}
}
