package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
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
	cmdHistory []string
	// cmdHistoryIndex is -1 while editing a fresh command; otherwise it points
	// at cmdHistory while Up/Down is browsing prior entries.
	cmdHistoryIndex int
	cmdHistoryDraft string
	popup           PopupModel
	detail          DetailPopup
	filepicker      FilePickerModel
	fileEditor      FileEditModel
	// One-shot: set when a commit came back FileEditConflict, consumed by the
	// next save so a second ctrl+j overwrites deliberately rather than by
	// default.
	fileEditForce bool
	// Temp file held across an external-editor handover.
	fileEditTmpPath string

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
	// actRecvAt records the LOCAL receipt time of each task's act fields
	// (last_output_at / output_idle_ms), whether they arrived via snapshot or
	// task event. Rendering ages an idle badge by the local elapsed time
	// since receipt (clock-skew-free: wire idle age + local duration); a busy
	// badge is never aged — it flips only when the server's idle-edge
	// task_activity event arrives.
	actRecvAt map[string]time.Time
	// runnersSnapshot holds the latest runners from the most recent snapshot.
	runnersSnapshot []protocol.RunnerInfo

	// connections view
	connsModal    ConnsModal
	forwardsModal ForwardsModal
	// forwardTap is the live traffic view for one forward. Its pump is stopped
	// through forwardTapStop, which is nil whenever no tap is running.
	forwardTap     ForwardTapView
	forwardTapStop context.CancelFunc
	execsModal     ExecsModal

	// live session viewer grid (full-screen overlay, `g` key)
	grid GridModel
	chat ChatModel

	// agentboard view
	boardModal BoardModal

	// read-only git view of a task's worktree
	gitModal GitModal
	// gitStatusToContent routes the NEXT status answer into the content
	// pane instead of only the worktree-row summary. Set when the operator
	// asks for the listing; a background refresh leaves it false.
	gitStatusToContent bool

	// workspace is the installed .harness/config workspace, or nil.
	// workspaceArmed means an apply is due on the next snapshot: the apply
	// needs task STATUSES to decide what to resume, and the snapshot is where
	// those arrive.
	workspace         *workspace.Workspace
	workspaceArmed    bool
	workspaceFile     *workspace.File
	workspacePath     string
	workspaceSaveName string // non-empty while a `workspace save` waits for the forward snapshot
	workspaceSaveAll  bool   // that save was `--all`: write without opening the picker
	// workspacePicker chooses WHICH tasks a save records, and their resume /
	// runner. See its doc comment for why no automatic rule replaced it.
	workspacePicker WorkspacePickerModel
	// workspaceRetryOnRunner arms ONE re-apply for the next runner event, set
	// when a workspace resume failed. See the SessionStartedMsg handler.
	workspaceRetryOnRunner bool

	// gridSel is the selection the open grid was built from. GridModel keeps
	// only cli.GridSet's display LABEL ("<id8>+desc"), which truncates the
	// anchor id and cannot be turned back into `--under <id>` — so `workspace
	// save` records the selection here instead, at openGrid, the one entry
	// point every grid path goes through.
	gridSelMode   cli.GridScopeMode
	gridSelAnchor string
	gridSelIDs    []string
	// gridSelSet records that a grid was opened at least once this session, so
	// a save can offer that selection AFTER the grid is closed. It has to: the
	// grid is a full-screen overlay that intercepts every key, so the command
	// line is unreachable while it is open and `a.grid.IsOpen()` is ALWAYS
	// false by the time a `workspace save` runs. Gating on it made the grid
	// unsavable in principle rather than merely unsaved.
	gridSelSet bool

	// ssh gateway: at most one per TUI. Deliberately NOT in activeForwards —
	// that map is keyed per forward session and drives the task-scoped P/B stop
	// keys and workspace capture, none of which apply to a listener that serves
	// every task and belongs to none.
	sshGateway *SSHGatewaySession
	// gatewayRestartTo parks a workspace apply's restart address while the
	// previous listener is still releasing its port. Cancel() only signals;
	// the SSHGatewayStoppedMsg handler is where the port is actually free, so
	// that is where the new one starts. See applyWorkspaceGateway.
	gatewayRestartTo string
	configPath       string

	// port-forward state
	portForwardModal PortForwardModal
	forwardPicker    ForwardPicker
	activeForwards   map[int]*PortForwardSession // keyed by client-side unique id
	nextForwardID    int

	// rawModal is the third way to start a forward (`t`), alongside p/-L and
	// b/-R: a live host:port connection whose client-side endpoint is this
	// TUI process, not a socket. It does not join activeForwards — there is
	// no local listener/dialer session object to track — so P/B (stop the
	// selected task's -L/-R forward) do not apply to it. It is still listed
	// and killable via `f` / `forward kill`, which act on the server-side
	// registration that OpenRawForward creates.
	rawModal RawConnectModal
	// rawGenSeq allocates the generation each pane is tagged with. It is the
	// ALLOCATOR, not the guard: the guard is "which pane owns this gen", so a
	// reply from a pump still parked in rc.Recv when its pane was closed finds
	// no owner and is dropped instead of splicing its trailing bytes onto
	// whichever pane is selected now.
	rawGenSeq uint64

	// authorityPicker is the selection UI for a task re-grant (tasks-pane
	// `a`) and the session-default authority (`caps` / `scope`, no args).
	authorityPicker AuthorityPickerModel

	// runnerPicker shows candidate runners when an interactive open returns
	// AmbiguousRunner; a pick re-issues the request pinned to the chosen cid.
	runnerPicker RunnerPickerModel
	// pendingInteractive holds the params of the in-flight interactive open so
	// the picker can re-issue it pinned to a chosen runner.
	pendingInteractive pendingInteractive
	// pickerArmed is true only while an in-flight interactive open was
	// dispatched from one of the two sites that also populate
	// pendingInteractive (the `S` key and the resume cases of `r`/`R`/`u`/`U`).
	// InteractiveReadyMsg's handler checks-and-clears this so an AmbiguousRunner
	// from any OTHER interactive-open path (`i`, InteractiveAction,
	// SessionNewAction, X11) falls back to a flat error instead of opening the
	// picker with stale or zero pendingInteractive. See the S/resume scope note on
	// the InteractiveReadyMsg case below.
	pickerArmed bool

	// log-following state
	logsCancel context.CancelFunc
	client     *cli.Client
	appCtx     context.Context
	program    *tea.Program
	// termReleased is true while a child (an attached session, the external
	// editor) owns the terminal via execWithoutSuspend. Written only on the
	// Update goroutine: set before returning the Cmd, cleared by that Cmd's
	// done message. Gates every further handover, since ReleaseTerminal /
	// RestoreTerminal do not nest.
	termReleased bool
	// logsGen increments on every followTask. It stamps each GetTaskLog so a
	// response from a superseded fetch can be discarded (see LogHistoryMsg).
	logsGen int

	// x11Cancel stops the background -R forward of the current X11 interactive
	// session. Set when InteractiveReadyMsg carries one; called and cleared on
	// InteractiveDoneMsg so the forward stops with the session.
	x11Cancel context.CancelFunc

	// sessionCaps is the default capability mask applied to every spawn
	// (submit / interactive / session new) issued from this TUI session.
	// Controlled by the `caps` command; defaults to Capability_None.
	sessionCaps protocol.Capability
	// sessionScope is the default target scope applied to every spawn, the
	// companion to sessionCaps: caps say which verbs, scope says which tasks
	// they may be pointed at. Zero value is subtree (self + descendants).
	sessionScope protocol.TaskScope
	// sessionOverrides narrows individual capabilities below sessionScope. It
	// travels with sessionScope as one half of the default authority: the
	// `scope` command sets and clears both together, so a spawn never carries
	// a scope from one command and overrides from another.
	sessionOverrides []protocol.ScopeOverride
}

// resolveSpawnCaps picks the capability mask for one spawn and says whether the
// server should re-grant it on a resume.
//
// An explicit --caps wins over the session default. On a resume it also implies
// the override, because the server otherwise keeps the resumed task's persisted
// caps and the mask the operator just typed would be discarded without a word —
// a flag whose value has no reachable effect is worse than either behaviour.
// The session default never overrides on resume, so an unqualified resume still
// cannot silently widen or narrow a task.
func (a App) resolveSpawnCaps(explicit *protocol.Capability, resuming bool) (protocol.Capability, bool) {
	if explicit == nil {
		return a.sessionCaps, false
	}
	return *explicit, resuming
}

