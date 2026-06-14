package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type focus int

const (
	focusRunners focus = iota
	focusTasks
	focusLogs
	focusNotify
	focusCmdresult
	focusCmdline
	numFocus = iota
)

// App is the top-level Bubble Tea Model.
type App struct {
	server      string
	defaultRepo string

	runners    RunnersModel
	tasks      TasksModel
	logs       LogsModel
	notify     NotifyModel
	cmdresult  CmdResultModel
	cmdline    textinput.Model
	popup      PopupModel
	detail     DetailPopup
	filepicker FilePickerModel

	focus  focus
	width  int
	height int

	// connected mirrors the persistent connection's status (set later by main.go via msgs).
	connected bool

	// status is a one-line message at the top (e.g., "DISCONNECTED — retrying").
	// Reserved for later tasks.
	status string

	// tasksByID holds the latest TaskInfo keyed by FormatTaskID(t.Id).
	tasksByID map[string]protocol.TaskInfo
	// runnersSnapshot holds the latest runners from the most recent snapshot.
	runnersSnapshot []protocol.RunnerInfo

	// port-forward state
	portForwardModal PortForwardModal
	forwardPicker    ForwardPicker
	activeForwards   map[int]*PortForwardSession // keyed by client-side unique id
	nextForwardID    int

	// log-following state
	logsCancel context.CancelFunc
	client     *cli.Client
	appCtx     context.Context
	program    *tea.Program

	// x11Cancel stops the background -R forward of the current X11 interactive
	// session. Set when InteractiveReadyMsg carries one; called and cleared on
	// InteractiveDoneMsg so the forward stops with the session.
	x11Cancel context.CancelFunc
}

// NotifyResultMsg carries the result of a notify send command.
type NotifyResultMsg struct {
	Level string
	Title string
	Err   error
}

// DoNotify sends a notification over the persistent *cli.Client. level is
// "info|warn|error"; empty defaults to "info".
func DoNotify(c *cli.Client, level, title, text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if level == "" {
			level = "info"
		}
		err := c.Notify(ctx, level, title, text)
		return NotifyResultMsg{Level: level, Title: title, Err: err}
	}
}

type Config struct {
	Server      string
	DefaultRepo string
}

func New(cfg Config) *App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = "submit / interactive / session / file / server / cancel / notify / prune / repo / clear / help / quit"
	cmd.CharLimit = 1024
	cmd.Width = 60
	a := &App{
		server:      cfg.Server,
		defaultRepo: cfg.DefaultRepo,
		runners:     NewRunners(),
		tasks:       NewTasks(),
		logs:        NewLogs(),
		notify:      NewNotify(),
		cmdresult:   NewCmdResult(),
		cmdline:     cmd,
		popup:       NewPopup(cfg.DefaultRepo),
		filepicker:  NewFilePicker(),
		focus:          focusTasks,
		connected:      false,
		status:         "connecting…",
		tasksByID:      map[string]protocol.TaskInfo{},
		activeForwards: map[int]*PortForwardSession{},
	}
	a.tasks.Focus()
	return a
}

// BindContext stores the application-level context for spawning per-task subscriptions.
func (a *App) BindContext(ctx context.Context) { a.appCtx = ctx }

// BindClient stores the active *cli.Client. Safe ONLY when called before
// the bubbletea program has started. Once the program is running, callers
// must send a BindClientMsg via program.Send instead so writes happen on
// the Update thread.
func (a *App) BindClient(c *cli.Client) {
	a.client = c
}

// BindProgram stores the tea.Program so per-task subscriber goroutines can
// dispatch LogChunkMsg back to the model.
func (a *App) BindProgram(p *tea.Program) { a.program = p }

