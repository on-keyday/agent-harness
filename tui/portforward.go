package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ForwardDirection distinguishes local (-L) and remote (-R) forwards.
type ForwardDirection int

const (
	ForwardLocal ForwardDirection = iota
	ForwardRemote
)

func (d ForwardDirection) flag() string {
	if d == ForwardRemote {
		return "-R"
	}
	return "-L"
}

func pfShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// PortForwardModal prompts for one forward spec for a selected task. mode picks
// local vs remote (placeholder + dispatch differ).
type PortForwardModal struct {
	open   bool
	taskID string
	mode   ForwardDirection
	input  textinput.Model
}

func (m *PortForwardModal) IsOpen() bool           { return m.open }
func (m *PortForwardModal) TaskID() string         { return m.taskID }
func (m *PortForwardModal) Mode() ForwardDirection { return m.mode }

// Open opens the modal in local mode (back-compat for existing call sites).
func (m *PortForwardModal) Open(taskID string) { m.OpenMode(taskID, ForwardLocal) }

// OpenMode opens the modal for taskID in the given direction.
func (m *PortForwardModal) OpenMode(taskID string, dir ForwardDirection) {
	m.taskID = taskID
	m.mode = dir
	if m.input.Prompt == "" {
		m.input = textinput.New()
	}
	if dir == ForwardRemote {
		m.input.Placeholder = "[bind:]runnerport:dialhost:dialport"
	} else {
		m.input.Placeholder = "[bind:]localport:remotehost:remoteport"
	}
	m.input.SetValue("")
	m.input.Focus()
	m.open = true
}

func (m *PortForwardModal) Close() {
	m.open = false
	m.input.Blur()
}

func (m *PortForwardModal) Spec() string { return m.input.Value() }