// spawnAuthority is resolveSpawnCaps' target-set half, folded into the
// Authority the Do* helpers carry: an explicit --scope wins over the session
// default, and on a resume ONLY an explicit --scope re-grants (ScopePresent)
// — the session default must never silently rewrite a resumed task's scope.
func (a App) spawnAuthority(explicit *protocol.TaskScope, overrides []protocol.ScopeOverride, resumeTaskID string, caps protocol.Capability) Authority {
	auth := Authority{Caps: caps, Scope: a.sessionScope, Overrides: a.sessionOverrides}
	// The scope half is one unit: naming EITHER --scope or --scope-for makes
	// the whole half explicit, so a resume re-grants both together. Letting
	// --scope-for alone ride the session default's scope would write an
	// authority that is half typed and half inherited.
	if explicit != nil || len(overrides) > 0 {
		if explicit != nil {
			auth.Scope = *explicit
		}
		auth.Overrides = overrides
		auth.ScopePresent = resumeTaskID != ""
	}
	return auth
}

// pendingInteractive captures what an interactive open needs so a runner-picker
// selection can re-issue it. repo is "" for resume (server reuses the task's
// repo); resumeTaskID is "" for a fresh session.
// authority is the session default a spawn carries when the command line did
// not name its own: the caps mask paired with the target scope.
func (a *App) authority() Authority {
	return Authority{Caps: a.sessionCaps, Scope: a.sessionScope, Overrides: a.sessionOverrides}
}

type pendingInteractive struct {
	repo               string
	resumeTaskID       string
	extraArgs          []string
	auth               Authority
	capsOverride       bool
	resumeConversation bool
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
	// WorkspaceFile / WorkspacePath are the parsed .harness/config and where it
	// came from; both nil/empty when no config applies. WorkspaceName is the
	// workspace to install, "" for none — an unknown name is rejected by the
	// caller, which can still print to stderr.
	WorkspaceFile *workspace.File
	WorkspacePath string
	WorkspaceName string
	// ConfigPath is the --config value verbatim ("" when not given), NOT the
	// path WorkspacePath reports. The ssh gateway derives its host-key location
	// from it, and that location is needed even when no config file exists —
	// which is exactly when WorkspacePath is empty.
	ConfigPath string
}

func New(cfg Config) *App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = cmdlinePlaceholder
	cmd.CharLimit = 1024
	cmd.Width = 60
	a := &App{
		server:          cfg.Server,
		defaultRepo:     cfg.DefaultRepo,
		workspaceFile:   cfg.WorkspaceFile,
		workspacePath:   cfg.WorkspacePath,
		configPath:      cfg.ConfigPath,
		runners:         NewRunners(),
		tasks:           NewTasks(),
		detail:          NewDetailPopup(),
		logs:            NewLogs(),
		notify:          NewNotify(),
		cmdresult:       NewCmdResult(),
		cmdline:         cmd,
		cmdHistoryIndex: -1,
		popup:           NewPopup(cfg.DefaultRepo),
		filepicker:      NewFilePicker(),
		fileEditor:      NewFileEdit(),
		connsModal:      NewConnsModal(),
		forwardsModal:   NewForwardsModal(),
		execsModal:      NewExecsModal(),
		rawModal:        NewRawConnectModal(),
		boardModal:      NewBoardModal(),
		gitModal:        NewGitModal(),
		grid:            NewGridModel(),
		focus:           focusTasks,
		connected:       false,
		status:          "connecting…",
		tasksByID:       map[string]protocol.TaskInfo{},
		actRecvAt:       map[string]time.Time{},
		activeForwards:  map[int]*PortForwardSession{},
		sessionCaps:     protocol.Capability_None,
		sessionScope:    protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
	}
	a.tasks.Focus()
	if cfg.WorkspaceName != "" {
		if ws, ok := cfg.WorkspaceFile.Workspace(cfg.WorkspaceName); ok {
			a.SetWorkspace(ws)
		}
	}
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

// actAgeTickInterval drives the LOCAL re-render that ages idle badges
// ("idle:4s" → "idle:5s"). Purely cosmetic — no RPC: act data itself arrives
// via task_activity events (server-side busy/idle edge watcher) and snapshot
// refreshes; the tick just advances the displayed age between them.
const actAgeTickInterval = time.Second

// actAgeTickMsg re-arms the aging re-render tick; emitted by actAgeTick.
type actAgeTickMsg struct{}

func actAgeTick() tea.Cmd {
	return tea.Tick(actAgeTickInterval, func(time.Time) tea.Msg { return actAgeTickMsg{} })
}

// hasAgingActRow reports whether any known task currently shows an idle
// badge — the only rows whose rendering changes with wall time. Gates the
// per-tick table rebuild so an all-busy/no-session table costs nothing.
func (a *App) hasAgingActRow() bool {
	for _, t := range a.tasksByID {
		if t.LastOutputAt > 0 && time.Duration(t.OutputIdleMs)*time.Millisecond >= protocol.ActivityBusyThreshold {
			return true
		}
	}
	return false
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, actAgeTick())
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return a.updateKey(msg)
	case tea.WindowSizeMsg:
		return a.updateWindowSize(msg)
	}
	return a.updateResult(msg)
}

