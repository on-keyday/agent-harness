package tui

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// scrollHint names the keys that scroll any viewport-backed pane here. It
// deliberately does NOT mention PgUp/PgDn: those need an Fn combination on most
// laptops, printed in faint legend on the key caps, and a footer that advertises
// the one control the reader cannot find is worse than one that says nothing.
// bubbles' viewport binds all of these out of the box — the keys always worked,
// only the hint was pointing at the wrong ones.
//
// Shared by every pane that scrolls, so the wording cannot drift between them.
const scrollHint = "space/f,b: page · d/u: half · j/k: line"

// DetailPopup is a read-only popup that displays formatted details for a
// selected runner or task row, and the `?` key list. Long fields (full repo
// path, worktree dir, multi-line prompt) that the row table truncates are shown
// in full here — so the body is routinely taller than the terminal and the
// popup scrolls. It has no editable state: Esc closes, everything else scrolls.
type DetailPopup struct {
	open  bool
	title string
	body  string
	// vp bounds the body to the terminal. Without it the box rendered at its
	// content's full height and the terminal simply scrolled: `?` lists 30
	// bindings across five groups, so on the 24-row minimum the TUI itself
	// enforces, the top border, the title and the first groups were pushed off
	// the top with no way to get them back. Mirrors NotifyModel / CmdResultModel.
	vp viewport.Model
	// overflow records whether the body did not fit, so the footer only offers
	// scrolling when there is something to scroll to.
	overflow bool
}

func NewDetailPopup() DetailPopup {
	return DetailPopup{vp: viewport.New(80, 10)}
}

func (d *DetailPopup) IsOpen() bool { return d.open }

func (d *DetailPopup) Open(title, body string) {
	d.open = true
	d.title = title
	d.body = body
	d.vp.SetContent(body)
	d.vp.GotoTop()
}

func (d *DetailPopup) Close() {
	d.open = false
}

// SetSize fits the popup inside a w x h terminal. Called from View on every
// render rather than from layout, so the popup tracks a resize even while it is
// already open.
//
// The reserve accounts for what the box adds around the body: 2 border rows,
// 2 padding rows, the title, the blank line under it, the blank line above the
// footer, and the footer — 8 rows. Horizontally: 2 border + 4 padding.
func (d *DetailPopup) SetSize(w, h int) {
	const chromeRows, chromeCols = 8, 6
	inner := h - chromeRows
	if inner < 3 {
		inner = 3
	}
	width := w - chromeCols
	if width < 20 {
		width = 20
	}
	d.vp.Width = width
	d.vp.Height = inner
	d.vp.SetContent(d.body)
	d.overflow = lipgloss.Height(d.body) > inner
}

// Update handles scrolling while the popup is open. Esc is handled by the
// caller (it closes); everything else goes to the viewport, so the arrow keys
// and page keys behave the way they do in every other scrollable pane here.
func (d *DetailPopup) Update(msg tea.Msg) (DetailPopup, tea.Cmd) {
	var cmd tea.Cmd
	d.vp, cmd = d.vp.Update(msg)
	return *d, cmd
}

func (d *DetailPopup) View() string {
	if !d.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	header := HeaderStyle.Render(d.title)
	hint := "Esc: close"
	if d.overflow {
		hint = scrollHint + " · Esc: close"
	}
	footer := FooterStyle.Render(hint)
	return box.Render(header + "\n\n" + d.vp.View() + "\n\n" + footer)
}

// formatRunnerDetail renders a multi-line, label:value description of a
// RunnerInfo for the detail popup.
func formatRunnerDetail(r protocol.RunnerInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "status:        %s\n", runnerStatusStr(r.Status))
	fmt.Fprintf(&sb, "id:            %s\n", protocol.RunnerIDToConnID(r.Id).String())
	fmt.Fprintf(&sb, "host:          %s\n", string(r.Hostname))
	fmt.Fprintf(&sb, "agent:         %s\n", agentProfilesDescriptor(r.AgentProfiles, string(r.AgentBin), r.SkillsInjected()))
	fmt.Fprintf(&sb, "tasks:         %d active / %d max\n", r.ActiveTasksLen, r.MaxTasks)
	for i, root := range r.AllowedRoots {
		fmt.Fprintf(&sb, "root[%d]:       %s\n", i, string(root.Path))
	}
	if len(r.ActiveTasks) > 0 {
		for i, at := range r.ActiveTasks {
			fmt.Fprintf(&sb, "active[%d]:     %s  %s\n", i,
				hex.EncodeToString(at.TaskId.Id[:]),
				string(at.RepoPath))
		}
	}
	fmt.Fprintf(&sb, "connected:     %s\n", formatNanoTs(r.ConnectedAt))
	fmt.Fprintf(&sb, "last seen:     %s\n", formatNanoTs(r.LastSeen))
	return sb.String()
}