func (a *App) Init() tea.Cmd {
	return textinput.Blink
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SnapshotMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("snapshot: " + msg.Err.Error()))
			return a, nil
		}
		a.runnersSnapshot = msg.Runners
		a.runners.SetRows(msg.Runners)
		a.tasksByID = make(map[string]protocol.TaskInfo, len(msg.Tasks))
		for _, t := range msg.Tasks {
			a.tasksByID[FormatTaskID(t.Id)] = t
		}
		a.refreshTasksTable()
		return a, nil

	case TaskEventMsg:
		id := FormatTaskID(msg.Event.TaskId)
		if msg.Event.Kind == protocol.StatusEventKind_TaskPruned {
			// Server forgot this task — drop its row immediately instead of
			// waiting for the next incidental snapshot refresh (the TUI has no
			// periodic poll; the WebUI's 5s poll is its own backstop).
			delete(a.tasksByID, id)
			a.refreshTasksTable()
			return a, nil
		}
		cur, ok := a.tasksByID[id]
		if !ok {
			// First time we see this task. TaskStatusEvent carries id /
			// status / kind / timestamps but not RepoPath, Prompt, or
			// WorktreeDir — those live only in the full TaskInfo from
			// List. Stub the row so the table reflects the new task
			// immediately (with the correct interactive/oneshot kind),
			// then kick a snapshot refresh so the remaining fields fill
			// in on the next tick rather than waiting for the periodic
			// refresh cadence.
			var ti protocol.TaskInfo
			ti.Id = msg.Event.TaskId
			ti.Status = msg.Event.TaskStatus
			ti.Kind = msg.Event.TaskKind
			ti.CreatedAt = msg.Event.Ts
			a.tasksByID[id] = ti
			a.refreshTasksTable()
			return a, RefreshSnapshot(a.client)
		}
		cur.Status = msg.Event.TaskStatus
		if msg.Event.Kind == protocol.StatusEventKind_TaskEnded {
			cur.ExitCode = msg.Event.ExitCode
			cur.EndedAt = msg.Event.Ts
		}
		a.tasksByID[id] = cur
		a.refreshTasksTable()
		return a, nil

	case RunnerEventMsg:
		// server-side RunnerStatusEvent.RunnerId is a placeholder (not keyable),
		// so we kick a full snapshot refresh on every runner event.
		return a, RefreshSnapshot(a.client)

	case LogChunkMsg:
		if msg.TaskID == a.logs.TaskID() {
			a.logs.Append(msg.Chunk)
		}
		return a, nil

	case NotifyEventMsg:
		a.notify.Append(msg.Event)
		return a, nil

	case LogHistoryMsg:
		// The user may have switched tasks between fetch and arrival; only
		// apply if it still matches.
		if msg.TaskID != a.logs.TaskID() {
			return a, nil
		}
		if msg.Err != nil {
			a.cmdresult.Append(WarnStyle.Render("history fetch failed: " + msg.Err.Error()))
			return a, nil
		}
		if !msg.Found {
			// Server has no log file for this task (e.g. pruned, or DataDir
			// unset). Leave the placeholder; the live subscription, if any,
			// will append from there.
			return a, nil
		}
		// Prepend history before any live chunks that may have already arrived.
		a.logs.Prepend(msg.Content)
		return a, nil

	case BindClientMsg:
		a.client = msg.Client
		return a, nil

	case ConnectionMsg:
		a.connected = msg.Connected
		switch {
		case msg.Connected:
			// fresh attach; logs are re-followed by the main goroutine on reconnect.
		case msg.Reconnecting:
			txt := fmt.Sprintf("reconnecting (attempt %d, next try in %s)",
				msg.Attempt, msg.NextRetry.Truncate(time.Second))
			if msg.Err != nil {
				txt += ": " + msg.Err.Error()
			}
			a.cmdresult.Append(FooterStyle.Render(txt))
		default:
			if msg.Err != nil {
				a.cmdresult.Append(ErrorStyle.Render("disconnected: " + msg.Err.Error()))
			} else {
				a.cmdresult.Append(ErrorStyle.Render("disconnected"))
			}
		}
		return a, nil

	case LogTailMsg:
		// slog records routed via SlogTailHandler land here. Display in cmdresult
		// with a dim "[log]" prefix so they share the panel without scribbling
		// over the alt-screen TUI.
		a.cmdresult.Append(FooterStyle.Render("[log] " + msg.Line))
		return a, nil

	case SubmitResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("submit failed: " + msg.Err.Error()))
			return a, nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("submitted: ") + short)
		// Pull a fresh snapshot so the new row shows up populated (Repo /
		// Prompt) without waiting for the periodic refresh — and without
		// leaving the user looking at a stub that arrived via TaskEventMsg.
		return a, RefreshSnapshot(a.client)

	case CancelResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("cancel failed: " + msg.Err.Error()))
			return a, nil
		}
		short := msg.Resolved
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("cancelled ") + short)
		return a, nil

	case PruneResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("prune failed: " + msg.Err.Error()))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("pruned %d task(s)", msg.Removed)))
		return a, RefreshSnapshot(a.client)

	case FileResultMsg:
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		// Tee to picker first so it can refresh its listing in-place.
		var pcmd tea.Cmd
		if a.filepicker.IsOpen() {
			a.filepicker, pcmd = a.filepicker.Update(msg)
		}
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("file %s %s: %s", msg.Op, short, msg.Err.Error())))
			return a, pcmd
		}
		if msg.Op == "ls" {
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("file ls %s %s", short, msg.Detail)))
			for _, line := range strings.Split(strings.TrimRight(msg.Output, "\n"), "\n") {
				if line == "" {
					continue
				}
				a.cmdresult.Append("  " + line)
			}
			return a, pcmd
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("file %s %s ok ", msg.Op, short)) + msg.Detail)
		return a, pcmd

	case NotifyResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("notify failed: " + msg.Err.Error()))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("notify [%s] %q sent", msg.Level, msg.Title)))
		return a, nil

	case ServerDialResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("server dial-runner %s: %v", msg.RunnerCID, msg.Err)))
			return a, nil
		}
		if msg.Status == protocol.DialRunnerStatus_Ok {
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("server dial-runner %s: ok", msg.RunnerCID)))
		} else {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("server dial-runner %s: %s", msg.RunnerCID, msg.Status.String())))
		}
		return a, RefreshSnapshot(a.client)

	case FilePickerListingMsg:
		var pcmd tea.Cmd
		a.filepicker, pcmd = a.filepicker.Update(msg)
		return a, pcmd

	case InteractiveReadyMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("open interactive failed: " + msg.Err.Error()))
			return a, nil
		}
		if msg.X11Warn != "" {
			a.cmdresult.Append(WarnStyle.Render("x11: " + msg.X11Warn))
		}
		a.x11Cancel = msg.X11Cancel
		if msg.X11Cancel != nil {
			a.cmdresult.Append(OKStyle.Render("x11 forward started: ") + pfShortID(msg.TaskID))
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("attaching ") + short + " — Ctrl+] to detach client; Ctrl+D / `exit` ends the session")
		return a, tea.Exec(&interactiveExec{stream: msg.Stream}, func(err error) tea.Msg {
			return InteractiveDoneMsg{TaskID: msg.TaskID, Err: err}
		})

	case SessionStartedMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("session start failed: " + msg.Err.Error()))
			return a, nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("started detached: ") + short)
		return a, RefreshSnapshot(a.client)

	case InteractiveDoneMsg:
		if a.x11Cancel != nil {
			a.x11Cancel()
			a.x11Cancel = nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		if msg.Err != nil {
			a.cmdresult.Append(WarnStyle.Render("interactive ended: ") + short + " (" + msg.Err.Error() + ")")
		} else {
			a.cmdresult.Append(OKStyle.Render("interactive ended: ") + short)
		}
		return a, RefreshSnapshot(a.client)

	case SessionListMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("session ls: " + msg.Err.Error()))
			return a, nil
		}
		if len(msg.Tasks) == 0 {
			a.cmdresult.Append("session ls: no detachable sessions")
			return a, nil
		}
		for _, t := range msg.Tasks {
			id := FormatTaskID(t.Id)
			short := id
			if len(short) > 12 {
				short = short[:12]
			}
			attached := ""
			if t.IsAttached() {
				attached = " [attached]"
			}
			a.cmdresult.Append(fmt.Sprintf("%s  %-10s%s  %s", short, t.Status.String(), attached, string(t.RepoPath)))
		}
		return a, nil

	case PortForwardStartedMsg:
		a.activeForwards[msg.ID] = &PortForwardSession{ID: msg.ID, TaskID: msg.TaskID, Direction: msg.Direction, Spec: msg.Spec, Cancel: msg.Cancel}
		a.cmdresult.Append(OKStyle.Render("forward started: ") + pfShortID(msg.TaskID) + "  " + msg.Direction.flag() + " " + msg.Spec)
		return a, nil

	case PortForwardStoppedMsg:
		// The forward goroutine exited (stopped, or never started on bind
		// failure). Drop it so it no longer shows in the stop picker. If it was
		// already removed (e.g. the user cancelled it via the picker), this is a
		// no-op and we skip the duplicate log.
		if _, ok := a.activeForwards[msg.ID]; ok {
			delete(a.activeForwards, msg.ID)
			a.cmdresult.Append("forward stopped: " + pfShortID(msg.TaskID))
		}
		return a, nil

	case PortForwardStatusMsg:
		a.cmdresult.Append(msg.Line)
		return a, nil

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.layout()
		a.filepicker.SetSize(a.width, a.height)
		return a, nil

	case tea.KeyMsg:
		// Detail popup is read-only — Esc closes, all other keys swallowed
		// so cursor movement / 'q' / etc. don't leak through.
		if a.detail.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.detail.Close()
			}
			return a, nil
		}
		// File picker intercepts ALL keys when open.
		if a.filepicker.IsOpen() {
			var pcmd tea.Cmd
			a.filepicker, pcmd = a.filepicker.Update(msg)
			return a, pcmd
		}
		// Submit popup intercepts ALL keys when open.
		if a.popup.IsOpen() {
			switch msg.Type {
			case tea.KeyEsc:
				a.popup.Close()
				return a, nil
			case tea.KeyCtrlJ:
				// Bubbletea reports Ctrl+Enter as Ctrl+J on most terminals.
				repo := a.popup.Repo()
				prompt := a.popup.Prompt()
				host := a.popup.Host()
				extraArgs := a.popup.ExtraArgs()
				resumeID := a.popup.ResumeTaskID()
				a.popup.Close()
				if prompt == "" {
					a.cmdresult.Append(WarnStyle.Render("submit cancelled (empty prompt)"))
					return a, nil
				}
				// repo is irrelevant on resume — server uses the existing
				// task's RepoPath. Only require it for fresh submits.
				if repo == "" && resumeID == "" {
					a.cmdresult.Append(WarnStyle.Render("submit cancelled (no repo — wait for a runner to register, then reopen with `s`)"))
					return a, nil
				}
				return a, DoSubmitWithOpts(a.client, repo, prompt, host, extraArgs, resumeID)
			case tea.KeyTab:
				a.popup.CycleRepo(+1)
				return a, nil
			case tea.KeyShiftTab:
				a.popup.CycleHost(+1)
				return a, nil
			case tea.KeyCtrlE:
				a.popup.ToggleFocus()
				return a, nil
			}
			var pcmd tea.Cmd
			a.popup, pcmd = a.popup.Update(msg)
			return a, pcmd
		}
		// Forward-stop picker intercepts keys when open (digit selects, Esc cancels).
		if a.forwardPicker.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.forwardPicker.Close()
				return a, nil
			}
			if sess := a.forwardPicker.Pick(msg.String()); sess != nil {
				sess.Cancel()
				delete(a.activeForwards, sess.ID)
				a.forwardPicker.Close()
				a.cmdresult.Append(OKStyle.Render("forward cancelled: ") + pfShortID(sess.TaskID) + "  " + sess.Direction.flag() + " " + sess.Spec)
			}
			return a, nil
		}
		// Port-forward modal intercepts ALL keys when open.
		if a.portForwardModal.IsOpen() {
			switch msg.Type {
			case tea.KeyEsc:
				a.portForwardModal.Close()
				return a, nil
			case tea.KeyEnter:
				spec := a.portForwardModal.Spec()
				taskID := a.portForwardModal.TaskID()
				mode := a.portForwardModal.Mode()
				a.portForwardModal.Close()
				if spec == "" {
					a.cmdresult.Append(WarnStyle.Render("forward cancelled (empty spec)"))
					return a, nil
				}
				a.nextForwardID++
				if mode == ForwardRemote {
					return a, DoStartRemoteForward(a.client, taskID, spec, a.nextForwardID, a.program)
				}
				return a, DoStartPortForward(a.client, taskID, spec, a.nextForwardID, a.program)
			}
			var pfcmd tea.Cmd
			a.portForwardModal, pfcmd = a.portForwardModal.Update(msg)
			return a, pfcmd
		}
		// Ctrl+C always quits.
		if msg.Type == tea.KeyCtrlC {
			return a, tea.Quit
		}
		// While the logs panel is in filter-edit mode, every printable rune
		// (including 'q', 's', 'c') belongs to the filter draft, just like
		// in cmdline focus.
		logsEditing := a.focus == focusLogs && a.logs.IsEditingFilter()
		// `q` quits when not in the cmdline / not composing a filter (those
		// must accept literal 'q').
		if a.focus != focusCmdline && !logsEditing && msg.String() == "q" {
			return a, tea.Quit
		}
		// Tab cycles focus.
		switch msg.Type {
		case tea.KeyTab:
			a.cycleFocus(+1)
			return a, nil
		case tea.KeyShiftTab:
			a.cycleFocus(-1)
			return a, nil
		}
		// `s` opens the submit popup when not in cmdline focus / filter edit.
		if a.focus != focusCmdline && !logsEditing && msg.String() == "s" {
			a.popup.SetRepoChoices(uniqueRepoPaths(a.runnersSnapshot), a.defaultRepo)
			a.popup.SetHostChoices(uniqueHostnames(a.runnersSnapshot))
			a.popup.Open()
			return a, nil
		}
		// `i` attaches to a Detached+Detachable task when one is selected in
		// the tasks panel, otherwise opens a new interactive PTY session in
		// the default repo. The RPC + tea.Exec dance is two-stage: the Cmd
		// dispatches the RPC, the response arrives as InteractiveReadyMsg,
		// and Update returns tea.Exec then to actually suspend the TUI.
		// `i` opens a new (non-detachable) interactive PTY in the default repo.
		// Reattach lives on `r` now (see below), so `i` no longer special-cases a
		// selected Detached task.
		if a.focus != focusCmdline && !logsEditing && msg.String() == "i" {
			return a, DoOpenInteractive(a.client, a.defaultRepo)
		}
		// `S` (capital) opens a new detachable interactive PTY session in the
		// default repo (equivalent to `harness-cli session new`).
		if a.focus != focusCmdline && !logsEditing && msg.String() == "S" {
			return a, DoOpenDetachableSession(a.client, a.defaultRepo, cli.SelectorOpts{}, nil, "")
		}
		// `F` opens the file picker for the task currently focused in the
		// tasks pane. No-op when the tasks pane is not focused or no task
		// is selected (the cmdresult line explains).
		if a.focus != focusCmdline && !logsEditing && msg.String() == "F" {
			if a.focus != focusTasks {
				a.cmdresult.Append(WarnStyle.Render("file picker: focus the tasks pane first"))
				return a, nil
			}
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("file picker: no task selected"))
				return a, nil
			}
			cmd := a.filepicker.OpenFor(a.client, taskID)
			a.filepicker.SetSize(a.width, a.height)
			return a, cmd
		}
		// `d` opens the detail popup for the focused row (runners or tasks).
		if !logsEditing && msg.String() == "d" {
			switch a.focus {
			case focusRunners:
				if r := a.runners.SelectedRunner(); r != nil {
					a.detail.Open("Runner detail", formatRunnerDetail(*r))
				} else {
					a.cmdresult.Append(WarnStyle.Render("no runner selected"))
				}
				return a, nil
			case focusTasks:
				if t := a.tasks.SelectedTask(); t != nil {
					a.detail.Open("Task detail", formatTaskDetail(*t))
				} else {
					a.cmdresult.Append(WarnStyle.Render("no task selected"))
				}
				return a, nil
			}
		}
		// `c` cancels the selected task when tasks panel is focused.
		if a.focus == focusTasks && msg.String() == "c" {
			id := a.tasks.SelectedID()
			if id == "" {
				a.cmdresult.Append(WarnStyle.Render("no task selected"))
				return a, nil
			}
			return a, DoCancel(a.client, id, id)
		}
		// `r` / `R` re-enter the selected session: reattach a live Detached
		// session, or resume a finished task into a new detachable session.
		// r resumes with --continue (keep claude's memory); R resumes fresh.
		if a.focus == focusTasks && (msg.String() == "r" || msg.String() == "R") {
			act := resumeReattachAction(a.tasks.SelectedTask(), msg.String() == "r")
			switch act.Kind {
			case actionReattach:
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_Control)
			case actionResume:
				// repo is irrelevant on resume — the server reuses the task's
				// RepoPath and worktree branch.
				return a, DoOpenDetachableSession(a.client, "", cli.SelectorOpts{}, act.ResumeArgs, a.tasks.SelectedID())
			case actionNone:
				a.cmdresult.Append(WarnStyle.Render(act.Hint))
				return a, nil
			}
		}
		// `v` view-attaches the selected live session in read-only mode (no input sent).
		if a.focus == focusTasks && msg.String() == "v" {
			act := resumeReattachAction(a.tasks.SelectedTask(), true)
			if act.Kind == actionReattach {
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_View)
			}
		}
		// `p` / `b` open the local / remote port-forward modal for the selected task.
		if a.focus == focusTasks && (msg.String() == "p" || msg.String() == "b") {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
				return a, nil
			}
			dir := ForwardLocal
			if msg.String() == "b" {
				dir = ForwardRemote
			}
			a.portForwardModal.OpenMode(taskID, dir)
			return a, nil
		}
		// `P` / `B` stop a local / remote forward for the selected task. With more
		// than one active, a digit picker is shown; with exactly one, cancel now.
		if a.focus == focusTasks && (msg.String() == "P" || msg.String() == "B") {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
				return a, nil
			}
			dir := ForwardLocal
			if msg.String() == "B" {
				dir = ForwardRemote
			}
			sel := selectForwards(a.activeForwards, taskID, dir)
			switch len(sel) {
			case 0:
				a.cmdresult.Append(WarnStyle.Render("forward: no active " + dir.flag() + " forward for selected task"))
			case 1:
				sel[0].Cancel()
				delete(a.activeForwards, sel[0].ID)
				a.cmdresult.Append(OKStyle.Render("forward cancelled: ") + pfShortID(taskID) + "  " + dir.flag() + " " + sel[0].Spec)
			default:
				a.forwardPicker.Open(dir, sel)
			}
			return a, nil
		}
		// Cmdline submit.
		if a.focus == focusCmdline && msg.Type == tea.KeyEnter {
			input := a.cmdline.Value()
			a.cmdline.SetValue("")
			act, err := ParseCommand(input, a.defaultRepo)
			if err != nil {
				a.cmdresult.Append(ErrorStyle.Render("error: " + err.Error()))
				return a, nil
			}
			if act == nil {
				return a, nil
			}
			a.cmdresult.Append("> " + input)
			return a.runAction(act)
		}
		// Follow task on Enter when tasks panel is focused.
		if a.focus == focusTasks && msg.Type == tea.KeyEnter {
			id := a.tasks.SelectedID()
			if id != "" {
				return a, a.followTask(id)
			}
			return a, nil
		}
	}

	// Forward to focused panel.
	var cmd tea.Cmd
	switch a.focus {
	case focusRunners:
		a.runners, cmd = a.runners.Update(msg)
	case focusTasks:
		a.tasks, cmd = a.tasks.Update(msg)
	case focusLogs:
		a.logs, cmd = a.logs.Update(msg)
	case focusNotify:
		a.notify, cmd = a.notify.Update(msg)
	case focusCmdresult:
		a.cmdresult, cmd = a.cmdresult.Update(msg)
	case focusCmdline:
		a.cmdline, cmd = a.cmdline.Update(msg)
	}
	return a, cmd
}