// updateResult handles every message that is neither a key nor a resize:
// the results of the Do* commands, the tickers, the pump traffic. A type it
// does not name belongs to whichever pane has focus.
func (a *App) updateResult(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case actAgeTickMsg:
		// Local aging re-render only; always re-arm so the tick survives
		// disconnects. No RPC here — act data arrives via events/snapshots.
		if a.hasAgingActRow() {
			a.refreshTasksTable()
		}
		return a, actAgeTick()

	case gridTickMsg:
		// Repaint pump for the live session viewer grid: re-render
		// unconditionally (v1 has no dirty-flag gating — 10Hz over ≤9 small
		// crops is cheap) and re-arm only while the grid stays open.
		if a.grid.IsOpen() {
			var cmd tea.Cmd
			a.grid, cmd = a.grid.Update(msg)
			return a, cmd
		}
		return a, nil

	case StreamWriteResultMsg:
		// Names the target and what changed, never a bare "ok" — the result
		// convention this repo keeps re-learning.
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("stream %s %s: %v", msg.Verb, shortTaskID(msg.Resolved), msg.Err)))
		} else {
			a.cmdresult.Append(fmt.Sprintf("stream %s %s: sent", msg.Verb, shortTaskID(msg.Resolved)))
		}
		return a, nil

	case chatTickMsg, ChatLineMsg, ChatEndedMsg, chatAttachedMsg:
		// The chat's own traffic: stream lines from its pump goroutine, the
		// attach handoff, and the elapsed-seconds tick. Delivered whether or
		// not the overlay is still open — a pump that has not noticed its
		// cancellation yet keeps sending — so ChatModel drops what is not for
		// the task it currently holds, and a closed chat's Update is a no-op
		// beyond releasing the session.
		var cmd tea.Cmd
		a.chat, cmd = a.chat.Update(msg)
		return a, cmd

	case SnapshotMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("snapshot: " + msg.Err.Error()))
			return a, nil
		}
		a.runnersSnapshot = msg.Runners
		a.runners.SetRows(msg.Runners)
		a.tasksByID = make(map[string]protocol.TaskInfo, len(msg.Tasks))
		now := time.Now()
		a.actRecvAt = make(map[string]time.Time, len(msg.Tasks))
		for _, t := range msg.Tasks {
			id := FormatTaskID(t.Id)
			a.tasksByID[id] = t
			if t.LastOutputAt > 0 {
				a.actRecvAt[id] = now
			}
		}
		a.refreshTasksTable()
		if a.workspaceArmed {
			a.workspaceArmed = false
			return a, a.applyWorkspace()
		}
		return a, nil

	case TaskEventMsg:
		id := FormatTaskID(msg.Event.TaskId)
		if msg.Event.Kind == protocol.StatusEventKind_TaskPruned {
			// Server forgot this task — drop its row immediately instead of
			// waiting for the next incidental snapshot refresh.
			delete(a.tasksByID, id)
			delete(a.actRecvAt, id)
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
			// then kick a snapshot refresh so the remaining fields fill in.
			var ti protocol.TaskInfo
			ti.Id = msg.Event.TaskId
			ti.Status = msg.Event.TaskStatus
			ti.Kind = msg.Event.TaskKind
			ti.CreatedAt = msg.Event.Ts
			a.applyEventAct(&ti, id, msg.Event)
			a.applyEventObservers(&ti, msg.Event)
			a.tasksByID[id] = ti
			a.refreshTasksTable()
			return a, RefreshSnapshot(a.client)
		}
		cur.Status = msg.Event.TaskStatus
		if msg.Event.Kind == protocol.StatusEventKind_TaskEnded {
			cur.ExitCode = msg.Event.ExitCode
			cur.EndedAt = msg.Event.Ts
		}
		a.applyEventAct(&cur, id, msg.Event)
		a.applyEventObservers(&cur, msg.Event)
		a.tasksByID[id] = cur
		a.refreshTasksTable()
		return a, nil

	case RunnerEventMsg:
		// server-side RunnerStatusEvent.RunnerId is a placeholder (not keyable),
		// so we kick a full snapshot refresh on every runner event.
		//
		// A runner appearing is also the event that can make a failed workspace
		// resume succeed, so consume the one armed retry here. Armed only by a
		// failure, never standing: a workspace re-applied on EVERY runner event
		// would reopen the grid overlay under the operator.
		if a.workspaceRetryOnRunner && a.workspace != nil {
			a.workspaceRetryOnRunner = false
			a.ArmWorkspace()
		}
		return a, RefreshSnapshot(a.client)

	case ConnSnapshotMsg:
		if msg.Err != nil {
			a.cmdresult.Append(WarnStyle.Render("conns snapshot: " + msg.Err.Error()))
			return a, nil
		}
		a.connsModal.ApplySnapshot(msg.Conns)
		return a, nil

	case ForwardsSnapshotMsg:
		if msg.Err != nil {
			if a.workspaceSaveName != "" {
				a.cmdresult.Append(ErrorStyle.Render("workspace save: " + msg.Err.Error()))
				a.workspaceSaveName = ""
			}
			a.cmdresult.Append(WarnStyle.Render("forwards snapshot: " + msg.Err.Error()))
			return a, nil
		}
		if a.workspaceSaveName != "" {
			a.finishWorkspaceSave(msg.Forwards)
		}
		a.forwardsModal.ApplySnapshot(msg.Forwards)
		// The text dump belongs only to the `forward ls` cmdline path
		// (ToCmdresult). cmdresult is a 200-line ring that evicts
		// oldest-first (CmdResultModel.Append) — unconditionally dumping a
		// banner + header + one line per forward on every `f` keypress (and
		// every kill-triggered refresh below) could evict an earlier error
		// notice the operator still needs.
		if msg.ToCmdresult {
			for _, line := range cli.PortForwardInfoLines(msg.Forwards) {
				a.cmdresult.Append(line)
			}
		}
		return a, nil

	case ForwardTapLinesMsg:
		// Lines for a tap the operator already closed are dropped rather than
		// appended into whatever view replaced it.
		if a.forwardTap.IsOpen() && a.forwardTap.ForwardID() == msg.ForwardID {
			a.forwardTap.Append(msg.Lines)
		}
		return a, nil
	case ForwardTapEndedMsg:
		if a.forwardTap.IsOpen() && a.forwardTap.ForwardID() == msg.ForwardID {
			if msg.Err != nil {
				a.forwardTap.Append([]string{ErrorStyle.Render("tap ended: " + msg.Err.Error())})
			} else {
				a.forwardTap.Append([]string{"-- tap ended --"})
			}
		}
		// The view stays up so the operator can read what it caught; only the
		// pump is finished.
		a.forwardTapStop = nil
		if msg.Err != nil && !a.forwardTap.IsOpen() {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("forward tap %d: %v", msg.ForwardID, msg.Err)))
		}
		return a, nil
	case ForwardStatusMsg:
		a.forwardsModal.ApplyEvent(msg.Event)
		return a, nil
	case ExecStatusMsg:
		a.execsModal.ApplyEvent(msg.Event)
		return a, nil
	case ForwardKillResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("forward kill %d: %v", msg.ID, msg.Err)))
			return a, nil
		}
		line := fmt.Sprintf("forward %d killed", msg.ID)
		if msg.TaskID != "" {
			line = fmt.Sprintf("forward %d killed: %s  %s", msg.ID, pfShortID(msg.TaskID), msg.Spec)
		}
		a.cmdresult.Append(OKStyle.Render(line))
		if a.forwardsModal.IsOpen() {
			return a, DoListForwards(a.client, false)
		}
		return a, nil

	case ExecRunOutputMsg:
		a.cmdresult.Append(execOutputLine(msg.Line, msg.Stderr))
		return a, nil

	case ExecRunDoneMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("exec %s: %v", pfShortID(msg.TaskID), msg.Err)))
			return a, nil
		}
		// Reported, never swallowed: a panel that quietly showed fewer lines
		// than the command printed would misrepresent what ran.
		if msg.Dropped > 0 {
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf(
				"exec: %d output line(s) dropped — the UI could not keep up", msg.Dropped)))
		}
		a.cmdresult.Append(execResultLine(msg.TaskID, msg.Argv, msg.Result))
		return a, nil

	case ExecRunListMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("exec ls: %v", msg.Err)))
			return a, nil
		}
		a.execsModal.ApplySnapshot(msg.Execs)
		// The text dump belongs only to the `exec ls` cmdline path. See
		// ExecRunListMsg.ToCmdresult for why the modal refresh must not write
		// into a ring the operator is also reading errors from.
		if msg.ToCmdresult {
			for _, line := range cli.ExecRunInfoLines(msg.Execs) {
				a.cmdresult.Append(line)
			}
		}
		return a, nil

	case ExecRunKillMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("exec kill %d: %v", msg.ExecID, msg.Err)))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("killed exec %d", msg.ExecID)))
		// A killed row must leave the list, and only a fresh fetch can say so
		// — execs have no push subscription. Silent (ToCmdresult false): the
		// "killed" line above is the report, and a listing after it would be a
		// second one nobody asked for.
		if a.execsModal.IsOpen() {
			return a, DoExecRunList(a.client, "", false)
		}
		return a, nil

	case ConnStatusMsg:
		a.connsModal.ApplyEvent(msg.Event)
		return a, nil

	case GitResultMsg:
		// An answer for a task the operator has already navigated away from is
		// stale, not an error — drop it rather than repaint the current view
		// with another task's diff.
		if !a.gitModal.IsOpen() || msg.TaskID != a.gitModal.TaskID() {
			return a, nil
		}
		if msg.Err != nil {
			a.gitModal.SetError(msg.Err.Error())
			return a, nil
		}
		if err := msg.Result.Err(); err != nil {
			a.gitModal.SetError(err.Error())
			return a, nil
		}
		switch msg.Kind {
		case protocol.GitQueryKind_Log:
			a.gitModal.SetLog(msg.Result)
		case protocol.GitQueryKind_Status:
			if a.gitStatusToContent {
				a.gitStatusToContent = false
				a.gitModal.SetStatusContent(msg.Result)
			} else {
				a.gitModal.SetStatus(msg.Result)
			}
		case protocol.GitQueryKind_Subrepos:
			a.gitModal.SetSubrepos(msg.Result)
		case protocol.GitQueryKind_File:
			a.gitModal.SetFileContent(msg.Path, msg.Result)
		default:
			a.gitModal.SetContent(msg.Result)
		}
		// Row count changes with the log, and the split depends on it.
		a.gitModal.SetSize(a.width, a.height)
		return a, nil

	case BoardTopicsMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("topics: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplyTopics(msg.Rows, msg.Subs)
		return a, nil

	case BoardReadMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("read: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplyMessages(msg.Topic, msg.Msgs, msg.Subs, msg.Found)
		return a, nil

	case BoardSubscribersMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("subscribers: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplySubscribers(msg.Topic, msg.Rows)
		return a, nil

	case BoardPurgeMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("purge: " + msg.Err.Error())
			return a, nil
		}
		if !msg.Found {
			a.boardModal.SetStatus("purge: not found")
			return a, nil
		}
		a.boardModal.SetStatus(fmt.Sprintf("purged %d msg(s)", msg.Purged))
		// Kick the relevant refresh so the view reflects the deletion.
		if msg.Seq == 0 {
			return a, DoBoardTopics(a.client)
		}
		return a, DoBoardRead(a.client, msg.Topic)

	case BoardRetractMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("retract: " + msg.Err.Error())
			return a, nil
		}
		if !msg.Found {
			a.boardModal.SetStatus("retract: not found")
			return a, nil
		}
		a.boardModal.SetStatus(fmt.Sprintf("retracted #%d (still readable here)", msg.Seq))
		// Re-read rather than edit the local copy: the message stays in this
		// view, moved to the withdrawn list, and only the server knows what it
		// looks like now.
		return a, DoBoardRead(a.client, msg.Topic)

	case LogChunkMsg:
		if msg.TaskID == a.logs.TaskID() {
			a.logs.Append(msg.Chunk)
		}
		return a, nil

	case NotifyEventMsg:
		a.notify.Append(msg.Event)
		return a, nil

	case LogHistoryMsg:
		// The user may have switched tasks or re-followed between fetch and
		// arrival; only apply the response for the current task AND the
		// current generation.
		if msg.TaskID != a.logs.TaskID() || msg.Gen != a.logsGen {
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
		// Re-follow so the log subscription is re-established on the new
		// client and remains owned by a.logsCancel. Without this the pane
		// would go silent after a reconnect; with a second subscription
		// spawned from main.go it would receive every chunk twice.
		if id := a.logs.TaskID(); id != "" {
			return a, a.followTask(id)
		}
		return a, nil

	case SubscribedMsg:
		switch {
		case msg.Topic == topics.TasksStatus():
			// Initial join: this IS the post-subscribe snapshot — ordering
			// it after the join closes the connect-time race where events
			// landing between snapshot and join were lost. Resubscribe:
			// gap-fill whatever the dead stream missed.
			//
			// Both cases arm the workspace apply. A reconnect must re-establish
			// the forwards (they die with the connection that held their
			// control streams) and may need the resume too: a server restart
			// leaves an interrupted task Failed, which is exactly the state an
			// apply brings back.
			if a.client != nil {
				if a.workspace != nil {
					a.ArmWorkspace()
				}
				return a, RefreshSnapshot(a.client)
			}
		case msg.Topic == topics.RunnersStatus():
			if msg.Resubscribed && a.client != nil {
				return a, RefreshSnapshot(a.client)
			}
		case msg.Resubscribed && a.logs.TaskID() != "" && msg.Topic == topics.TaskLog(a.logs.TaskID()):
			// The followed task-log stream died and rejoined; chunks in
			// between are gone from the live feed. Re-follow: history
			// fetch + fresh subscription repaints the panel completely.
			return a, a.followTask(a.logs.TaskID())
		}
		return a, nil

	case ConnectionMsg:
		a.connected = msg.Connected
		switch {
		case msg.Connected:
			// Nothing to do: a followed task's log is re-followed by the
			// BindClientMsg handler above. Both messages originate from the
			// same PersistLoop reconnect, but this one is emitted first —
			// cli/persist.go calls emit(Connected) synchronously and only
			// then spawns the goroutine that sends BindClientMsg — so do not
			// assume the re-follow has already happened here.
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

	case RestoreResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("restore failed: " + msg.Err.Error()))
			return a, nil
		}
		for _, line := range strings.Split(msg.Text, "\n") {
			a.cmdresult.Append(line)
		}
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

	case AwaitIdleResultMsg:
		short := shortTaskID(msg.TaskID)
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("await-idle " + short + ": " + msg.Err.Error()))
			return a, nil
		}
		switch msg.Status {
		case protocol.AwaitIdleStatus_Fired:
			line := "await-idle " + short + ": session is idle"
			if msg.LastOutputAt > 0 {
				line += " (last output " + formatNanoTs(msg.LastOutputAt) + ")"
			}
			a.cmdresult.Append(OKStyle.Render(line))
		case protocol.AwaitIdleStatus_Armed:
			a.cmdresult.Append(OKStyle.Render("await-idle " + short + ": armed"))
		case protocol.AwaitIdleStatus_SessionStopped:
			a.cmdresult.Append(WarnStyle.Render("await-idle " + short + ": session stopped before going idle"))
		case protocol.AwaitIdleStatus_NotFound:
			a.cmdresult.Append(WarnStyle.Render("await-idle " + short + ": no live session for this task"))
		default:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-idle %s: %v", short, msg.Status)))
		}
		return a, nil

	case PruneResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("prune failed: " + msg.Err.Error()))
			return a, nil
		}
		if msg.IDMode {
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("pruned %d, skipped %d (active=%d, missing=%d)",
				msg.Removed, msg.SkippedActive+msg.SkippedMissing, msg.SkippedActive, msg.SkippedMissing)))
			if msg.SkippedActive > 0 && !msg.Forced {
				a.cmdresult.Append(WarnStyle.Render("prune: pass --force to also drop active (Queued/Running/Detached) tasks"))
			}
		} else {
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("pruned %d task(s)", msg.Removed)))
		}
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

	case SetCapsResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("caps set failed: " + msg.Err.Error()))
			return a, nil
		}
		// Name the target and the change — "1 task changed" says nothing on
		// the usual single-target call. The count appears only when a cascade
		// actually reached descendants.
		short := msg.TaskID
		if len(short) > 8 {
			short = short[:8]
		}
		line := "caps set " + short
		if msg.Summary != "" {
			line += ": " + msg.Summary
		}
		if n := len(msg.Affected) - 1; n > 0 {
			line += fmt.Sprintf("  (+%d descendant(s) clamped)", n)
		}
		if msg.ConnsClosed > 0 {
			// Worth saying out loud: a narrowing tears down the affected tasks'
			// live connections, so an attach or transfer they had open is gone.
			line += fmt.Sprintf(", %d connection(s) closed", msg.ConnsClosed)
		}
		a.cmdresult.Append(OKStyle.Render(line))
		return a, RefreshSnapshot(a.client)

	case SetParentResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("set-parent failed: " + msg.Err.Error()))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(cli.SetParentMessage(msg.Opts, msg.Res)))
		return a, RefreshSnapshot(a.client)

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

	case FileEditRequestMsg:
		a.cmdresult.Append("loading " + msg.Rel + " for edit…")
		return a, DoFileEditLoad(a.client, msg.TaskID, msg.Rel,
			// The route is a command-line choice; the interactive widgets
			// take the default.
			protocol.FileTransferRoute_Splice)

	case FileEditNewRequestMsg:
		a.fileEditor.SetSize(a.width, a.height)
		a.fileEditor.OpenNew(msg.TaskID, msg.Dir)
		return a, nil

	case FileEditLoadedMsg:
		if msg.Err != nil {
			var why string
			switch {
			case errors.Is(msg.Err, cli.ErrFileEditTooLarge):
				why = msg.Rel + ": too large to edit — use file pull"
			case errors.Is(msg.Err, cli.ErrFileEditNotText):
				why = msg.Rel + ": not editable text"
			default:
				why = "edit load: " + msg.Err.Error()
			}
			a.cmdresult.Append(ErrorStyle.Render(why))
			// cmdresult is behind the picker overlay, so an `e` on a binary
			// would otherwise look like the key did nothing.
			if a.filepicker.IsOpen() {
				a.filepicker.SetOpResult(why, true)
			}
			return a, nil
		}
		a.fileEditor.SetSize(a.width, a.height)
		a.fileEditor.OpenEdit(msg.TaskID, msg.Doc)
		return a, nil

	case FileEditSaveMsg:
		force := a.fileEditForce
		a.fileEditForce = false
		// A retargeted path is a new file wherever it points, so there is no
		// baseline to compare it against — push it as a create.
		if msg.Create || msg.Name != msg.Doc.Rel {
			return a, DoFileEditCreate(a.client, msg.TaskID, msg.Name, msg.Text, msg.Doc, protocol.FileTransferRoute_Splice)
		}
		return a, DoFileEditCommit(a.client, msg.TaskID, msg.Doc, msg.Text, force, protocol.FileTransferRoute_Splice)

	case FileEditCommittedMsg:
		switch {
		case msg.Err != nil:
			a.fileEditor.SetStatus("save failed: "+msg.Err.Error(), true)
		case msg.Status == cli.FileEditConflict:
			// Stay open with the buffer intact — losing what was typed is
			// worse than either outcome the operator is choosing between.
			a.fileEditForce = true
			a.fileEditor.SetStatus(msg.Rel+" は runner 側で変更されています — もう一度 ctrl+j で上書き, esc で破棄", true)
		case msg.Status == cli.FileEditUnchanged:
			a.fileEditor.Close()
			a.cmdresult.Append(WarnStyle.Render("no change: " + msg.Rel))
			if a.filepicker.IsOpen() {
				a.filepicker.SetOpResult("no change: "+msg.Rel, false)
			}
		default:
			a.fileEditor.Close()
			a.cmdresult.Append(OKStyle.Render("saved: " + msg.Rel))
			// The picker is what the operator drops back into, and it covers
			// cmdresult — tell them there, too.
			if a.filepicker.IsOpen() {
				a.filepicker.SetOpResult("saved: "+msg.Rel, false)
				return a, DoListFilesFor(a.client, a.filepicker.TaskID(), a.filepicker.CurDir())
			}
		}
		return a, nil

	case FileEditExternalMsg:
		if a.termReleased {
			a.fileEditor.SetStatus("terminal is busy with an attached session; detach it first", true)
			return a, nil
		}
		path, werr := writeFileEditTemp(msg.Name, msg.Text)
		if werr != nil {
			a.fileEditor.SetStatus("temp file: "+werr.Error(), true)
			return a, nil
		}
		ecmd, err := cli.ExternalEditorCommand(path)
		if err != nil {
			os.Remove(path)
			a.fileEditor.SetStatus(err.Error(), true)
			return a, nil
		}
		a.fileEditTmpPath = path
		a.termReleased = true
		return a, execWithoutSuspend(a.program, &editorExec{cmd: ecmd, name: ecmd.Path, path: path},
			func(execErr error) tea.Msg {
				return fileEditExecDoneMsg{path: path, err: execErr}
			})

	case fileEditExecDoneMsg:
		defer os.Remove(msg.path)
		a.termReleased = false
		a.fileEditTmpPath = ""
		if msg.err != nil {
			a.fileEditor.SetStatus("editor exited with an error: "+msg.err.Error()+" (buffer unchanged)", true)
			return a, nil
		}
		b, rerr := os.ReadFile(msg.path)
		if rerr != nil {
			a.fileEditor.SetStatus("read back: "+rerr.Error()+" (buffer unchanged)", true)
			return a, nil
		}
		// Read back into the buffer, never straight to a push: if a GUI
		// editor's launcher returned before its window closed, the operator
		// sees an unchanged buffer instead of a silent no-op "save".
		if string(b) == a.fileEditor.Text() {
			a.fileEditor.SetStatus("external editor made no change", false)
			return a, nil
		}
		a.fileEditor.SetText(string(b))
		a.fileEditor.SetStatus("外部エディタの内容を読み込みました — ctrl+j で保存", false)
		return a, nil

	case InteractiveReadyMsg:
		// armed is true only when this open was dispatched from the `S` key
		// or the resume cases of `r`/`R`/`u`/`U` (the only sites that set
		// pendingInteractive). Capture-and-clear here so a stray AmbiguousRunner
		// from an unrelated in-flight open (e.g. a slow `i` request that
		// resolves after a later `S`) can't misuse this cycle's arm state.
		armed := a.pickerArmed
		a.pickerArmed = false
		if msg.Err != nil {
			var are *cli.AmbiguousRunnerError
			if armed && errors.As(msg.Err, &are) {
				a.runnerPicker.Open(are.Candidates)
				return a, nil
			}
			a.cmdresult.Append(ErrorStyle.Render("open interactive failed: " + msg.Err.Error()))
			return a, nil
		}
		if a.termReleased {
			// Release/Restore do not nest, and this is now reachable: under
			// the old tea.Exec path the Update loop was frozen for the whole
			// suspension, so a second open could not be processed until the
			// first ended. Update keeps running now, so refuse explicitly
			// rather than let two children fight over stdin.
			a.cmdresult.Append(WarnStyle.Render("terminal is busy with another session; detach it first"))
			if msg.X11Cancel != nil {
				msg.X11Cancel()
			}
			if msg.Stream != nil {
				_ = msg.Stream.Close()
			}
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
		a.termReleased = true
		return a, execWithoutSuspend(a.program, &interactiveExec{stream: msg.Stream}, func(err error) tea.Msg {
			return InteractiveDoneMsg{TaskID: msg.TaskID, Err: err}
		})

	case SessionStartedMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("session start failed: " + msg.Err.Error()))
			// A workspace resume that failed is very often the reconnect after a
			// server restart racing the RUNNER's own reconnect: the client is
			// back first, so the apply finds no runner for the repo. Arm ONE
			// retry for the next runner event rather than leaving the case this
			// feature exists for failing by default. Measured against a dummy
			// harness: the TUI reconnected ~10s before the runner did.
			if a.workspace != nil {
				a.workspaceRetryOnRunner = true
			}
			return a, nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("started detached: ") + short)
		// A workspace resume only makes the task exist again; its forwards were
		// deliberately held back (see applyWorkspace). Re-arm so the snapshot
		// this refresh triggers reconciles with the task alive and starts them.
		// Terminates: the task is then live, so the next pass resumes nothing.
		if a.workspaceDeclares(msg.TaskID) {
			a.ArmWorkspace()
		}
		return a, RefreshSnapshot(a.client)

	case InteractiveDoneMsg:
		a.termReleased = false
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
		a.activeForwards[msg.ID] = &PortForwardSession{ID: msg.ID, TaskID: msg.TaskID, Direction: msg.Direction, Spec: msg.Spec, Cancel: msg.Cancel, ForwardID: msg.ForwardID, FromWorkspace: msg.FromWorkspace}
		a.cmdresult.Append(OKStyle.Render("forward started: ") + pfShortID(msg.TaskID) + "  " + msg.Direction.flag() + " " + msg.Spec)
		return a, nil

	case PortForwardRegisteredMsg:
		// Backfills the server-assigned id onto a local (-L) forward once its
		// RegisterPortForward call completes (see PortForwardRegisteredMsg doc
		// in tui/portforward.go) — remote (-R) forwards already carry it from
		// PortForwardStartedMsg above. A miss here (already stopped/removed
		// before registration was reported) is a harmless no-op.
		if s, ok := a.activeForwards[msg.ID]; ok {
			s.ForwardID = msg.ForwardID
		}
		return a, nil

	case PortForwardStoppedMsg:
		// The forward goroutine exited (stopped, or never started on bind
		// failure). Drop it so it no longer shows in the stop picker. If it was
		// already removed (e.g. the user killed it via the picker), this is a
		// no-op and we skip the duplicate log.
		if _, ok := a.activeForwards[msg.ID]; ok {
			delete(a.activeForwards, msg.ID)
			a.cmdresult.Append("forward stopped: " + pfShortID(msg.TaskID))
		}
		return a, nil

	case PortForwardStatusMsg:
		a.cmdresult.Append(msg.Line)
		return a, nil

	case SSHGatewayStartedMsg:
		a.sshGateway = &SSHGatewaySession{Listen: msg.Listen, Cancel: msg.Cancel}
		for _, line := range sshGatewayStartedLines(msg.Listen) {
			a.cmdresult.Append(line)
		}
		return a, nil

	case SSHGatewayStoppedMsg:
		// The serve loop exited — stopped on purpose, or failed after binding.
		// A miss is a no-op; the failure line, if any, arrived separately.
		if a.sshGateway != nil {
			a.sshGateway = nil
			a.cmdresult.Append("ssh-gateway stopped")
		}
		// Only NOW is the port actually released, which is why a workspace
		// apply that has to move the gateway parks its restart here instead of
		// batching it beside the stop. Cleared before dispatching so a start
		// that fails does not leave a restart armed for the next stop.
		if to := a.gatewayRestartTo; to != "" {
			a.gatewayRestartTo = ""
			if a.client == nil {
				a.cmdresult.Append(ErrorStyle.Render("ssh-gateway: not connected to server"))
				return a, nil
			}
			a.cmdresult.Append("ssh-gateway: starting on " + to)
			return a, DoStartSSHGateway(a.client, to, sshgw.DefaultHostKeyPath(a.configPath), "", a.program)
		}
		return a, nil

	case SSHGatewayStatusMsg:
		a.cmdresult.Append(msg.Line)
		return a, nil

	case RawForwardOpenedMsg:
		// A pane closed while its open was in flight owns nothing now, so the
		// connection must be closed here — otherwise it stays registered with
		// no UI reference able to reach it.
		if a.rawModal.PaneForGen(msg.Gen) == nil {
			_ = msg.Conn.Close()
			if msg.Cancel != nil {
				msg.Cancel()
			}
			return a, nil
		}
		a.rawModal.SetConn(msg.Gen, msg.Conn, msg.Cancel,
			fmt.Sprintf("connected (fwd %d)", msg.ForwardID))
		a.rawModal.Refresh()
		return a, nil

	case RawForwardDataMsg:
		if p := a.rawModal.PaneForGen(msg.Gen); p != nil {
			p.AppendOutput(msg.Data)
			a.rawModal.Refresh()
		}
		return a, nil

	case RawForwardClosedMsg:
		// The pump already closed the connection (that's what deregisters the
		// forward server-side — a data-side EOF alone does not); MarkClosed is
		// therefore idempotent here, but still the one place that drops the
		// pane's own reference and stops its sink goroutine.
		a.rawModal.MarkClosed(msg.Gen, msg.Reason)
		a.rawModal.Refresh()
		return a, nil
	}
	return a.updatePane(msg)
}

