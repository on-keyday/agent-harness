package tui

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
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