// formatTaskDetail renders a multi-line, label:value description of a
// TaskInfo for the detail popup. The prompt is shown in full at the bottom
// (it can be multi-line and is the most likely thing the user wants to
// inspect after the row's truncation).
func formatTaskDetail(t protocol.TaskInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "id:            %s\n", hex.EncodeToString(t.Id.Id[:]))
	fmt.Fprintf(&sb, "kind:          %s\n", taskKindStr(t.Kind))
	fmt.Fprintf(&sb, "status:        %s\n", taskStatusStr(t.Status))
	if len(t.AgentProfile) > 0 {
		fmt.Fprintf(&sb, "agent:         %s\n", string(t.AgentProfile))
	}
	// Spelled out rather than folded into the agent line's "+skills" suffix:
	// the table has no room to distinguish "the runner declares no injection"
	// from "no runner has been assigned yet", and a detail view is where that
	// difference belongs. Always printed — a hidden default reads as "this
	// task has no such property".
	fmt.Fprintf(&sb, "skills:        %s\n", skillsInjectedDetail(t))
	// Busy/idle badge + last-output timestamp for a live interactive session,
	// mirroring the task table's Act column (blank there for tasks without a
	// live session — the server leaves last_output_at at 0 for those).
	if t.LastOutputAt > 0 {
		fmt.Fprintf(&sb, "act:           %s\n", cli.ActivityStr(t.OutputIdleMs))
		fmt.Fprintf(&sb, "last output:   %s\n", formatNanoTs(t.LastOutputAt))
	}
	// Who is on this session, spelled out. Only a live session has observers at
	// all, so the line is gated on that rather than printed as a hollow "0" for
	// every finished task. Worth its own line because the status field cannot
	// carry it: Running/Detached tracks ONLY the control attach, so a task with
	// viewers reads Detached and looks abandoned.
	if taskSessionAlive(t.Status) {
		fmt.Fprintf(&sb, "attached:      %s\n", attachDetail(t))
	}
	fmt.Fprintf(&sb, "from:          %s\n", originCell(t.OriginKind))
	if t.CreatorTaskId.Id != ([16]byte{}) {
		fmt.Fprintf(&sb, "created by:    %s\n", hex.EncodeToString(t.CreatorTaskId.Id[:]))
	}
	if t.ResumedByKind != protocol.ClientKind_Unspecified {
		fmt.Fprintf(&sb, "resumed by:    %s\n", originCell(t.ResumedByKind))
	}
	fmt.Fprintf(&sb, "caps:          %s\n", cli.CapsLabel(t.Capabilities))
	fmt.Fprintf(&sb, "scope:         %s\n", cli.ScopeLabel(t.Scope))
	// Printed only when present: the scope is half a task's authority and is
	// never absent, while a per-capability narrowing that is not there has
	// nothing to report.
	if ov := cli.OverridesLabel(t.Overrides); ov != "" {
		fmt.Fprintf(&sb, "scope-for:     %s\n", ov)
	}
	fmt.Fprintf(&sb, "repo:          %s\n", string(t.RepoPath))
	if len(t.WorktreeDir) > 0 {
		fmt.Fprintf(&sb, "worktree:      %s\n", string(t.WorktreeDir))
	}
	// Always printed, zero included. Unlike the attach counts above there is no
	// "no subject" case to gate on: every task can be asked how many commands
	// are running in its tree, and none is a real answer to that. Eliding it
	// would make "nothing is running" read as "this popup does not report
	// execs" (surface-parity item 31).
	fmt.Fprintf(&sb, "execs:         %d running\n", t.ExecCount)
	fmt.Fprintf(&sb, "created:       %s\n", formatNanoTs(t.CreatedAt))
	if t.StartedAt > 0 {
		fmt.Fprintf(&sb, "started:       %s\n", formatNanoTs(t.StartedAt))
		fmt.Fprintf(&sb, "assigned to:   %s\n", protocol.RunnerIDToConnID(t.AssignedTo).String())
	}
	if t.EndedAt > 0 {
		fmt.Fprintf(&sb, "ended:         %s\n", formatNanoTs(t.EndedAt))
		fmt.Fprintf(&sb, "exit code:     %d\n", t.ExitCode)
		if len(t.ErrorMessage) > 0 {
			fmt.Fprintf(&sb, "error:         %s\n", string(t.ErrorMessage))
		}
	}
	if len(t.Prompt) > 0 {
		fmt.Fprintf(&sb, "\nprompt:\n%s\n", string(t.Prompt))
	}
	return sb.String()
}

// attachDetail words who is attached to a live session for the detail popup:
// the control slot (the only one that moves Running/Detached) plus the observer
// counts. "no control" is stated rather than left blank, because that IS what
// Detached means and a reader looking for the difference should find it here.
func attachDetail(t protocol.TaskInfo) string {
	control := "no control"
	if t.IsAttached() {
		control = "control"
	}
	// Counts always printed, zeros included. They were elided at zero, which
	// made "nobody is spectating" and "this popup does not report spectators"
	// read the same — in a DETAIL view, which is precisely where a default must
	// not hide (surface-parity item 31).
	return fmt.Sprintf("%s, %d cowrite, %d viewer", control, t.Cowriters, t.Viewers)
}

// skillsInjectedDetail words TaskInfo.skills_injected for the detail popup.
// The bit says the ASSIGNED runner is configured to inject the harness skill
// + inbox hook; it is not a check that the files landed (the runner's write is
// non-fatal). A task with no assignment yet has no runner to describe, so it
// reports that instead of the false-looking "not declared".
func skillsInjectedDetail(t protocol.TaskInfo) string {
	if t.SkillsInjected() {
		return "injected (runner declares +skills)"
	}
	if t.StartedAt == 0 {
		return "unknown (not assigned to a runner yet)"
	}
	return "not declared by the assigned runner"
}

func taskKindStr(k protocol.TaskKind) string {
	switch k {
	case protocol.TaskKind_Oneshot:
		return "oneshot"
	case protocol.TaskKind_Interactive:
		return "interactive"
	case protocol.TaskKind_Stream:
		return "stream"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}

func formatNanoTs(ns uint64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, int64(ns)).Format(time.RFC3339)
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