func (a *App) updateWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	a.width = msg.Width
	a.height = msg.Height
	a.layout()
	a.filepicker.SetSize(a.width, a.height)
	a.fileEditor.SetSize(a.width, a.height)
	a.connsModal.SetSize(a.width, a.height)
	a.forwardsModal.SetSize(a.width, a.height)
	a.forwardTap.SetSize(a.width, a.height)
	a.execsModal.SetSize(a.width, a.height)
	a.boardModal.SetSize(a.width, a.height)
	a.gitModal.SetSize(a.width, a.height)
	a.grid.SetSize(a.width, a.height)
	a.chat.SetSize(a.width, a.height)
	a.rawModal.SetSize(a.width, a.height)
	a.authorityPicker.SetSize(a.width, a.height)
	a.workspacePicker.SetSize(a.width, a.height)
	return a, nil
}

// updateKey routes one key. An open overlay owns it outright (appOverlays,
// topmost first); otherwise the structural keys — quit, help, focus — are
// checked, then the mainKeyBindings row that names the key runs where its
// Scope applies, and whatever nothing claimed reaches the focused pane.
func (a *App) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	for _, o := range appOverlays {
		if o.open(a) {
			return a, o.key(a, msg)
		}
	}
	// Ctrl+C always quits.
	if msg.Type == tea.KeyCtrlC {
		return a, a.quit()
	}
	// While the logs panel is in filter-edit mode, every printable rune
	// (including 'q', 's', 'c') belongs to the filter draft, just like
	// in cmdline focus.
	logsEditing := a.focus == focusLogs && a.logs.IsEditingFilter()
	// `q` quits when not in the cmdline / not composing a filter (those
	// must accept literal 'q').
	if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Quit {
		return a, a.quit()
	}
	// `?` shows every binding. The footer is one row and drops what does
	// not fit (see footerHints), so this is where the full list lives.
	// Reuses the read-only DetailPopup — same Esc-closes / swallow-all
	// handling as the `d` detail view.
	if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Help {
		a.detail.Open("keys", keyHelpBody())
		return a, nil
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
	if cmd, ok := a.dispatchMainKey(msg, logsEditing); ok {
		return a, cmd
	}
	// Cmdline submit.
	if a.focus == focusCmdline && (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) {
		if a.navigateCmdHistory(msg.Type == tea.KeyUp) {
			return a, nil
		}
	}
	if a.focus == focusCmdline && msg.Type == tea.KeyEnter {
		input := a.cmdline.Value()
		a.addCmdHistory(input)
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
	return a.updatePane(msg)
}

// updatePane forwards a message to the focused pane.
func (a *App) updatePane(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (a *App) addCmdHistory(input string) {
	if strings.TrimSpace(input) == "" {
		a.cmdHistoryIndex = -1
		a.cmdHistoryDraft = ""
		return
	}
	if len(a.cmdHistory) == 0 || a.cmdHistory[len(a.cmdHistory)-1] != input {
		a.cmdHistory = append(a.cmdHistory, input)
		const maxCmdHistory = 100
		if len(a.cmdHistory) > maxCmdHistory {
			a.cmdHistory = a.cmdHistory[len(a.cmdHistory)-maxCmdHistory:]
		}
	}
	a.cmdHistoryIndex = -1
	a.cmdHistoryDraft = ""
}

func (a *App) navigateCmdHistory(previous bool) bool {
	if len(a.cmdHistory) == 0 {
		return false
	}
	switch {
	case previous && a.cmdHistoryIndex == -1:
		a.cmdHistoryDraft = a.cmdline.Value()
		a.cmdHistoryIndex = len(a.cmdHistory) - 1
	case previous && a.cmdHistoryIndex > 0:
		a.cmdHistoryIndex--
	case !previous && a.cmdHistoryIndex == -1:
		return false
	case !previous && a.cmdHistoryIndex < len(a.cmdHistory)-1:
		a.cmdHistoryIndex++
	case !previous:
		a.cmdHistoryIndex = -1
		a.cmdline.SetValue(a.cmdHistoryDraft)
		a.cmdline.CursorEnd()
		return true
	}
	a.cmdline.SetValue(a.cmdHistory[a.cmdHistoryIndex])
	a.cmdline.CursorEnd()
	return true
}

func (a *App) cycleFocus(delta int) {
	a.setFocus(focus((int(a.focus) + delta + numFocus) % numFocus))
}

// setFocus moves focus straight to one pane, blurring the rest.
func (a *App) setFocus(f focus) {
	a.runners.Blur()
	a.tasks.Blur()
	a.logs.Blur()
	a.notify.Blur()
	a.cmdresult.Blur()
	a.cmdline.Blur()

	a.focus = f

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

// quit tears down what the process owns before ending the program. Raw panes
// hold RawConns whose Close is what deregisters the forward server-side: a TUI
// that exits without closing them leaves rows in `forward ls` that nothing can
// reach.
func (a *App) quit() tea.Cmd {
	a.rawModal.CloseAllPanes()
	return tea.Quit
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
	// The footer is budgeted as exactly one row (see layout), so every branch
	// here is clipped to the terminal width — an over-long hint wraps and
	// pushes the bottom of the view off-screen.
	var hint string
	switch {
	case a.logs.IsEditingFilter():
		hint = "/" + a.logs.FilterDraft() + "_   (enter apply · esc cancel)"
	case a.logs.Filter() != "":
		hint = "[filter: " + a.logs.Filter() + "]   tab focus · / edit · esc clear · " + mainKeys.Quit + " quit"
	default:
		hint = footerHints(a.focus, a.width)
	}
	footer := FooterStyle.Render(clipLine(hint, 0, a.width))

	view := strings.Join([]string{
		header,
		top,
		logView,
		notifyView,
		cmdresultView,
		cmdlineView,
		footer,
	}, "\n")
	// Editor before picker: it opens from the picker and draws on top of it,
	// matching the key precedence in Update.
	if a.fileEditor.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.fileEditor.View())
	}
	if a.filepicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.filepicker.View())
	}
	if a.popup.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.popup.View())
	}
	if a.detail.IsOpen() {
		a.detail.SetSize(a.width, a.height)
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.detail.View())
	}
	if a.portForwardModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.portForwardModal.View())
	}
	if a.rawModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.rawModal.View())
	}
	if a.forwardPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.forwardPicker.View())
	}
	if a.workspacePicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.workspacePicker.View())
	}
	if a.authorityPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.authorityPicker.View())
	}
	if a.runnerPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.runnerPicker.View())
	}
	if a.connsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.connsModal.View())
	}
	if a.execsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.execsModal.View())
	}
	if a.forwardTap.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.forwardTap.View())
	}
	if a.forwardsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.forwardsModal.View())
	}
	if a.boardModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.boardModal.View())
	}
	if a.gitModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.gitModal.View())
	}
	if a.grid.IsOpen() {
		// Top-align vertically (not Center): if the grid is ever fractionally
		// taller than the terminal, the overflow must clip the BOTTOM, never the
		// top — the top row carries the pane headers. MaxHeight clamps it too.
		return lipgloss.NewStyle().MaxHeight(a.height).Render(
			lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Top, a.grid.View()))
	}
	if a.chat.IsOpen() {
		// Top-aligned and height-clamped for the grid's reason, plus one of its
		// own: this view pins the input line to the BOTTOM of its own body, so
		// letting the overflow clip the bottom would take the prompt with it.
		// ChatModel.View sizes its transcript from a.height, so the clamp here
		// is a backstop rather than the mechanism.
		// LEFT, unlike the grid's Center: the grid places fixed-width panes and
		// centring the block reads as deliberate, while this view is full-width
		// prose. Centred, every transcript line floated to its own indent —
		// caught by driving it, not by reading it.
		return lipgloss.NewStyle().MaxHeight(a.height).Render(
			lipgloss.Place(a.width, a.height, lipgloss.Left, lipgloss.Top, a.chat.View()))
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
	a.logsGen++
	if taskID == "" || a.client == nil || a.program == nil || a.appCtx == nil {
		return nil
	}
	gen := a.logsGen
	subCtx, cancel := context.WithCancel(a.appCtx)
	a.logsCancel = cancel
	return tea.Batch(
		DoGetTaskLogGen(a.client, taskID, gen),
		func() tea.Msg {
			go SubscribeTaskLog(subCtx, a.client, a.program, taskID)
			return nil
		},
	)
}

