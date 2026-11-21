package tui

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
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
	// recordsFor is the task whose PTY records the popup is waiting for (or
	// showing); base is the body without them. The list arrives after the
	// popup opens (A-D28), so it is appended when it does.
	recordsFor string
	base       string
	// records is what the block lists, for the keep form (t).
	records []protocol.RecordInfo
	// recordsBlock and transcriptsBlock are the two sections under base. They
	// arrive separately and in either order, so each replaces only its own.
	recordsBlock, transcriptsBlock string
	// transcriptsFor is the task whose agent transcripts the popup waits for.
	transcriptsFor string
	// wrap soft-wraps the body to the viewport width (OpenWrapped). Bodies
	// whose lines are long by nature (a schedule's history entries carry
	// seven times and a message) would otherwise run off the edge, taking
	// the outcome and message with them.
	wrap bool
}

func NewDetailPopup() DetailPopup {
	return DetailPopup{vp: viewport.New(80, 10)}
}

func (d *DetailPopup) IsOpen() bool { return d.open }

// OpenWrapped is Open for a body whose lines are long by nature (a
// schedule's history entries carry seven times and a message): they are
// soft-wrapped to the popup width, rune-width aware, so nothing runs off the
// edge. Chosen over a two-line entry shape because the same cli renderer
// feeds `schedule show` and the WebUI, which scroll or wrap on their own.
func (d *DetailPopup) OpenWrapped(title, body string) {
	d.Open(title, body)
	d.wrap = true
	d.vp.SetContent(d.content())
}

// content is the body as the viewport shows it.
func (d *DetailPopup) content() string {
	if !d.wrap {
		return d.body
	}
	return wrapBody(d.body, d.vp.Width)
}

// wrapBody soft-wraps each line of s to width cells, breaking at the last
// space that fits (else mid-word); continuations are indented four cells past
// the line's own indent.
func wrapBody(s string, width int) string {
	if width < 12 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		cont := indent + "    "
		cur := line
		for runewidth.StringWidth(cur) > width {
			w, cut := 0, 0
			for i, r := range cur {
				rw := runewidth.RuneWidth(r)
				if w+rw > width {
					break
				}
				w += rw
				cut = i + len(string(r))
			}
			if sp := strings.LastIndexByte(cur[:cut], ' '); sp > len(cont) {
				cut = sp
			}
			out = append(out, strings.TrimRight(cur[:cut], " "))
			cur = cont + strings.TrimLeft(cur[cut:], " ")
		}
		out = append(out, cur)
	}
	return strings.Join(out, "\n")
}

func (d *DetailPopup) Open(title, body string) {
	d.open = true
	d.wrap = false
	d.title = title
	d.body = body
	d.base, d.recordsFor, d.records = body, "", nil
	d.recordsBlock, d.transcriptsBlock, d.transcriptsFor = "", "", ""
	d.vp.SetContent(body)
	d.vp.GotoTop()
}

// WatchRecords marks the open popup as the task's, and shows block (the
// records, or that they are on their way) under its body.
func (d *DetailPopup) WatchRecords(taskID, block string) {
	d.recordsFor, d.records = taskID, nil
	d.setRecords(block)
}

// SetRecordList shows a task's records (or why they could not be listed)
// when the popup still shows taskID, and keeps them for the keep form.
func (d *DetailPopup) SetRecordList(taskID string, rs []protocol.RecordInfo, err error) {
	if !d.open || d.recordsFor != taskID {
		return
	}
	d.records = rs
	if err != nil {
		d.records = nil
	}
	d.setRecords(formatRecordsBlock(rs, err))
}

// Records is the task the popup lists records for and those records.
func (d *DetailPopup) Records() (taskID string, rs []protocol.RecordInfo) {
	return d.recordsFor, d.records
}

func (d *DetailPopup) setRecords(block string) {
	d.recordsBlock = block
	d.render()
}

// WatchTranscripts marks the open popup as waiting for the task's agent
// transcripts (pillar B) and shows block until they arrive.
func (d *DetailPopup) WatchTranscripts(taskID, block string) {
	d.transcriptsFor = taskID
	d.transcriptsBlock = block
	d.render()
}

// TranscriptsFor is the task whose transcripts the popup lists, "" when it is
// not a task's popup.
func (d *DetailPopup) TranscriptsFor() string { return d.transcriptsFor }

