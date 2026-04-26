package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	focusCmdline
)

// App is the top-level Bubble Tea Model.
type App struct {
	server      string
	defaultRepo string

	runners   RunnersModel
	tasks     TasksModel
	logs      LogsModel
	cmdresult CmdResultModel
	cmdline   textinput.Model
	popup     PopupModel

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

	// log-following state
	logsCancel context.CancelFunc
	client     *cli.Client
	appCtx     context.Context
	program    *tea.Program
}

type Config struct {
	Server      string
	DefaultRepo string
}

func New(cfg Config) *App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = "submit / cancel / prune / clear / help / quit"
	cmd.CharLimit = 1024
	cmd.Width = 60
	a := &App{
		server:      cfg.Server,
		defaultRepo: cfg.DefaultRepo,
		runners:     NewRunners(),
		tasks:       NewTasks(),
		logs:        NewLogs(),
		cmdresult:   NewCmdResult(),
		cmdline:     cmd,
		popup:       NewPopup(cfg.DefaultRepo),
		focus:       focusTasks,
		connected:   false,
		status:      "connecting…",
		tasksByID:   map[string]protocol.TaskInfo{},
	}
	a.tasks.Focus()
	return a
}

// BindContext stores the application-level context for spawning per-task subscriptions.
func (a *App) BindContext(ctx context.Context) { a.appCtx = ctx }

// BindClient stores the persistent cli.Client used for both pubsub
// subscriptions and TaskControl RPCs. Called by main.go after cli.Dial succeeds.
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
		cur, ok := a.tasksByID[id]
		if !ok {
			var ti protocol.TaskInfo
			ti.Id = msg.Event.TaskId
			ti.Status = msg.Event.TaskStatus
			ti.Kind = msg.Event.TaskKind
			ti.CreatedAt = msg.Event.Ts
			a.tasksByID[id] = ti
		} else {
			cur.Status = msg.Event.TaskStatus
			if msg.Event.Kind == protocol.StatusEventKind_TaskEnded {
				cur.ExitCode = msg.Event.ExitCode
				cur.EndedAt = msg.Event.Ts
			}
			a.tasksByID[id] = cur
		}
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

	case ConnectionMsg:
		a.connected = msg.Connected
		if !msg.Connected && msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("disconnected: " + msg.Err.Error()))
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
		return a, nil

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

	case InteractiveReadyMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("open interactive failed: " + msg.Err.Error()))
			return a, nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("attaching ") + short + " — Ctrl+D / `exit` to detach")
		return a, tea.Exec(&interactiveExec{stream: msg.Stream}, func(err error) tea.Msg {
			return InteractiveDoneMsg{TaskID: msg.TaskID, Err: err}
		})

	case InteractiveDoneMsg:
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

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.layout()
		return a, nil

	case tea.KeyMsg:
		// Popup intercepts ALL keys when open.
		if a.popup.IsOpen() {
			switch msg.Type {
			case tea.KeyEsc:
				a.popup.Close()
				return a, nil
			case tea.KeyCtrlJ:
				// Bubbletea reports Ctrl+Enter as Ctrl+J on most terminals.
				repo := a.popup.Repo()
				prompt := a.popup.Prompt()
				a.popup.Close()
				if prompt == "" {
					a.cmdresult.Append(WarnStyle.Render("submit cancelled (empty prompt)"))
					return a, nil
				}
				return a, DoSubmit(a.client, repo, prompt)
			}
			var pcmd tea.Cmd
			a.popup, pcmd = a.popup.Update(msg)
			return a, pcmd
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
			a.popup.SetRepo(a.defaultRepo)
			a.popup.Open()
			return a, nil
		}
		// `i` opens an interactive PTY claude session in the default repo.
		// The RPC + tea.Exec dance is two-stage: the Cmd dispatches the RPC,
		// the response arrives as InteractiveReadyMsg, and Update returns
		// tea.Exec then to actually suspend the program and run the shell.
		if a.focus != focusCmdline && !logsEditing && msg.String() == "i" {
			return a, DoOpenInteractive(a.client, a.defaultRepo)
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
	case focusCmdline:
		a.cmdline, cmd = a.cmdline.Update(msg)
	}
	return a, cmd
}

func (a *App) cycleFocus(delta int) {
	a.runners.Blur()
	a.tasks.Blur()
	a.logs.Blur()
	a.cmdline.Blur()

	a.focus = focus((int(a.focus) + delta + 4) % 4)

	switch a.focus {
	case focusRunners:
		a.runners.Focus()
	case focusTasks:
		a.tasks.Focus()
	case focusLogs:
		a.logs.Focus()
	case focusCmdline:
		a.cmdline.Focus()
	}
}

// layout computes per-panel sizes from a.width / a.height. Header 1, runners
// + tasks 10 each, cmdresult 5, cmdline 1, footer 1, plus 4 border rows
// distributed across panels = 22 reserved. Log gets the rest (min 5).
func (a *App) layout() {
	if a.width < 80 || a.height < 24 {
		return
	}
	half := a.width / 2
	a.runners.SetSize(half-2, 10)
	a.tasks.SetSize(a.width-half-2, 10)
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

	logHeight := a.height - 22
	if logHeight < 5 {
		logHeight = 5
	}
	a.logs.SetSize(a.width-4, logHeight-2) // -2 for the panel border rows
	logBorder := PanelStyle
	if a.logs.IsFocused() {
		logBorder = PanelStyleFocused
	}
	logView := logBorder.
		Width(a.width - 2).
		Height(logHeight).
		Render(a.logs.View())

	cmdresultView := PanelStyle.Width(a.width - 2).Render(a.cmdresult.View())
	cmdlineView := a.cmdline.View()
	var hint string
	switch {
	case a.logs.IsEditingFilter():
		hint = "/" + a.logs.FilterDraft() + "_   (enter apply · esc cancel)"
	case a.logs.Filter() != "":
		hint = "[filter: " + a.logs.Filter() + "]   tab focus · / edit · esc clear · q quit"
	default:
		hint = "tab focus · ←/→ scroll · shift+←/→ page · 0/$ edge · / filter · s submit · i interactive · c cancel · q quit"
	}
	footer := FooterStyle.Render(hint)

	view := strings.Join([]string{
		header,
		top,
		logView,
		cmdresultView,
		cmdlineView,
		footer,
	}, "\n")
	if a.popup.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.popup.View())
	}
	return view
}

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
	a.tasks.SetRows(all)
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
		a.cmdresult.Append("commands: submit / cancel <id> / prune [--before=DUR] [--offline] / clear / help / quit")
		return a, nil
	case SubmitAction:
		return a, DoSubmit(a.client, v.Repo, v.Prompt)
	case CancelAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoCancel(a.client, v.IDPrefix, full)
	case PruneAction:
		if v.Offline {
			a.cmdresult.Append(WarnStyle.Render("--offline is a CLI-only flag; use harness-cli prune --offline. Server-side prune skipped."))
			return a, nil
		}
		return a, DoPruneTasks(a.client, v.Before)
	}
	a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("(unhandled action %T)", act)))
	return a, nil
}
