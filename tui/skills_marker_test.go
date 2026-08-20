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
	if body := formatTaskDetail(solo); !strings.Contains(body, "attached:      control\n") {
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