// SetTranscriptList shows a task's agent sessions (or why they could not be
// listed) when the popup still shows taskID.
func (d *DetailPopup) SetTranscriptList(taskID string, ss []protocol.TranscriptSession, note string, err error) {
	if !d.open || d.transcriptsFor != taskID {
		return
	}
	d.transcriptsBlock = formatTranscriptsBlock(ss, note, err)
	d.render()
}

func (d *DetailPopup) render() {
	body := strings.TrimRight(d.base, "\n")
	for _, b := range []string{d.recordsBlock, d.transcriptsBlock} {
		if b != "" {
			body += "\n" + b
		}
	}
	d.body = body
	d.vp.SetContent(d.content())
}

// formatTranscriptsBlock is the popup's agent-transcripts section: one line
// per session, as `transcript ls` prints it, then the note naming any store
// that could not be read.
func formatTranscriptsBlock(ss []protocol.TranscriptSession, note string, err error) string {
	var sb strings.Builder
	switch {
	case err != nil:
		return "transcripts:   (" + err.Error() + ")"
	case len(ss) == 0:
		sb.WriteString("transcripts:   none")
	default:
		sb.WriteString("transcripts:")
		for _, s := range ss {
			sb.WriteString("\n  " + cli.TranscriptSessionRow(s))
		}
	}
	if note != "" {
		sb.WriteString("\n  note: " + note)
	}
	return sb.String()
}