func (a *App) cycleFocus(delta int) {
	a.runners.Blur()
	a.tasks.Blur()
	a.logs.Blur()
	a.notify.Blur()
	a.cmdresult.Blur()
	a.cmdline.Blur()

	a.focus = focus((int(a.focus) + delta + numFocus) % numFocus)

	switch a.focus {
	case focusRunners:
		a.runners.Focus()
	case focusTasks:
		a.tasks.Focus()
	case focusLogs:
		a.logs.Focus()
	case focusNotify:
		a.notify.Focus()
	case focusCmdresult:
		a.cmdresult.Focus()
	case focusCmdline:
		a.cmdline.Focus()
	}
}

// layout computes per-panel sizes from a.width / a.height. Header 1, runners
// + tasks border-inclusive 12, notify border-inclusive 6, cmdresult
// border-inclusive 7, cmdline 1, footer 1 = 28 fixed non-log rows, plus the
// log panel's own 2 border rows = 30 reserved. Log content gets the rest
// (min 5); logHeight refers to the inner content height of the log panel.
func (a *App) layout() {
	if a.width < 80 || a.height < 24 {
		return
	}
	half := a.width / 2
	a.runners.SetSize(half-2, 10)
	a.tasks.SetSize(a.width-half-2, 10)
	a.notify.SetSize(a.width-2, 4)
	a.cmdresult.SetSize(a.width-2, 5)
	a.cmdline.Width = a.width - 4
}