// applyEventAct copies the act fields (last_output_at / output_idle_ms) from
// a task event into ti and stamps the local receipt time. Every task event
// carries current act enrichment (zero = no live session); a terminal status
// clears the badge outright — idleness of a dead session is meaningless, and
// a TaskEnded event may still carry the just-stopped mux's timestamps.
func (a *App) applyEventAct(ti *protocol.TaskInfo, id string, ev protocol.TaskStatusEvent) {
	terminal := ev.TaskStatus == protocol.TaskStatus_Succeeded ||
		ev.TaskStatus == protocol.TaskStatus_Failed ||
		ev.TaskStatus == protocol.TaskStatus_Cancelled
	if terminal || ev.LastOutputAt == 0 {
		ti.LastOutputAt = 0
		ti.OutputIdleMs = 0
		delete(a.actRecvAt, id)
		return
	}
	ti.LastOutputAt = ev.LastOutputAt
	ti.OutputIdleMs = ev.OutputIdleMs
	a.actRecvAt[id] = time.Now()
}

// applyEventObservers copies the observer counts from a task event into ti.
// Every task event carries them (the server stamps them alongside the act
// fields), and a task_observers event exists precisely because an observer
// attaching moves no status — without it this row would keep whatever the last
// List snapshot said, and the TUI polls no snapshots of its own.
//
// A terminal task has no session and therefore no observers; a stale non-zero
// count on a finished row would read as "someone is still watching this".
func (a *App) applyEventObservers(ti *protocol.TaskInfo, ev protocol.TaskStatusEvent) {
	terminal := ev.TaskStatus == protocol.TaskStatus_Succeeded ||
		ev.TaskStatus == protocol.TaskStatus_Failed ||
		ev.TaskStatus == protocol.TaskStatus_Cancelled
	if terminal {
		ti.Viewers, ti.Cowriters = 0, 0
		return
	}
	ti.Viewers = ev.Viewers
	ti.Cowriters = ev.Cowriters
}