func (m *PortForwardModal) Update(msg tea.Msg) (PortForwardModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

func (m *PortForwardModal) View() string {
	if !m.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	footer := FooterStyle.Render("Enter to start · Esc to cancel")
	return box.Render("Port-forward task " + pfShortID(m.taskID) + "  " + m.mode.flag() + " " + m.input.View() + "\n\n" + footer)
}

// PortForwardSession tracks a running forward so it can be identified for a
// stop request. ID is a client-side unique handle so a task can hold several
// forwards at once. ForwardID is the server-assigned id from
// RegisterPortForward — the only thing KillPortForwardWith accepts, so it is
// what a stop request actually needs. It is populated synchronously for a
// remote (-R) forward (OpenRemoteForward returns it directly) and
// asynchronously for a local (-L) forward (PortForwardRegisteredMsg backfills
// it once cli.RunForward's registration completes); zero means "not yet
// known — nothing to kill".
type PortForwardSession struct {
	ID        int
	TaskID    string
	Direction ForwardDirection
	Spec      string
	Cancel    context.CancelFunc
	ForwardID uint64
	// FromWorkspace marks a forward started by a workspace apply. It is what
	// reconciliation acts on: an apply may stop only forwards it owns, so one
	// the operator started by hand is never taken away. Client-local
	// bookkeeping — the server-side registry has no notion of a workspace, and
	// giving it one would mean a wire field.
	FromWorkspace bool
}

// selectForwards returns the active sessions for a task in one direction, sorted
// by ID for stable picker ordering.
func selectForwards(m map[int]*PortForwardSession, taskID string, dir ForwardDirection) []*PortForwardSession {
	var out []*PortForwardSession
	for _, s := range m {
		if s.TaskID == taskID && s.Direction == dir {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForwardPicker lists active forwards (one task + direction) for digit-key
// selection, shown when more than one is active for a stop request.
type ForwardPicker struct {
	open     bool
	dir      ForwardDirection
	sessions []*PortForwardSession
}

func (p *ForwardPicker) IsOpen() bool { return p.open }

func (p *ForwardPicker) Open(dir ForwardDirection, sessions []*PortForwardSession) {
	p.open = true
	p.dir = dir
	p.sessions = sessions
}

func (p *ForwardPicker) Close() { p.open = false; p.sessions = nil }

// Pick maps a digit key ("1".."9") to a session, or nil if out of range.
func (p *ForwardPicker) Pick(key string) *PortForwardSession {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return nil
	}
	idx := int(key[0] - '1')
	if idx >= len(p.sessions) {
		return nil
	}
	return p.sessions[idx]
}

func (p *ForwardPicker) View() string {
	if !p.open {
		return ""
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFocused).
		Padding(1, 2)
	body := "Stop which " + p.dir.flag() + " forward?\n\n"
	for i, s := range p.sessions {
		body += fmt.Sprintf("%d) %s\n", i+1, s.Spec)
	}
	body += "\n" + FooterStyle.Render("press number to stop · Esc to cancel")
	return box.Render(body)
}

// PortForwardStatusMsg carries a line to append to cmdresult.
type PortForwardStatusMsg struct{ Line string }

// PortForwardStartedMsg registers a started forward in the App. ForwardID is
// the server-assigned id when known synchronously (remote forwards); zero for
// a local forward, whose id arrives later via PortForwardRegisteredMsg.
type PortForwardStartedMsg struct {
	ID            int
	TaskID        string
	Direction     ForwardDirection
	Spec          string
	Cancel        context.CancelFunc
	ForwardID     uint64
	FromWorkspace bool
}

// PortForwardRegisteredMsg backfills the server-assigned forward id onto a
// local (-L) forward's session once its RegisterPortForward call completes.
// cli.RunForward blocks for the whole lifetime of the forward and only
// reports the id via the onRegistered callback, not a return value, so
// DoStartPortForward relays it through this message instead. Needed so the
// tasks-pane P/B stop and the forwards-modal `x` kill can name the forward via
// KillPortForwardWith — the only stop path after this task.
type PortForwardRegisteredMsg struct {
	ID        int
	ForwardID uint64
}

// PortForwardStoppedMsg removes a finished/failed forward from the App so it no
// longer lingers in the stop picker. Sent when the forward goroutine exits —
// including a bind failure, where the forward never actually ran.
type PortForwardStoppedMsg struct {
	ID     int
	TaskID string
}

// forwardFailLine renders a clearly-marked failure line so a failed forward is
// unmistakable (the old flow showed a green "forward started" even on failure).
func forwardFailLine(taskID string, err error) string {
	return WarnStyle.Render("✗ forward failed: ") + pfShortID(taskID) + "  " + err.Error()
}

// DoStartPortForward parses the spec and starts a background local (-L) forward
// using the long-lived client (NOT a fresh dial). program MUST be App's
// *tea.Program (goroutines emit messages via program.Send).
//
// Started is emitted via program.Send (not the cmd return value) so it is
// enqueued before the goroutine's Stopped message — otherwise a fast failure
// could enqueue Stopped first and leave a stale entry in activeForwards.
// forwardStatusLogf returns a logf-style callback that delivers per-connection
// forward status lines to the TUI WITHOUT ever blocking the caller.
//
// bubbletea's program.Send writes to an UNBUFFERED msgs channel (tea.go), so a
// direct Send from a forward relay goroutine parks whenever the event loop is
// not draining. Because cli.dialAndSplice logs the dial-failure line BEFORE it
// CloseBoth's the stream, that block stalls connection teardown: the
// runner-side accepted connection is never closed and the forwarded peer (e.g.
// curl) hangs for as long as the park lasts. It used to last a whole attached
// session, when the event loop sat inside tea.Exec/RemoteShell and drained
// nothing; the attach path no longer suspends the loop (suspend.go), but a
// relay goroutine still must not wait on the UI to make progress. Routing
// status through a
// buffered channel drained by a single goroutine decouples cosmetic logging from
// the relay's progress: the relay does a non-blocking send and on overflow the
// pending status line is dropped (status is cosmetic). The drain goroutine exits
// when ctx is cancelled (forward stop).
func forwardStatusLogf(ctx context.Context, program *tea.Program) func(string) {
	ch := make(chan string, 256)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-ch:
				program.Send(PortForwardStatusMsg{Line: s})
			}
		}
	}()
	return func(s string) {
		select {
		case ch <- s:
		default: // UI suspended (interactive session) + buffer full — drop cosmetic line
		}
	}
}