func (a *App) View() string {
	if a.width < 80 || a.height < 24 {
		return "terminal too small (need at least 80x24)"
	}

	connectedTag := ErrorStyle.Render("DISCONNECTED")
	if a.connected {
		connectedTag = OKStyle.Render("CONNECTED")
	}
	header := HeaderStyle.Render(fmt.Sprintf("harness-tui · %s · %s", a.server, connectedTag))

	runnersView := a.runners.View()
	tasksView := a.tasks.View()
	if a.runners.IsFocused() {
		runnersView = PanelStyleFocused.Render(runnersView)
	} else {
		runnersView = PanelStyle.Render(runnersView)
	}
	if a.tasks.IsFocused() {
		tasksView = PanelStyleFocused.Render(tasksView)
	} else {
		tasksView = PanelStyle.Render(tasksView)
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, runnersView, tasksView)

	logHeight := max(a.height-30, 5)
	a.logs.SetSize(a.width-4, logHeight-2) // -2 for the panel border rows
	logBorder := PanelStyle
	if a.logs.IsFocused() {
		logBorder = PanelStyleFocused
	}
	logView := logBorder.
		Width(a.width - 2).
		Height(logHeight).
		Render(a.logs.View())

	notifyBorder := PanelStyle
	if a.notify.IsFocused() {
		notifyBorder = PanelStyleFocused
	}
	notifyView := notifyBorder.Width(a.width - 2).Render(a.notify.View())

	cmdresultBorder := PanelStyle
	if a.cmdresult.IsFocused() {
		cmdresultBorder = PanelStyleFocused
	}
	cmdresultView := cmdresultBorder.Width(a.width - 2).Render(a.cmdresult.View())
	cmdlineView := a.cmdline.View()
	var hint string
	switch {
	case a.logs.IsEditingFilter():
		hint = "/" + a.logs.FilterDraft() + "_   (enter apply · esc cancel)"
	case a.logs.Filter() != "":
		hint = "[filter: " + a.logs.Filter() + "]   tab focus · / edit · esc clear · q quit"
	default:
		hint = "tab focus · ←/→ scroll · / filter · s submit · S session · i interactive · r reattach/resume · R resume-fresh · v view-only · F file picker · d detail · c cancel · p/P L-forward · b/B R-forward · q quit"
	}
	footer := FooterStyle.Render(hint)

	view := strings.Join([]string{
		header,
		top,
		logView,
		notifyView,
		cmdresultView,
		cmdlineView,
		footer,
	}, "\n")
	if a.filepicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.filepicker.View())
	}
	if a.popup.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.popup.View())
	}
	if a.detail.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.detail.View())
	}
	if a.portForwardModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.portForwardModal.View())
	}
	if a.forwardPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.forwardPicker.View())
	}
	return view
}

