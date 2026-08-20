package tui

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// DetailPopup is a read-only popup that displays formatted details for a
// selected runner or task row. Long fields (full repo path, worktree dir,
// multi-line prompt) that the row table truncates are shown in full here.
// The popup intercepts no keys other than Esc (close) — it has no editable
// state.
type DetailPopup struct {
	open  bool
	title string
	body  string
}

func (d *DetailPopup) IsOpen() bool { return d.open }

func (d *DetailPopup) Open(title, body string) {
	d.open = true
	d.title = title
	d.body = body
}

func (d *DetailPopup) Close() {
	d.open = false
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
	footer := FooterStyle.Render("Esc: close")
	return box.Render(header + "\n\n" + d.body + "\n\n" + footer)
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
	fmt.Fprintf(&sb, "repo:          %s\n", string(t.RepoPath))
	if len(t.WorktreeDir) > 0 {
		fmt.Fprintf(&sb, "worktree:      %s\n", string(t.WorktreeDir))
	}
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
	parts := []string{"no control"}
	if t.IsAttached() {
		parts[0] = "control"
	}
	if t.Cowriters > 0 {
		parts = append(parts, fmt.Sprintf("%d cowrite", t.Cowriters))
	}
	if t.Viewers > 0 {
		parts = append(parts, fmt.Sprintf("%d viewer", t.Viewers))
	}
	return strings.Join(parts, ", ")
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