func DoStartPortForward(c *cli.Client, taskID, spec string, id int, program *tea.Program, fromWorkspace bool) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		program.Send(PortForwardStartedMsg{ID: id, TaskID: taskID, Direction: ForwardLocal, Spec: spec, Cancel: cancel, FromWorkspace: fromWorkspace})
		go func() {
			onRegistered := func(_ cli.ForwardSpec, fid uint64) {
				program.Send(PortForwardRegisteredMsg{ID: id, ForwardID: fid})
			}
			if err := cli.RunForward(ctx, c, taskID, []cli.ForwardSpec{sp}, forwardStatusLogf(ctx, program), onRegistered); err != nil {
				program.Send(PortForwardStatusMsg{Line: forwardFailLine(taskID, err)})
			}
			program.Send(PortForwardStoppedMsg{ID: id, TaskID: taskID})
		}()
		return nil
	}
}

// DoStartRemoteForward is the -R counterpart. It confirms the runner actually
// bound the listener (OpenRemoteForward blocks until the bind result) BEFORE
// registering, so a bind failure shows a clear error instead of a misleading
// "forward started" followed by an error.
func DoStartRemoteForward(c *cli.Client, taskID, spec string, id int, program *tea.Program, fromWorkspace bool) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseRemoteForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		ctrl, fid, err := c.OpenRemoteForward(ctx, taskID, sp)
		if err != nil {
			cancel()
			return PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}
		}
		program.Send(PortForwardStartedMsg{ID: id, TaskID: taskID, Direction: ForwardRemote, Spec: spec, Cancel: cancel, ForwardID: fid, FromWorkspace: fromWorkspace})
		go func() {
			c.ServeRemoteForwardControl(ctx, sp, ctrl, forwardStatusLogf(ctx, program))
			program.Send(PortForwardStoppedMsg{ID: id, TaskID: taskID})
		}()
		return nil
	}
}

// ForwardsSnapshotMsg carries the result of DoListForwards. ToCmdresult is
// set only by the cmdline `forward ls` dispatch: it gates whether the text
// table (cli.PortForwardInfoLines) is appended to cmdresult, a 200-line
// ring that evicts oldest-first (CmdResultModel.Append). Without the gate,
// every `f` keypress — plus every kill-triggered refresh while the modal is
// open — would ALSO dump a banner + header + one line per forward, which can
// evict an earlier error notice the operator still needs. The modal itself
// (ApplySnapshot) always applies regardless of ToCmdresult; only the text
// dump is conditional.
type ForwardsSnapshotMsg struct {
	Forwards    []protocol.PortForwardInfo
	Err         error
	ToCmdresult bool
}

// DoListForwards fetches every forward visible to this operator. Uses the
// long-lived client (a.client), like every other Do* in this file.
// toCmdresult is threaded straight onto the result (see ForwardsSnapshotMsg).
func DoListForwards(c *cli.Client, toCmdresult bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fs, err := c.PortForwardListWith(ctx, "")
		return ForwardsSnapshotMsg{Forwards: fs, Err: err, ToCmdresult: toCmdresult}
	}
}

// ForwardKillResultMsg carries the result of a DoKillForward request.
// TaskID/Spec are optional context for the confirmation line — set whenever
// the caller already knows what it's killing (the forwards modal's confirm,
// the tasks-pane P/B / picker), empty for a bare cmdline `forward kill <id>`
// where the operator supplied only a number and there is nothing to echo
// back beyond it.
type ForwardKillResultMsg struct {
	ID     uint64
	TaskID string
	Spec   string
	Err    error
}