// FollowingTaskID returns the task id whose log is being streamed into the
// log pane, or "" if no task is followed. Used by the persist-loop wiring
// to re-issue SubscribeTaskLog after a reconnect.
func (a *App) FollowingTaskID() string { return a.logs.TaskID() }

// followTask LEAVEs the previous log subscription (if any), kicks off both a
// historical fetch (GetTaskLog) and a live subscribe (task.<taskID>.log).
// History arrives via LogHistoryMsg and is Prepend'd; live chunks arrive via
// LogChunkMsg and are Append'd. For Done tasks the live subscription yields
// nothing — the user still sees the persisted log file.
func (a *App) followTask(taskID string) tea.Cmd {
	if a.logsCancel != nil {
		a.logsCancel()
		a.logsCancel = nil
	}
	a.logs.Reset(taskID)
	if taskID == "" || a.client == nil || a.program == nil || a.appCtx == nil {
		return nil
	}
	subCtx, cancel := context.WithCancel(a.appCtx)
	a.logsCancel = cancel
	return tea.Batch(
		DoGetTaskLog(a.client, taskID),
		func() tea.Msg {
			go SubscribeTaskLog(subCtx, a.client, a.program, taskID)
			return nil
		},
	)
}

// refreshTasksTable rebuilds the tasks table from tasksByID, sorted by
// descending CreatedAt, capped at 100 rows.
func (a *App) refreshTasksTable() {
	all := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	for _, t := range a.tasksByID {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > 100 {
		all = all[:100]
	}
	a.tasks.SetRows(all, a.runnersSnapshot)
}

// resolveTaskIDPrefix returns the full hex id matching prefix (case-insensitive).
// Returns ("", reason) if zero or multiple matches.
func (a *App) resolveTaskIDPrefix(prefix string) (string, string) {
	p := strings.ToLower(prefix)
	var matches []string
	for id := range a.tasksByID {
		if strings.HasPrefix(id, p) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", "no task matches " + prefix
	case 1:
		return matches[0], ""
	default:
		return "", fmt.Sprintf("ambiguous prefix %q matches %d tasks", prefix, len(matches))
	}
}

// runAction dispatches a parsed cmdline Action.
func (a *App) runAction(act Action) (tea.Model, tea.Cmd) {
	switch v := act.(type) {
	case QuitAction:
		return a, tea.Quit
	case ClearAction:
		a.cmdresult.Clear()
		return a, nil
	case HelpAction:
		a.cmdresult.Append("commands: submit / interactive [--repo=PATH] / cancel <id> / notify <text> / prune [--before=DUR] / repo <path> / clear / help / quit")
		a.cmdresult.Append("notify [info|warn|error] <title> [<text>...]        - send a notification (shows in this feed + --notify-hook egress; keep it one line)")
		a.cmdresult.Append("session new [--detach] [--host NAME | --runner HEX | --ip ADDR] - open detachable interactive session (--detach: background, print id)")
		a.cmdresult.Append("session attach <id>         - reattach to a session")
		a.cmdresult.Append("session ls                  - list detachable sessions")
		a.cmdresult.Append("session kill <id>           - terminate a session")
		a.cmdresult.Append("file ls <task-id> [<rel>]                          - list a directory in the task's worktree (root if rel omitted)")
		a.cmdresult.Append("file push [-r] [-f] <task-id> <local-src> <rel-dst>  - copy a local file/dir into the worktree (-r tar, -f overwrite)")
		a.cmdresult.Append("file pull [-r] [-f] <task-id> <rel-src> <local-dst>  - copy from the worktree to a local path")
		a.cmdresult.Append("file delete [-r [-f]] <task-id> <rel>              - remove a file (no -r) or directory (-r empty / -r -f recursive)")
		a.cmdresult.Append("server dial-runner <runner-cid>                    - ask the server to reverse-dial a Listen-mode runner (Phase A, ACL envs)")
		a.cmdresult.Append("F (tasks focus): open file picker — Enter/→ to descend a dir, Backspace/← to go back. u push / g pull / d delete / D rm -rf. Esc closes.")
		a.cmdresult.Append("  picker push/pull input — Tab toggles local fs browser. Tab back to typing pre-fills the selected file's path; Enter commits.")
		a.cmdresult.Append("  push/pull overwrite — first try fails on existing dest; picker prompts overwrite? (y/n). y retries with force=true.")
		a.cmdresult.Append("trsf                        - dump the client↔server transport's internal state (debug)")
		return a, nil
	case TrsfDebugAction:
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("trsf: not connected"))
			return a, nil
		}
		st := a.client.Transport().GetInternalState()
		if st == nil {
			a.cmdresult.Append(WarnStyle.Render("trsf: no internal state"))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render("trsf internal state (client↔server):"))
		a.cmdresult.Append(fmt.Sprintf("  streams: send=%d recv=%d   mtu=%d", st.ActiveSendStreams, st.ActiveReceiveStreams, st.CurrentMTU))
		a.cmdresult.Append(fmt.Sprintf("  queues: send=%d recv=%d   triggers: sendAction=%d updateWin=%d cancel=%d",
			st.SendQueueLength, st.ReceiveQueueLength, st.SendActionCount, st.UpdateWindowCount, st.CancelStreamCount))
		a.cmdresult.Append(fmt.Sprintf("  cc: inflight=%dB cwnd=%dB rtt=%v (var %v) sentPkts=%d",
			st.BytesInFlight, st.CongestionWindow, st.SmoothedRTT, st.RTTVariance, len(st.SentPackets)))
		return a, nil
	case RepoAction:
		// The repo string is treated as an opaque identifier — server
		// matches it byte-for-byte against runner-registered AllowedRoots.
		// We cannot filepath.Abs() here because the TUI host and runner
		// host may have different OSes (e.g. Windows TUI + Linux runner),
		// where local Abs would mangle a valid runner path into a
		// meaningless drive-prefixed one.
		path := v.Path
		hasRunner := false
	outer:
		for _, r := range a.runnersSnapshot {
			for _, root := range r.AllowedRoots {
				if string(root.Path) == path {
					hasRunner = true
					break outer
				}
			}
		}
		if !hasRunner {
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("repo: no runner currently registered for %s — submit/interactive will fail with NoRunnerForRepo until one connects", path)))
		}
		a.defaultRepo = path
		a.cmdresult.Append(fmt.Sprintf("default repo set to %s", path))
		return a, nil
	case InteractiveAction:
		return a, DoOpenInteractiveWithOpts(a.client, v.Repo, "", v.ExtraArgs, v.ResumeTaskID)
	case SubmitAction:
		return a, DoSubmitWithOpts(a.client, v.Repo, v.Prompt, "", v.ExtraArgs, v.ResumeTaskID)
	case CancelAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoCancel(a.client, v.IDPrefix, full)
	case PruneAction:
		a.cmdresult.Append(fmt.Sprintf("prune: cutoff = %s; asking server to forget terminal tasks", cli.FormatPruneCutoff(v.Before)))
		return a, DoPruneTasks(a.client, v.Before)
	case SessionNewAction:
		repo := v.Repo
		if repo == "" {
			repo = a.defaultRepo
		}
		sel := cli.SelectorOpts{Host: v.Host, Runner: v.Runner, IP: v.IP}
		if v.X11 {
			return a, DoOpenX11Session(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, v.X11Display, a.program)
		}
		if v.Detach {
			return a, DoStartDetachedSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID)
		}
		return a, DoOpenDetachableSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID)
	case SessionAttachAction:
		return a, DoAttachSession(a.client, v.TaskID, protocol.AttachMode_Control)
	case SessionLsAction:
		return a, DoSessionList(a.client)
	case SessionKillAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoCancel(a.client, v.IDPrefix, full)
	case FileLsAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFileLs(a.client, full, v.RelPath)
	case FilePushAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFilePush(a.client, full, v.LocalSrc, v.RemoteDst, v.Recursive, v.Force)
	case FilePullAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFilePull(a.client, full, v.RemoteSrc, v.LocalDst, v.Recursive, v.Force)
	case FileDeleteAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFileDelete(a.client, full, v.RelPath, v.Recursive, v.Force)
	case ServerDialRunnerAction:
		if a.client == nil {
			a.cmdresult.Append(ErrorStyle.Render("server dial-runner: not connected to server"))
			return a, nil
		}
		return a, DoServerDialRunner(a.client, v.RunnerCID, v.Via)
	case NotifyAction:
		if a.client == nil {
			a.cmdresult.Append(ErrorStyle.Render("notify: not connected to server"))
			return a, nil
		}
		return a, DoNotify(a.client, v.Level, v.Title, v.Text)
	}
	a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("(unhandled action %T)", act)))
	return a, nil
}

// uniqueRepoPaths returns the de-duplicated list of allowed-root paths from a
// runner snapshot, in stable (sorted) order — used to populate the submit
// popup's repo selector.
func uniqueRepoPaths(rs []protocol.RunnerInfo) []string {
	seen := make(map[string]struct{}, len(rs))
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		for _, root := range r.AllowedRoots {
			p := string(root.Path)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// uniqueHostnames returns the de-duplicated list of runner hostnames from a
// snapshot, in stable (sorted) order — used to populate the submit popup's
// optional host-pin selector.
func uniqueHostnames(rs []protocol.RunnerInfo) []string {
	seen := make(map[string]struct{}, len(rs))
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		h := string(r.Hostname)
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
