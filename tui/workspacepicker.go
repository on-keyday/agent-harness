package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// workspacePickerRow is one candidate task for a workspace.
//
// Forwards is what the registry currently reports for the task, already
// rendered as config values; the row shows the count so the operator can see
// what a save would write without opening the file.
type workspacePickerRow struct {
	IDHex    string
	Label    string
	Include  bool
	Resume   workspace.Resume
	Runner   workspace.Runner
	Forwards []string
	// Live marks a task with a session alive right now, as opposed to one that
	// is only here because the workspace already declares it.
	Live bool
}

// WorkspacePickerModel chooses WHICH tasks a `workspace save` records and with
// what resume / runner policy.
//
// It exists because neither automatic rule was right. Recording the task the
// logs pane happened to follow wrote one block for no stated reason; recording
// every live session is a better guess and still a guess — "detached right now"
// and "what I want brought back" are different sets, and the second one is the
// operator's to state. Editing the file by hand was the only way to say it.
//
// resume / runner are cycled per row rather than typed, for item 34a's reason:
// both are small closed enums, so a form should offer their states rather than
// a text box.
type WorkspacePickerModel struct {
	open bool
	name string
	rows []workspacePickerRow
	cur  int
	w, h int
}

func (m *WorkspacePickerModel) IsOpen() bool { return m.open }
func (m *WorkspacePickerModel) Close()       { m.open = false; m.rows = nil }
func (m *WorkspacePickerModel) Name() string { return m.name }

func (m *WorkspacePickerModel) SetSize(w, h int) { m.w, m.h = w, h }

// Open builds the candidate list for workspace `name`.
//
// Candidates are every live interactive session plus every task the named
// workspace already declares, so a task that has since finished is still
// listed — dropping it must be a decision, not a side effect of it not running
// at the moment you saved.
//
// Pre-selection: an existing workspace's own tasks if it has any (a save then
// defaults to "what I already said"), otherwise every live session.
func (m *WorkspacePickerModel) Open(name string, live []protocol.TaskInfo,
	existing *workspace.Workspace, forwards map[string][]string) {

	m.open, m.name, m.cur = true, name, 0
	m.rows = m.rows[:0]

	declared := map[string]workspace.Task{}
	var declaredOrder []string
	if existing != nil {
		for _, t := range existing.Tasks {
			declared[t.ID] = t
			declaredOrder = append(declaredOrder, t.ID)
		}
	}
	hadDeclared := len(declaredOrder) > 0

	seen := map[string]bool{}
	add := func(idHex, label string, isLive bool) {
		if seen[idHex] {
			return
		}
		seen[idHex] = true
		row := workspacePickerRow{
			IDHex: idHex, Label: label, Live: isLive,
			Resume: workspace.ResumeContinue, Runner: workspace.RunnerAssigned,
			Forwards: forwards[idHex],
		}
		if d, ok := declared[idHex]; ok {
			// An existing block's policy is the operator's; the picker starts
			// from it rather than from the defaults.
			row.Resume, row.Runner = d.Resume, d.Runner
			if row.Resume == "" {
				row.Resume = workspace.ResumeNo
			}
			if row.Runner == "" {
				row.Runner = workspace.RunnerAssigned
			}
			row.Include = true
		} else {
			row.Include = !hadDeclared && isLive
		}
		m.rows = append(m.rows, row)
	}

	for i := range live {
		t := live[i]
		add(FormatTaskID(t.Id), workspacePickerLabel(t), true)
	}
	for _, id := range declaredOrder {
		add(id, id[:8]+"  (not running)", false)
	}
	// Tasks that only have a forward, with no session and no declaration.
	var extra []string
	for id := range forwards {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		add(id, id[:8]+"  (forward only)", false)
	}
}