// refreshTasksTable rebuilds the tasks table from tasksByID, sorted by
// descending CreatedAt, capped at 100 rows. Idle badges are aged here: the
// rendered idle duration is the wire value plus the local time elapsed since
// receipt (wire age is server-clock, elapsed is local — no cross-host skew).
// Busy badges are NOT aged; they flip only via the server's idle-edge event.
func (a *App) refreshTasksTable() {
	all := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	now := time.Now()
	for id, t := range a.tasksByID {
		if t.LastOutputAt > 0 && time.Duration(t.OutputIdleMs)*time.Millisecond >= protocol.ActivityBusyThreshold {
			if rt, ok := a.actRecvAt[id]; ok {
				t.OutputIdleMs += uint64(now.Sub(rt) / time.Millisecond)
			}
		}
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > 100 {
		all = all[:100]
	}
	a.tasks.SetRows(all, a.runnersSnapshot)
}

// openGrid is the ONE way this TUI opens the session viewer: the keys and the
// `grid` verb both land here, so the set, its label and the refusal wording
// cannot drift between them.
//
// It refuses on an empty result rather than opening. A full-screen overlay
// reading "nothing here" costs a keystroke to escape and says less than the
// line this appends — and the two counts are the useful part: a scope holding
// four tasks none of which is watchable is a different situation from a scope
// holding none.
func (a *App) openGrid(mode cli.GridScopeMode, anchor string, ids []string) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("grid: not connected"))
		return nil
	}
	set, label, err := cli.GridSet(a.visibleTasks(), mode, anchor, ids)
	if err != nil {
		a.cmdresult.Append(ErrorStyle.Render(err.Error()))
		return nil
	}
	if n := len(gridLiveTasks(set)); n == 0 {
		a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf(
			"grid %s: no live interactive session in this set (%d task(s) in it)", label, len(set))))
		return nil
	}
	a.gridSelMode, a.gridSelAnchor, a.gridSelIDs, a.gridSelSet = mode, anchor, ids, true
	a.grid.Open(a.appCtx, a.client, set, label)
	a.grid.SetSize(a.width, a.height)
	return gridTick()
}