// DoKillForward asks the server to close one registered forward by id. This
// is the only stop path after this task — it works identically whether the
// forward was started by this TUI (forwards modal `x`, tasks-pane P/B) or by
// a different client (forwards modal `x` on someone else's row, or the
// `forward kill` cmdline verb): the server pushes a `closed` event onto the
// owning client's control stream, and that client's already-running
// serveLocalForwardControl / ServeRemoteForwardControl loop tears the forward
// down from there (unaffected by this task).
func DoKillForward(c *cli.Client, id uint64, taskID, spec string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.KillPortForwardWith(ctx, id)
		return ForwardKillResultMsg{ID: id, TaskID: taskID, Spec: spec, Err: err}
	}
}

// portForwardInfoRow maps one server-side forward to its table row. Origin
// reuses cli.PortForwardOrigin (kind + cid) rather than re-deriving just the
// kind locally: the cid is what actually distinguishes two forwards with an
// identical spec started by different clients — the entire point of a
// shared registry — and duplicating the rendering would let this surface and
// harness-cli's `forward ls` silently disagree on what "origin" means.
func portForwardInfoRow(fi *protocol.PortForwardInfo) table.Row {
	return table.Row{
		fmt.Sprintf("%d", fi.ForwardId),
		cli.PortForwardDirFlag(fi.Direction),
		pfShortID(FormatTaskID(fi.TaskId)),
		cli.PortForwardSpecString(fi),
		cli.PortForwardOrigin(fi),
	}
}

// ForwardsModal is a full-screen overlay listing every port forward visible
// to this operator on the server — not just ones this TUI process started.
// Opened with `f` (fetches via DoListForwards), closed with Esc. `x` arms a
// y/n kill confirmation for the selected row (BeginKillConfirm /
// ConfirmKill / CancelKillConfirm — App drives the actual DoKillForward
// dispatch since it alone holds a.client); `j`/`k` are left to the embedded
// table's own LineDown/LineUp bindings (bubbles table.go DefaultKeyMap),
// deliberately NOT intercepted for kill — see tui/grid.go's k/j comment for
// the same convention elsewhere in this layer. It mirrors ConnsModal's
// snapshot shape (ApplySnapshot from a server round-trip) rather than the
// old App.activeForwards-fed design: forwards have no live push subscription,
// so unlike ConnsModal there is no incremental ApplyEvent — a fresh fetch is
// the only way to refresh.
type ForwardsModal struct {
	open     bool
	table    table.Model
	baseCols []table.Column // natural column sizing; see fitColumns
	forwards []protocol.PortForwardInfo

	// Pending kill confirmation (armed by BeginKillConfirm, resolved by
	// ConfirmKill/CancelKillConfirm). confirmID == 0 means none pending —
	// the server's forward-id counter starts at 1 (server/port_forward_registry.go),
	// so 0 is never a real forward id.
	confirmID     uint64
	confirmTaskID string
	confirmSpec   string
}

// NewForwardsModal constructs a ForwardsModal with fixed column widths.
// origin is wide enough for "<kind> <cid>" (e.g. "cli ws:127.0.0.1:41436-0")
// without truncation for realistic cids — bubbles/table hard-truncates a
// cell to its column Width with an ellipsis (table.go renderRow), so a
// too-narrow column would silently hide the cid half that matters most.
func NewForwardsModal() ForwardsModal {
	cols := []table.Column{
		{Title: "id", Width: 6},
		{Title: "dir", Width: 4},
		{Title: "task", Width: 12},
		{Title: "spec", Width: 44},
		{Title: "origin", Width: 30},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return ForwardsModal{table: t, baseCols: cols}
}

func (m *ForwardsModal) IsOpen() bool { return m.open }
func (m *ForwardsModal) Open()        { m.open = true }
func (m *ForwardsModal) Close()       { m.open = false }

// SetSize propagates terminal dimensions into the table (full-screen overlay).
// Reserve 4 rows for border + header + footer (as ConnsModal.SetSize).
func (m *ForwardsModal) SetSize(w, h int) {
	m.table.SetWidth(w - 4)
	m.table.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "origin")))
	m.table.SetHeight(h - 4)
}