func workspacePickerLabel(t protocol.TaskInfo) string {
	label := FormatTaskID(t.Id)[:8] + "  " + taskStatusStr(t.Status) + "  " + taskAgentCell(t) +
		"  " + truncateLeft(string(t.RepoPath), 20)
	if p := strings.TrimSpace(string(t.Prompt)); p != "" {
		label += "  " + runewidth.Truncate(p, 20, "…")
	}
	return label
}

func (m *WorkspacePickerModel) Move(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cur = (m.cur + delta + len(m.rows)) % len(m.rows)
}

// Toggle includes or excludes the focused task.
func (m *WorkspacePickerModel) Toggle() {
	if len(m.rows) == 0 {
		return
	}
	m.rows[m.cur].Include = !m.rows[m.cur].Include
}

// CycleResume walks no → continue → fresh → no on the focused row. `fresh` is
// reachable here — it drops the agent's conversation, which is recoverable with
// /resume inside the agent, so it is a choice rather than a hazard.
func (m *WorkspacePickerModel) CycleResume() {
	if len(m.rows) == 0 {
		return
	}
	switch m.rows[m.cur].Resume {
	case workspace.ResumeNo:
		m.rows[m.cur].Resume = workspace.ResumeContinue
	case workspace.ResumeContinue:
		m.rows[m.cur].Resume = workspace.ResumeFresh
	default:
		m.rows[m.cur].Resume = workspace.ResumeNo
	}
}

// CycleRunner flips assigned ⇄ any on the focused row.
func (m *WorkspacePickerModel) CycleRunner() {
	if len(m.rows) == 0 {
		return
	}
	if m.rows[m.cur].Runner == workspace.RunnerAny {
		m.rows[m.cur].Runner = workspace.RunnerAssigned
	} else {
		m.rows[m.cur].Runner = workspace.RunnerAny
	}
}

// SetAll includes or excludes every row.
func (m *WorkspacePickerModel) SetAll(include bool) {
	for i := range m.rows {
		m.rows[i].Include = include
	}
}

// Result returns the chosen task blocks and the ids the save observed. The
// observed set is every row the picker LISTED, included or not: excluding a row
// is a statement about it, so its block must be dropped rather than preserved
// by the merge.
func (m *WorkspacePickerModel) Result() ([]workspace.Task, map[string]bool) {
	var tasks []workspace.Task
	observed := map[string]bool{}
	for _, r := range m.rows {
		observed[r.IDHex] = true
		if !r.Include {
			continue
		}
		tasks = append(tasks, workspace.Task{
			ID: r.IDHex, Resume: r.Resume, Runner: r.Runner, Forwards: r.Forwards,
		})
	}
	return tasks, observed
}

// ExcludedIDs are the listed-but-unticked tasks, whose blocks a save removes.
func (m *WorkspacePickerModel) ExcludedIDs() []string {
	var out []string
	for _, r := range m.rows {
		if !r.Include {
			out = append(out, r.IDHex)
		}
	}
	return out
}

func (m *WorkspacePickerModel) View() string {
	if !m.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)

	var b strings.Builder
	fmt.Fprintf(&b, "workspace %q — which tasks?\n\n", m.name)
	if len(m.rows) == 0 {
		b.WriteString("  (no live session, no declared task, no forward)\n")
	}
	for i, r := range m.rows {
		cursor := "  "
		if i == m.cur {
			cursor = "> "
		}
		mark := "[ ]"
		if r.Include {
			mark = "[x]"
		}
		fwd := ""
		switch n := len(r.Forwards); {
		case n == 1:
			fwd = "  1 forward"
		case n > 1:
			fwd = fmt.Sprintf("  %d forwards", n)
		}
		line := fmt.Sprintf("%s%s %-52s  resume:%-8s runner:%-8s%s",
			cursor, mark, r.Label, r.Resume, r.Runner, fwd)
		if i == m.cur {
			line = lipgloss.NewStyle().Foreground(colorFocused).Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + FooterStyle.Render(
		"↑↓/jk move · space include · r resume · u runner · a/n all/none · enter save · esc cancel"))
	return box.Render(b.String())
}