// formatRecordsBlock is the popup's records section: one line per run's
// record with its size and expiry (G43: every surface lists both).
func formatRecordsBlock(rs []protocol.RecordInfo, err error) string {
	switch {
	case err != nil:
		return "records:       (" + err.Error() + ")"
	case len(rs) == 0:
		return "records:       none"
	}
	var sb strings.Builder
	sb.WriteString("records:\n")
	for _, r := range rs {
		sb.WriteString("  " + cli.RecordRunRow(r))
		if r.Recording() {
			sb.WriteString("  recording")
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
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
	c := d.content()
	d.vp.SetContent(c)
	d.overflow = lipgloss.Height(c) > inner
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
	if d.transcriptsFor != "" {
		hint = modalKeys.DetailTranscripts + ": transcripts · " + hint
	}
	if len(d.records) > 0 {
		hint = modalKeys.DetailRecordKeep + ": keep · " + hint
	}
	if d.overflow {
		hint = scrollHint + " · " + hint
	}
	footer := FooterStyle.Render(hint)
	return box.Render(header + "\n\n" + d.vp.View() + "\n\n" + footer)
}

// formatRunnerDetail renders a multi-line, label:value description of a
// RunnerInfo for the detail popup.
func formatRunnerDetail(r protocol.RunnerInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "status:        %s\n", cli.RunnerStatusWord(r.Status))
	fmt.Fprintf(&sb, "id:            %s\n", r.Id.Hex())
	// WHERE it is reached, as its own line: id used to BE the connection id, so
	// one value answered both. Hostname does not replace it — two slots on one
	// machine share a hostname and differ only here.
	fmt.Fprintf(&sb, "cid:           %s\n", cli.RunnerCIDStr(r.Cid))
	fmt.Fprintf(&sb, "host:          %s\n", string(r.Hostname))
	// Always printed. A runner has a platform whether or not it reported one,
	// and it decides real things — which shell an `ssh host cmd` reaches, which
	// paths its roots are spelled in.
	fmt.Fprintf(&sb, "os:            %s\n", cli.RunnerGOOSStr(r.Goos))
	fmt.Fprintf(&sb, "key:           %s (%s)\n", string(r.KeyLabel), cli.KeyPrefix16(r.Key[:]))
	// How it is connected, at every width (the table shows it from 120 cells).
	switch r.Link {
	case protocol.RunnerLink_Via:
		fmt.Fprintf(&sb, "link:          %s (relay %s)\n", cli.RunnerLinkStr(r), r.LinkVia.Hex())
	default:
		fmt.Fprintf(&sb, "link:          %s\n", cli.RunnerLinkStr(r))
	}
	if len(r.LinkTarget) > 0 {
		fmt.Fprintf(&sb, "target:        %s\n", string(r.LinkTarget))
	}
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
	fmt.Fprintf(&sb, "last task:     %s\n", formatNanoTs(r.LastTaskActivity))
	return sb.String()
}

// formatTaskDetail renders a multi-line, label:value description of a
// TaskInfo for the detail popup. The prompt is shown in full at the bottom
// (it can be multi-line and is the most likely thing the user wants to
// inspect after the row's truncation).
// runnerIndex keys a runner snapshot by identity hex, the form
// TaskInfo.assigned_to carries. One helper because the task table builds the
// same map to resolve a task's agent, and a second hand-built one would be
// free to key it differently.
func runnerIndex(rs []protocol.RunnerInfo) map[string]protocol.RunnerInfo {
	byID := make(map[string]protocol.RunnerInfo, len(rs))
	for _, r := range rs {
		byID[r.Id.Hex()] = r
	}
	return byID
}

// assignedToDetail words a task's assignment: WHICH runner process, and where
// that process is reached right now.
//
// Both, because this line used to be a dial address —
// protocol.RunnerIDToConnID(t.AssignedTo).String(), back when a runner's
// identity WAS its connection id — and printing only the opaque identity took
// the address away with nothing in its place. The design said the address would
// "come from the join"; on `ls` that is fair, because the runner rows are on
// screen beside the task rows and carry `cid=`. This popup REPLACES the view,
// so here the join has to actually be performed.
//
// A runner that is no longer connected is not in the snapshot, and that is
// worth saying rather than leaving the line looking truncated: the identity is
// still the right answer to "what ran this", and "not connected" is the answer
// to "where".
func assignedToDetail(t protocol.TaskInfo, runnerByID map[string]protocol.RunnerInfo) string {
	id := t.AssignedTo.Hex()
	r, ok := runnerByID[id]
	if !ok {
		return id + "  (runner not connected)"
	}
	return fmt.Sprintf("%s  at %s (%s)", id, cli.RunnerCIDStr(r.Cid), string(r.Hostname))
}

func formatTaskDetail(t protocol.TaskInfo, runnerByID map[string]protocol.RunnerInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "id:            %s\n", hex.EncodeToString(t.Id.Id[:]))
	// The marks name a `session new --oneshot` run and a PTY choice that is
	// not the mode's default (A-D33), as on the `ls` row.
	fmt.Fprintf(&sb, "kind:          %s%s\n", taskKindStr(t.Kind), cli.RunMark(&t))
	fmt.Fprintf(&sb, "status:        %s\n", taskStatusStr(t.Status))
	// The task's display name (D21, any kind), when it has one; sanitized,
	// since an issuer typed it and it reaches this terminal.
	if len(t.Name) > 0 {
		n := string(t.Name)
		fmt.Fprintf(&sb, "name:          %s\n", vtgrid.SanitizeTitle(n, len(n)))
	}
	// A credential's expiry and last use, as `credential ls` words them; a
	// last use the server has not seen since it started is `unknown`, said.
	if t.Kind == protocol.TaskKind_Credential {
		fmt.Fprintf(&sb, "expires:       %s\n", cli.CredentialExpiryLabel(&t))
		fmt.Fprintf(&sb, "last used:     %s\n", cli.CredentialLastUsedLabel(&t))
	}
	if len(t.AgentProfile) > 0 {
		fmt.Fprintf(&sb, "agent:         %s\n", string(t.AgentProfile))
	}
	// A credential task has no process (D19): no runner, run, worktree or
	// exec, so the lines about those are not printed for it -- the subject
	// does not exist, which is not the same as a zero (checklist 31).
	proc := t.Kind != protocol.TaskKind_Credential
	// Spelled out rather than folded into the agent line's "+skills" suffix:
	// the table has no room to distinguish "the runner declares no injection"
	// from "no runner has been assigned yet", and a detail view is where that
	// difference belongs. Always printed — a hidden default reads as "this
	// task has no such property".
	if proc {
		fmt.Fprintf(&sb, "skills:        %s\n", skillsInjectedDetail(t))
	}
	// A held task's deadline, which is the only question the operator has
	// about one: status=held cannot tell 5 seconds of window from 85, and that
	// is the whole decision. Gated on the hold EXISTING rather than on its
	// value — a task with no hold has nothing to describe here, which is a
	// different thing from a hold of zero length.
	if t.HoldDeadlineNs > 0 {
		fmt.Fprintf(&sb, "held until:    %s\n", formatNanoTs(t.HoldDeadlineNs))
	}
	// Busy/idle badge + last-output timestamp for a live interactive session,
	// mirroring the task table's Act column (blank there for tasks without a
	// live session — the server leaves last_output_at at 0 for those).
	if t.LastOutputAt > 0 {
		fmt.Fprintf(&sb, "act:           %s\n", cli.ActivityStr(t.OutputIdleMs))
		fmt.Fprintf(&sb, "last output:   %s\n", formatNanoTs(t.LastOutputAt))
	}
	// The task's own line, beside last output so the reader can judge how
	// stale it is. A cleared one shows as "" — quoted, so it cannot be
	// mistaken for a task that wrote "(cleared)".
	if t.ProgressAt != 0 {
		fmt.Fprintf(&sb, "progress:      %q (%s)\n", string(t.Progress), cli.AgeStr(t.ProgressAgeMs))
	}
	// Run mode and how the run ended (A-D11, A-D14), every task.
	if r := cli.RunStr(&t); r != "" {
		fmt.Fprintf(&sb, "run:           %s\n", r)
	}
	// The run number names the run's PTY record (A-D20): <id>#<n>.
	if proc {
		fmt.Fprintf(&sb, "run number:    %d\n", t.Run)
	}
	// What the task's hooks last reported, beside its own line and the
	// output age, so the three can be read against each other.
	// Gated like the badge: not for a finished task, which must not read
	// "working" (spec D14); "-" for a live one that never reported (D28).
	if cli.LifecycleShown(&t) {
		if t.LifecycleAt == 0 {
			sb.WriteString("lifecycle:     - (never reported)\n")
		} else {
			fmt.Fprintf(&sb, "lifecycle:     %s (%s)\n", cli.LifecycleWord(t.Lifecycle), cli.AgeStr(t.LifecycleAgeMs))
		}
	}
	// The program's own OSC 7501 report, under what its hooks said, so a
	// disagreement between the two is on screen rather than resolved away.
	// The msg is the program's text: control characters stripped here, as
	// for the title.
	if p := cli.ProgramDetail(&t); p != "" {
		fmt.Fprintf(&sb, "program:       %s\n", vtgrid.SanitizeTitle(p, len(p)))
	}
	// Printed for every live terminal, empty included: "untitled" is a
	// measurement.
	if t.LiveTerminal() {
		fmt.Fprintf(&sb, "title:         %q\n", string(t.Title))
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
	// The schedule this task is bound to (pillar S): the full id, which is what
	// `schedule show` / `edit` / `run` / `rm` take. Gated on the binding
	// existing, never on its value, as `ls` does.
	if t.SchedulePresent() {
		if id := t.ScheduleId(); id != nil {
			fmt.Fprintf(&sb, "schedule:      %s\n", hex.EncodeToString(id.Id[:]))
		}
	}
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
	fmt.Fprintf(&sb, "delegate:      %s\n", cli.CapsLabel(t.DelegateCaps))
	fmt.Fprintf(&sb, "delegate-scope: %s\n", cli.ScopeLabel(t.DelegateScope))
	fmt.Fprintf(&sb, "spawn limits:  depth=%s max-descendants=%s descendants=%d\n",
		authority.LimitLabel(t.SpawnDepth), authority.LimitLabel(t.MaxDescendants), t.Descendants)
	fmt.Fprintf(&sb, "granter:       %s\n", cli.AuthorityLabel(t))
	if proc {
		fmt.Fprintf(&sb, "repo:          %s\n", string(t.RepoPath))
	}
	if len(t.WorktreeDir) > 0 {
		fmt.Fprintf(&sb, "worktree:      %s\n", string(t.WorktreeDir))
	}
	// Always printed, zero included. Unlike the attach counts above there is no
	// "no subject" case to gate on: every task can be asked how many commands
	// are running in its tree, and none is a real answer to that. Eliding it
	// would make "nothing is running" read as "this popup does not report
	// execs" (surface-parity item 31).
	if proc {
		fmt.Fprintf(&sb, "execs:         %d running\n", t.ExecCount)
	}
	fmt.Fprintf(&sb, "created:       %s\n", formatNanoTs(t.CreatedAt))
	if t.StartedAt > 0 {
		fmt.Fprintf(&sb, "started:       %s\n", formatNanoTs(t.StartedAt))
		if proc {
			fmt.Fprintf(&sb, "assigned to:   %s\n", assignedToDetail(t, runnerByID))
		}
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
	case protocol.TaskKind_Credential:
		return "credential"
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