// ApplySnapshot replaces the rows with the given server snapshot.
func (m *ForwardsModal) ApplySnapshot(fs []protocol.PortForwardInfo) {
	m.forwards = make([]protocol.PortForwardInfo, len(fs))
	copy(m.forwards, fs)
	rows := make([]table.Row, 0, len(m.forwards))
	for i := range m.forwards {
		rows = append(rows, portForwardInfoRow(&m.forwards[i]))
	}
	m.table.SetRows(rows)
}

// SelectedID returns the forward id under the cursor.
func (m *ForwardsModal) SelectedID() (uint64, bool) {
	if len(m.forwards) == 0 {
		return 0, false
	}
	i := m.table.Cursor()
	if i < 0 || i >= len(m.forwards) {
		return 0, false
	}
	return m.forwards[i].ForwardId, true
}

// IsConfirming reports whether a kill confirmation is currently pending.
// While true, App swallows every key except y/n/Esc so the table (and its
// j/k navigation) doesn't move under the operator mid-confirm.
func (m *ForwardsModal) IsConfirming() bool { return m.confirmID != 0 }

// BeginKillConfirm arms the y/n confirmation for the row under the cursor.
// Returns false (no-op) if nothing is selected. The target may belong to a
// different operator's `harness-cli forward` session — only its owner can
// re-establish it — so this is deliberately not a direct kill: see the
// picker's push/pull overwrite prompt (tui/filepicker.go handlePushOverwriteKey)
// for the existing precedent of a confirm gate in this layer.
func (m *ForwardsModal) BeginKillConfirm() bool {
	id, ok := m.SelectedID()
	if !ok {
		return false
	}
	fi := &m.forwards[m.table.Cursor()]
	m.confirmID = id
	m.confirmTaskID = FormatTaskID(fi.TaskId)
	m.confirmSpec = cli.PortForwardDirFlag(fi.Direction) + " " + cli.PortForwardSpecString(fi)
	return true
}

// CancelKillConfirm clears a pending confirmation without killing anything.
func (m *ForwardsModal) CancelKillConfirm() {
	m.confirmID, m.confirmTaskID, m.confirmSpec = 0, "", ""
}

// ConfirmKill returns the pending kill's (id, taskID, spec) and clears the
// pending state. ok is false if nothing was pending (defensive — App only
// calls this from the "y" branch while IsConfirming is true).
func (m *ForwardsModal) ConfirmKill() (id uint64, taskID, spec string, ok bool) {
	if m.confirmID == 0 {
		return 0, "", "", false
	}
	id, taskID, spec = m.confirmID, m.confirmTaskID, m.confirmSpec
	m.CancelKillConfirm()
	return id, taskID, spec, true
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
	header := HeaderStyle.Render(fmt.Sprintf("active port forwards (%d)", len(m.forwards)))
	box := PanelStyleFocused.Padding(0, 1)
	if m.confirmID != 0 {
		prompt := fmt.Sprintf("kill forward %d (%s  %s) ? (y/n)", m.confirmID, pfShortID(m.confirmTaskID), m.confirmSpec)
		footer := FooterStyle.Render(prompt)
		return box.Render(header + "\n" + m.table.View() + "\n" + footer)
	}
	footer := FooterStyle.Render("x: kill · Esc: close")
	if len(m.forwards) == 0 {
		return box.Render(header + "\n" + "no active forwards" + "\n" + footer)
	}
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