// visibleTasks is the operator's current task set as a slice. tasksByID is the
// store; everything that filters, tiles or walks the creator tree wants a
// slice, and each of those sites building its own was how the grid's input and
// the table's input drifted apart in the first place.
func (a *App) visibleTasks() []protocol.TaskInfo {
	out := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	for _, t := range a.tasksByID {
		out = append(out, t)
	}
	return out
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

// killLocalForward stops one of this TUI's own forwards (tasks-pane P/B, or
// the forward-stop picker) through the same DoKillForward RPC as the forwards
// modal's `x` (then y/n) and the `forward kill` cmdline verb — the only stop
// path after this task, whether the forward is ours or another client's. A
// local (-L) forward's ForwardID is populated asynchronously
// (PortForwardRegisteredMsg, see tui/portforward.go); zero means the
// registration hasn't landed yet, so there is nothing to kill. TaskID/Spec
// ride along on the result so the confirmation line names what was killed,
// not just its server-assigned id.
func (a *App) killLocalForward(sess *PortForwardSession) tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("forward: not connected"))
		return nil
	}
	if sess.ForwardID == 0 {
		a.cmdresult.Append(WarnStyle.Render("forward: not fully registered yet — try again in a moment"))
		return nil
	}
	return DoKillForward(a.client, sess.ForwardID, sess.TaskID, sess.Direction.flag()+" "+sess.Spec)
}

// runAction dispatches a parsed cmdline Action.
func (a *App) runAction(act Action) (tea.Model, tea.Cmd) {
	// Client-requiring actions dispatch a Do* closure that calls a method on
	// a.client; with a nil client (initial dial still pending / failed under
	// --persist) that closure nil-panics inside cli.(*Client).<RPC> when
	// bubbletea executes it — observed as a runtime panic on `prune <id>` while
	// disconnected. Reject them early here with the same "not connected" notice
	// the per-modal guards use. The listed actions need no client (Refresh /
	// Trsf self-guard below and so fall through here); anything not listed is
	// treated as client-requiring, so a future Do*-dispatching action is
	// guarded by default rather than silently re-opening this panic.
	switch act.(type) {
	case verb.ScreenAction, verb.CatalogAction, verb.SetDefaultsAction:
		// no client needed. ScreenAction is the screen-state family --
		// clear / quit / help / refresh / trsf / diag / repo -- where refresh
		// and trsf carry their own nil-client notice and the rest touch no RPC
		// at all. CatalogAction (`caps`) reads a compiled-in table, and
		// SetDefaultsAction only writes this process's own spawn defaults.
	default:
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
			return a, nil
		}
	}
	// Every verb the declaration gives this surface, routed by the GENERATED
	// dispatcher: tuiVerbs implements one method per declared verb, and a
	// missing one does not compile. "The TUI answers every declared verb" was
	// a test before, and a test only sees what it was told to look for.
	if cmd, handled := verb.DispatchTUIAction[tea.Cmd](tuiVerbs{a}, act); handled {
		return a, cmd
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

// uniqueAgentProfiles returns the de-duplicated union of agent profiles
// advertised across a runner snapshot, in stable (sorted) order — used to
// populate the submit popup's agent selector (§6 TUI compose flow). A
// runner that advertises no explicit AgentProfiles (legacy runner) falls
// back to its single implicit AgentBin profile, mirroring the server's
// RunnerEntry.DefaultProfile/advertisedProfiles fallback (server/registry.go,
// server/task_handler.go) so the choices shown here match what the server
// will actually accept.
func uniqueAgentProfiles(rs []protocol.RunnerInfo) []string {
	seen := make(map[string]struct{}, len(rs))
	out := make([]string, 0, len(rs))
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, r := range rs {
		if len(r.AgentProfiles) == 0 {
			add(string(r.AgentBin))
			continue
		}
		for _, p := range r.AgentProfiles {
			add(string(p.Name))
		}
	}
	sort.Strings(out)
	return out
}

// gitReload asks for everything the git modal shows at its CURRENT root: the
// commit list, the worktree summary, the nested repositories, and the diff for
// the default selection. One helper because a re-root, a refresh and the
// initial open all need the same four answers, and a route that forgot one
// would silently show the previous repository's data.
// gitReload issues the three panel queries. limits, when non-nil, applies the
// command line's --max / --max-bytes -- the keyboard route passes nil and
// keeps the modal's own settings.
func (a *App) gitReload(taskID string, limits func(cli.GitQuery) cli.GitQuery) tea.Cmd {
	q := a.gitModal.Query()
	if limits != nil {
		q = limits(q)
	}
	diffQ := q
	diffQ.BaseRev = a.gitModal.BaseRev()
	diffQ.Target = protocol.GitDiffTarget_Worktree
	diffQ.Kind = protocol.GitQueryKind_Diff
	a.gitModal.RecordContentQuery(diffQ)
	return tea.Batch(
		DoGitLog(a.client, taskID, q),
		DoGitStatus(a.client, taskID, q),
		DoGitSubrepos(a.client, taskID, q),
		DoGitDiff(a.client, taskID, diffQ),
	)
}

// runSpawnAction executes a parsed spawn. The three verbs share their grammar
// (cli/verb) and differ only here, in what the TUI does with the result:
// submit queues, interactive attaches now, session new opens a detachable one.
func (a *App) runSpawnAction(v verb.SpawnAction) (tea.Model, tea.Cmd) {
	repo := v.Repo
	if repo == "" {
		repo = a.defaultRepo
	}
	caps, capsOverride := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
	auth := a.spawnAuthority(v.Scope, v.Overrides, v.ResumeTaskID, caps)
	switch v.Kind {
	case verb.KindSubmit:
		return a, DoSubmitWithOpts(a.client, repo, v.Task, "", v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
	case verb.KindInteractive:
		return a, DoOpenInteractiveWithOpts(a.client, repo, "", v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
	}
	sel := cli.SelectorOpts{Host: v.Host, Runner: v.Runner, IP: v.IP}
	if v.X11 {
		return a, DoOpenX11Session(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, int(v.X11Display), a.program, auth, capsOverride, v.ResumeConversation, v.Agent)
	}
	if v.Detach {
		return a, DoStartDetachedSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent, TermSize{Rows: uint16(a.height), Cols: uint16(a.width)}, v.Stream)
	}
	// Stream without Detach cannot reach here: the declaration refuses it.
	return a, DoOpenDetachableSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
}

// cmdlinePlaceholder is the cmdline's ghost text: a width-limited SUMMARY, so
// unlike cmdlineHelpLines it does not name every verb. What it must not do is
// name one the cmdline cannot parse, which is what
// TestPlaceholderNamesNothingUnreachable checks -- the placeholder is the
// first thing an operator reads, and a verb listed there is a promise.
const cmdlinePlaceholder = "submit / interactive / session / file / forward / ssh-gateway / server / workspace / cancel / notify / prune / repo / caps / clear / help / quit"

// tuiVerbHelp is what each declared verb DOES, on this surface. The synopsis
// -- which flags, which positionals, in what order -- is not written here:
// cmdlineHelpLines generates it from the same declaration the parser reads,
// so a flag this surface accepts cannot be missing from the line describing
// it, and one it does not accept cannot appear.
//
// That split is the point. The hand-written half was 50 lines carrying BOTH,
// and the synopsis half is the one that drifts silently: the CLI's copy
// documented `--caps ... default all` for as long as the declaration has said
// none.
//
// A path with no entry is a build-time gap, not a silent omission: the
// completeness test names it.
var tuiVerbHelp = map[string]string{
	"cancel":                   "cancel a queued/running task",
	"caps set":                 "OPERATOR: re-grant a LIVE task's authority; effective on its next request, no restart. --cascade also clamps its descendants",
	"caps set-defaults":        "the defaults a spawn from THIS session carries when its own line names neither; no flags opens the picker. Also spelled `scope`",
	"caps set-parent":          "OPERATOR: re-point a live task's parent, the edge subtree scopes walk; --swap inverts it with its current parent",
	"caps":                     "the capability catalog: every grantable capability and the sentence saying what it gates, plus the scope grammar",
	"clear":                    "empty this result panel",
	"diag":                     "grid panes overlay their own state + arrival rate on row 1 (debug; bare `diag` toggles, HARNESS_GRID_DIAG seeds it at startup)",
	"exec kill":                "stop one running exec",
	"exec ls":                  "list the running execs (Obs column shows Nx while any run)",
	"exec":                     "run a command in the task's worktree as its own process, NOT in the session's shell (stdout 1| / stderr 2|). --shell hands it to the runner's own shell so pipes and redirects mean something; --sshd-parent gives the line a parent named sshd for a client that checks its ancestry (Windows only; needs --shell)",
	"exit":                     "leave the TUI",
	"file delete":              "remove a file (no -r) or directory (-r empty / -r -f recursive)",
	"file edit":                "open a text file in the editor popup and push it back (ctrl+j save, ctrl+o $EDITOR)",
	"file ls":                  "list a directory in the task's worktree (root if rel omitted)",
	"file mkdir":               "create a directory in the worktree (-p: mkdir -p)",
	"file new":                 "write a new text file in the editor popup and push it",
	"file pull":                "copy from the worktree to a local path",
	"file push":                "copy a local file/dir into the worktree (-r tar, -f overwrite, -p mkdir parents)",
	"forward kill":             "close one registered forward by id (also: tasks-pane P/B on the owning task)",
	"forward ls":               "list every port forward visible to this operator (also: f key, kill: x then y/n)",
	"forward tap":              "stream the bytes crossing a forward, live; nothing is recorded server-side, so a tap sees only what crosses after it opens",
	"git diff":                 "revisions counted as git counts them: none=unstaged, one=<base> vs working tree, two=commit vs commit",
	"git file":                 "one file's whole content (also: o in the modal, from the diff you are reading)",
	"git log":                  "the task's commits (also: tasks-pane G)",
	"git show":                 "one commit and its diff",
	"git status":               "uncommitted and untracked paths (untracked appear in no diff)",
	"git subrepos":             "git repos nested inside the worktree ([REPO] rows; Enter descends, u goes up)",
	"grid":                     "live session viewer over exactly these tasks (also: g for all, z/Z for the selected task's subtree); --under <id> takes that task's working set \u2014 its subtree PLUS the tasks its own scope names (ids:) \u2014 and --descendants leaves the task itself out",
	"help":                     "this list",
	"interactive":              "open/resume interactive session (detachable)",
	"notify":                   "send a notification (shows in this feed + --notify-hook egress; keep it one line)",
	"prune":                    "ask the server to forget tasks (ids, or --before; active tasks need --force)",
	"quit":                     "leave the TUI (alias: exit); the sessions it opened keep running",
	"refresh":                  "force a full runners+tasks snapshot re-sync now (alias: sync)",
	"repo":                     "the default repo a spawn uses when its own line names none",
	"restore":                  "with no ids (or --list): what a prune forgot and could still be put back \u2014 the ids live only in the server's WAL. With ids: put those back (needs `prune` and the same scope; the record returns, the task log does not)",
	"scope":                    "the shorter spelling of `caps set-defaults`",
	"server dial-runner":       "ask the server to reverse-dial a Listen-mode runner (Phase A, ACL envs)",
	"session attach":           "reattach to a session",
	"session await-idle":       "fire when the session's output goes idle (default: result line here; --notify: operator notification)",
	"session kill":             "terminate a session",
	"session ls":               "list detachable sessions",
	"session new":              "open/resume detachable interactive; --detach backgrounds it and prints the id",
	"session stream approve":   "answer a tool-approval request the agent is blocked on",
	"session stream attach":    "follow an event-stream session's events (the counterpart of attaching to a PTY)",
	"session stream finish":    "close the agent's stdin so it ends the turn in flight and exits cleanly",
	"session stream interrupt": "abandon the running turn; the agent survives to take the next one",
	"session stream turn":      "send one user turn to an event-stream session",
	"ssh-gateway start":        "serve ssh: `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches; bare user = cowrite, .control takes the seat, .view watches",
	"ssh-gateway status":       "whether it is running, and on what address",
	"ssh-gateway stop":         "stop the gateway and every session it serves",
	"submit":                   "submit/resume a task",
	"sync":                     "the shorter spelling of refresh",
	"trsf":                     "dump the client\u2194server transport's internal state (debug)",
	"workspace apply":          "re-apply a workspace now (also runs on start and on every reconnect)",
	"workspace detach":         "stop re-applying on reconnect; --stop also stops its forwards and gateway",
	"workspace ls":             "list the workspaces in .harness/config",
	"workspace rm":             "delete one workspace from .harness/config",
	"workspace save":           "pick which tasks, their resume/runner, their forwards and the grid (--all: no picker)",
	"workspace show":           "print one workspace",
}

// tuiKeyHelp is the part no table holds: what a KEY does, and how a verb
// pairs with one. It stays hand-written because a keybinding is not a verb --
// nothing parses it -- and pretending otherwise would put screen state in the
// grammar.
var tuiKeyHelp = []string{
	"F (tasks focus): open file picker — Enter/→ to descend a dir, Backspace/← to go back. e edit / n new / u push / g pull / d delete / D rm -rf. Esc closes.",
	"  picker push/pull input — Tab toggles local fs browser. Tab back to typing pre-fills the selected file's path; Enter commits.",
	"  push/pull overwrite — first try fails on existing dest; picker prompts overwrite? (y/n). y retries with force=true.",
}

// cmdlineHelpLines renders the `help` body: one line per declared verb, its
// synopsis generated and its description declared, then the key bindings.
func cmdlineHelpLines() []string {
	out := []string{
		"commands: submit / interactive / session / file / git / forward / exec / grid / workspace / " +
			"cancel / notify / prune / restore / repo / caps / caps set-defaults / refresh / clear / help / quit",
	}
	for _, path := range verb.PathsForSurface(verb.TUI) {
		sp, ok := verb.Lookup(strings.Fields(path)...)
		if !ok {
			continue
		}
		// Synopsis and description on separate lines. `submit`'s declared
		// flags run past 300 characters, and this panel is narrower than a
		// terminal -- one line carrying both put the description where a
		// reader never reaches it.
		out = append(out, strings.TrimPrefix(sp.For(verb.TUI).Usage(), "usage: "))
		if d := tuiVerbHelp[path]; d != "" {
			out = append(out, "    - "+d)
		}
	}
	return append(out, tuiKeyHelp...)
}
