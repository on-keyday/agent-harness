package tui

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"github.com/on-keyday/agent-harness/connauth"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

type focus int

const (
	focusRunners focus = iota
	focusTasks
	focusSchedules
	focusCredentials
	focusNotify
	focusCmdresult
	focusCmdline
	numFocus = iota
)

// App is the top-level Bubble Tea Model.
type App struct {
	server string
	// principal is what this process connects AS — "operator" or
	// "agent <short-task-id>". Shown in the header because an agent's
	// caps=none makes the runner list come back empty, which otherwise
	// reads as a broken pane.
	principal   string
	defaultRepo string

	runners   RunnersModel
	tasks     TasksModel
	schedules SchedulesModel
	// credentials is the fourth table: the credential tasks of the snapshot.
	credentials CredentialsModel
	notify      NotifyModel
	cmdresult   CmdResultModel
	cmdline     textinput.Model
	cmdHistory  []string
	// cmdHistoryIndex is -1 while editing a fresh command; otherwise it points
	// at cmdHistory while Up/Down is browsing prior entries.
	cmdHistoryIndex int
	cmdHistoryDraft string
	popup           PopupModel
	bgform          BgFormModel
	recordKeep      RecordKeepForm
	detail          DetailPopup
	filepicker      FilePickerModel
	fileEditor      FileEditModel
	// One-shot: set when a commit came back FileEditConflict, consumed by the
	// next save so a second ctrl+j overwrites deliberately rather than by
	// default.
	fileEditForce bool
	// Temp file held across an external-editor handover.
	fileEditTmpPath string

	focus focus
	// lastTable is the table (runners / tasks / schedules / credentials) focus last rested on;
	// shownTable draws it while focus is on notify / cmdresult / cmdline.
	lastTable focus
	width     int
	height    int

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
	// progressRecvAt is the same for progress_age_ms: the local receipt time
	// of each task's progress fields, so a stale line keeps aging on screen
	// (D4 — the reader judges staleness by the age) although the TUI fetches
	// nothing by itself. Kept apart from actRecvAt because progress outlives
	// the session: a finished task's line still ages.
	progressRecvAt map[string]time.Time
	// lifecycleRecvAt is the same for lifecycle_age_ms, so `WAIT:3m` keeps
	// counting between events.
	lifecycleRecvAt map[string]time.Time
	// authorityRecvAt is the same for authority_age_ms. Only a snapshot brings
	// provenance (no task event carries it), so only a snapshot stamps it.
	authorityRecvAt map[string]time.Time
	// schedDeleteID is the schedule (32 hex) whose delete awaits y/n; "" = none.
	// While set, inSchedDeleteConfirm owns the keys.
	schedDeleteID string
	// schedErr is the last schedule-list failure reported, so a list that keeps
	// failing is said once, not on every snapshot.
	schedErr string
	// schedExec, when set, replaces cli.ScheduleIDWith for the run / delete
	// keys. A test seam: the real call needs a server.
	schedExec                           func(ctx context.Context, act verb.ScheduleIDAction, out io.Writer) error
	schedList                           func(ctx context.Context) ([]protocol.ScheduleRow, error)
	schedListInFlight, schedListPending bool
	// credRevokeID is the credential (32 hex) whose revoke awaits y/n; "" =
	// none. While set, inCredRevokeConfirm owns the keys.
	credRevokeID string
	// credExec, when set, replaces the cli call of every credential verb (the
	// x key and the typed verbs). A test seam: it is handed the client the
	// real call would use, which must be the long-lived a.client.
	credExec func(ctx context.Context, c *cli.Client, act any, out io.Writer) error
	// runnersSnapshot holds the latest runners from the most recent snapshot.
	runnersSnapshot []protocol.RunnerInfo

	// connections view
	connsModal ConnsModal
	// overview overlay (V / `overview`) and the inputs it last fetched itself
	overview   OverviewModel
	overviewIn OverviewFetchMsg
	// overviewHave: overviewIn holds a real read (not the zero value).
	overviewHave bool
	// Which run of the conns modal's trsf poll is current; a tick from an
	// older one is dropped rather than allowed to reschedule. See trsfTickMsg.
	trsfGen       int
	forwardsModal ForwardsModal
	// tap is the live traffic view for one forward or exec. Its pump is
	// stopped through tapStop, which is nil whenever no tap is running.
	tap        TapView
	tapStop    context.CancelFunc
	execsModal ExecsModal
	// actionsModal is M (the Actions screen); actionRunForm is the tasks
	// table's ! (run an action against the selected task).
	actionsModal  ActionsModal
	actionRunForm ActionRunFormModel
	// runnerKeysModal is K (runner keys and dial records); kickAsk is the
	// runners table's pending kick question, nil when none.
	runnerKeysModal RunnerKeysModal
	kickAsk         *kickQuestion
	// idleWatchersModal lists armed await-idle watchers; opened with `I`.
	idleWatchersModal IdleWatchersModal
	// capsAuditModal lists, per capability, the tasks holding it; opened with `i`.
	capsAuditModal CapsAuditModal
	// auditPins are tasks the caps audit pointed the tree at (a jump target, or
	// a marked bit's holders) and their creator chains, kept in the task table
	// past its history cut until the next jump, t, or T.
	auditPins map[string]bool
	// allFinished is H: the task table lists every finished task rather than
	// the newest verb.FinishedDefault. snapshotOmitted is the last snapshot's
	// FinishedOmitted, what the server left out of it.
	allFinished     bool
	snapshotOmitted uint32

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
	workspace      *workspace.Workspace
	workspaceArmed bool
	workspaceFile  *workspace.File
	workspacePath  string
	// loadWorkspaceFile reads .harness/config the way start-up did. Every
	// workspace operation calls it again (reloadWorkspace), so a hand edit made
	// while the TUI runs is what the next operation sees. A field so tests can
	// read a temp file without the process environment deciding.
	loadWorkspaceFile func() (*workspace.File, string, error)
	workspaceSaveName string // non-empty while a `workspace save` waits for the forward snapshot
	workspaceSaveAll  bool   // that save was `--all`: write without opening the picker
	// workspacePicker chooses WHICH tasks a save records, and their resume /
	// runner. See its doc comment for why no automatic rule replaced it.
	workspacePicker WorkspacePickerModel
	// workspacePickerBase is the target workspace's rendered block from the
	// read the picker was built on (nil when it did not exist). A commit whose
	// fresh read differs refuses to write: the rows would revert the change.
	workspacePickerBase []byte
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
	// watchPicker asks `w`/`W` what to wake on; lastWatch is the last pick,
	// where its cursor starts next time.
	watchPicker    WatchPicker
	lastWatch      string
	activeForwards map[int]*PortForwardSession // keyed by client-side unique id
	// pendingKeep holds, per client-side forward id, the workspace a start
	// modal's keep will write into once that forward registers.
	pendingKeep   map[int]string
	nextForwardID int
	// x11Reserved holds the display numbers handed to an X11 open whose
	// InteractiveReadyMsg has not arrived yet; the number then lives on its
	// ForwardX11 entry instead. See pickX11Display.
	x11Reserved map[int]bool
	// x11RestartAsk is the task (32 hex) whose D restart awaits y/n; "" =
	// none. x11Restarts holds the ones whose cancel was sent and whose X11
	// resume waits for a finished status (advanceX11Restarts).
	x11RestartAsk string
	x11Restarts   map[string]bool

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

	// runnerPicker shows the candidates of an ambiguous spawn; a pick runs the
	// Retry the ambiguous result carried (InteractiveReadyMsg /
	// SessionStartedMsg).
	runnerPicker RunnerPickerModel

	client  *cli.Client
	appCtx  context.Context
	program *tea.Program
	// termReleased is true while a child (an attached session, the external
	// editor) owns the terminal via execWithoutSuspend. Written only on the
	// Update goroutine: set before returning the Cmd, cleared by that Cmd's
	// done message. Gates every further handover, since ReleaseTerminal /
	// RestoreTerminal do not nest.
	termReleased bool

	// sessionCaps is the default capability mask applied to every spawn
	// (session new, --oneshot included) issued from this TUI session.
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
	// sessionGrantConditions are the bodies of sessionOverrides' conditioned
	// overrides; they travel together (grant conditions C4, C6).
	sessionGrantConditions []protocol.GrantCondition
	// sessionDelegateCaps / sessionDelegateScope are the default delegation
	// bound for spawns from this session (`caps set-defaults --delegate-*`).
	// Zero is the server's default bound: the children delegate nothing.
	sessionDelegateCaps  protocol.Capability
	sessionDelegateScope protocol.TaskScope
	// sessionSpawnDepth / sessionMaxDescendants are the default spawn limits
	// for spawns from this session (`caps set-defaults --spawn-depth …`).
	// Zero is unbounded. A resume never re-grants them.
	sessionSpawnDepth     protocol.SpawnLimit
	sessionMaxDescendants protocol.SpawnLimit
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
// The delegation bound follows the same rule, one half per flag.
func (a App) spawnAuthority(v verb.SpawnAction, caps protocol.Capability) Authority {
	explicit, overrides, resumeTaskID := v.Scope, v.Overrides, v.ResumeTaskID
	auth := a.authority()
	auth.Caps = caps
	if v.DelegateCaps != nil {
		auth.DelegateCaps = *v.DelegateCaps
		auth.DelegateCapsPresent = resumeTaskID != ""
	}
	if v.DelegateScope != nil {
		auth.DelegateScope = *v.DelegateScope
		auth.DelegateScopePresent = resumeTaskID != ""
	}
	if v.SpawnDepth != nil {
		auth.SpawnDepth = *v.SpawnDepth
		auth.SpawnDepthPresent = resumeTaskID != "" && v.SpawnDepthPresent
	}
	if v.MaxDescendants != nil {
		auth.MaxDescendants = *v.MaxDescendants
		auth.MaxDescendantsPresent = resumeTaskID != "" && v.MaxDescendantsPresent
	}
	// The scope half is one unit: naming EITHER --scope or --scope-for makes
	// the whole half explicit, so a resume re-grants both together. Letting
	// --scope-for alone ride the session default's scope would write an
	// authority that is half typed and half inherited.
	if explicit != nil || len(overrides) > 0 {
		if explicit != nil {
			auth.Scope = *explicit
		}
		auth.Overrides = overrides
		// The conditions belong to those overrides (grant conditions C4), so
		// they travel with them and never with the session default's.
		auth.GrantConditions = v.GrantConditions
		auth.ScopePresent = resumeTaskID != ""
	}
	return auth
}

// pendingInteractive captures what an interactive open needs so a runner-picker
// selection can re-issue it. repo is "" for resume (server reuses the task's
// repo); resumeTaskID is "" for a fresh session.
// authority is the session default a spawn carries when the command line did
// not name its own: the caps mask paired with the target scope.
// Its presence bits are all false: on a resume it re-grants nothing.
func (a *App) authority() Authority {
	return Authority{Caps: a.sessionCaps, Scope: a.sessionScope, Overrides: a.sessionOverrides, GrantConditions: a.sessionGrantConditions,
		DelegateCaps: a.sessionDelegateCaps, DelegateScope: a.sessionDelegateScope,
		SpawnDepth: a.sessionSpawnDepth, MaxDescendants: a.sessionMaxDescendants}
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
		server:            cfg.Server,
		principal:         principalLabel(),
		defaultRepo:       cfg.DefaultRepo,
		workspaceFile:     cfg.WorkspaceFile,
		workspacePath:     cfg.WorkspacePath,
		loadWorkspaceFile: func() (*workspace.File, string, error) { return workspace.Load(cfg.ConfigPath) },
		configPath:        cfg.ConfigPath,
		runners:           NewRunners(),
		tasks:             NewTasks(),
		schedules:         NewSchedules(),
		credentials:       NewCredentials(),
		detail:            NewDetailPopup(),
		notify:            NewNotify(),
		cmdresult:         NewCmdResult(),
		cmdline:           cmd,
		cmdHistoryIndex:   -1,
		popup:             NewPopup(cfg.DefaultRepo),
		bgform:            NewBgForm(),
		recordKeep:        NewRecordKeepForm(),
		filepicker:        NewFilePicker(),
		fileEditor:        NewFileEdit(),
		connsModal:        NewConnsModal(),
		overview:          NewOverviewModel(),
		forwardsModal:     NewForwardsModal(),
		execsModal:        NewExecsModal(),
		actionsModal:      NewActionsModal(),
		actionRunForm:     NewActionRunForm(),
		runnerKeysModal:   NewRunnerKeysModal(),
		idleWatchersModal: NewIdleWatchersModal(),
		capsAuditModal:    NewCapsAuditModal(),
		rawModal:          NewRawConnectModal(),
		boardModal:        NewBoardModal(),
		gitModal:          NewGitModal(),
		grid:              NewGridModel(),
		focus:             focusTasks,
		connected:         false,
		status:            "connecting…",
		tasksByID:         map[string]protocol.TaskInfo{},
		actRecvAt:         map[string]time.Time{},
		progressRecvAt:    map[string]time.Time{},
		lifecycleRecvAt:   map[string]time.Time{},
		authorityRecvAt:   map[string]time.Time{},
		activeForwards:    map[int]*PortForwardSession{},
		x11Reserved:       map[int]bool{},
		x11Restarts:       map[string]bool{},
		pendingKeep:       map[int]string{},
		sessionCaps:       protocol.Capability_None,
		sessionScope:      protocol.TaskScope{Base: protocol.ScopeBase_Subtree},
	}
	a.schedules.liveStatus = func(id protocol.TaskID) (protocol.TaskStatus, bool) {
		t, ok := a.tasksByID[FormatTaskID(id)]
		return t.Status, ok
	}
	// setFocus, not tasks.Focus(): a.focus must agree with the pane that is
	// focused, or key scope and the shown table start out as the runners'.
	a.setFocus(focusTasks)
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

// BindProgram stores the tea.Program so subscriber goroutines can dispatch
// their messages back to the model.
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
		// The Last column's age reads the clock, and a schedule that is not
		// firing sends no event.
		if a.shownTable() == focusSchedules && len(a.schedules.rows) > 0 {
			a.schedules.rebuild()
		}
		// The execs modal's `last` / `age` read the clock too, and a wedged
		// exec sends no event to refresh them.
		if a.execsModal.IsOpen() {
			a.execsModal.Reage()
		}
		return a, actAgeTick()

	case gridTickMsg:
		// Repaint pump for the live session viewer grid: re-render
		// unconditionally (v1 has no dirty-flag gating — 10Hz over ≤9 small
		// crops is cheap) and re-arm only while the grid stays open.
		if a.grid.IsOpen() {
			paneTasks := a.gridPaneTasks()
			a.grid.SetLifecycle(gridLifecycleBadges(paneTasks))
			a.grid.SetAbout(gridPaneAbouts(paneTasks))
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
		a.snapshotOmitted = msg.FinishedOmitted
		a.tasksByID = make(map[string]protocol.TaskInfo, len(msg.Tasks))
		now := time.Now()
		a.actRecvAt = make(map[string]time.Time, len(msg.Tasks))
		a.progressRecvAt = make(map[string]time.Time, len(msg.Tasks))
		a.lifecycleRecvAt = make(map[string]time.Time, len(msg.Tasks))
		a.authorityRecvAt = make(map[string]time.Time, len(msg.Tasks))
		for _, t := range msg.Tasks {
			id := FormatTaskID(t.Id)
			a.tasksByID[id] = t
			if t.LastOutputAt > 0 {
				a.actRecvAt[id] = now
			}
			if t.ProgressAt != 0 {
				a.progressRecvAt[id] = now
			}
			if t.LifecycleAt != 0 {
				a.lifecycleRecvAt[id] = now
			}
			if t.AuthorityAt != 0 {
				a.authorityRecvAt[id] = now
			}
		}
		a.refreshTasksTable()
		a.schedules.rebuild()
		if a.overview.IsOpen() {
			a.rebuildOverview()
		}
		listCmd := a.requestScheduleList()
		if a.workspaceArmed {
			a.workspaceArmed = false
			return a, tea.Batch(listCmd, a.applyWorkspace(), a.advanceX11Restarts())
		}
		return a, tea.Batch(listCmd, a.advanceX11Restarts())

	case TaskEventMsg:
		id := FormatTaskID(msg.Event.TaskId)
		if msg.Event.Kind == protocol.StatusEventKind_TaskPruned {
			// Server forgot this task — drop its row immediately instead of
			// waiting for the next incidental snapshot refresh.
			delete(a.tasksByID, id)
			delete(a.actRecvAt, id)
			delete(a.progressRecvAt, id)
			delete(a.lifecycleRecvAt, id)
			delete(a.authorityRecvAt, id)
			a.refreshTasksTable()
			a.schedules.rebuild()
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
			a.applyEventProgress(&ti, id, msg.Event)
			applyEventSchedule(&ti, msg.Event)
			a.tasksByID[id] = ti
			a.refreshTasksTable()
			a.schedules.rebuild()
			return a, tea.Batch(a.refreshSnapshot(), a.scheduleListFor(msg.Event))
		}
		cur.Status = msg.Event.TaskStatus
		cur.Kind = msg.Event.TaskKind
		if msg.Event.Kind == protocol.StatusEventKind_TaskEnded {
			cur.ExitCode = msg.Event.ExitCode
			cur.EndedAt = msg.Event.Ts
		}
		a.applyEventAct(&cur, id, msg.Event)
		a.applyEventObservers(&cur, msg.Event)
		a.applyEventProgress(&cur, id, msg.Event)
		applyEventSchedule(&cur, msg.Event)
		a.tasksByID[id] = cur
		a.refreshTasksTable()
		a.schedules.rebuild()
		return a, tea.Batch(a.scheduleListFor(msg.Event), a.advanceX11Restarts())

	case RunnerEventMsg:
		// RunnerStatusEvent now carries the runner's identity, but a registered
		// runner still needs the snapshot: the event has no Hostname,
		// AllowedRoots, MaxTasks or AgentProfiles, so there is no row to build
		// from it. Offline could key on RunnerId alone; it is not split out
		// because the two arrive at the same rate (once per runner lifecycle).
		//
		// A runner appearing is also the event that can make a failed workspace
		// resume succeed, so consume the one armed retry here. Armed only by a
		// failure, never standing: a workspace re-applied on EVERY runner event
		// would reopen the grid overlay under the operator.
		if a.workspaceRetryOnRunner && a.workspace != nil {
			a.workspaceRetryOnRunner = false
			a.ArmWorkspace()
		}
		return a, a.refreshSnapshot()

	case ConnSnapshotMsg:
		if msg.Err != nil {
			a.cmdresult.Append(WarnStyle.Render("conns snapshot: " + msg.Err.Error()))
			return a, nil
		}
		a.connsModal.ApplySnapshot(msg.Conns)
		return a, nil

	case TrsfStateMsg:
		// A reading that lands after the operator left the mode describes a
		// view that is gone. Dropping it also keeps the sampler's previous
		// reading from being set by something nobody saw.
		if !a.connsModal.IsOpen() || !a.connsModal.IsTrsf() {
			return a, nil
		}
		if msg.Err != nil {
			// The rows already on screen stay under it: a refused or failed
			// read says nothing about whether they were true when taken.
			a.connsModal.SetTrsfError(msg.Err)
			return a, nil
		}
		a.connsModal.ApplyTrsf(msg.Conns, msg.SampledAt)
		return a, nil

	case trsfTickMsg:
		// The tick IS the loop: it stops by not being rescheduled, which is
		// what closing the modal, leaving the mode, or a newer generation do.
		if msg.gen != a.trsfGen || !a.connsModal.IsOpen() || !a.connsModal.IsTrsf() {
			return a, nil
		}
		return a, tea.Batch(a.readTrsfOnce(), trsfTick(a.connsModal.TrsfEvery(), msg.gen))

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
		// The pane's `ws` cells are read from the file, so it is re-read
		// whenever the pane's rows are fetched: on open and on `r`.
		if a.forwardsModal.IsOpen() && a.reloadWorkspace("forwards") {
			a.refreshForwardMarks()
		}
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

	case TapLinesMsg:
		// Lines for a tap the operator already closed are dropped rather than
		// appended into whatever view replaced it.
		if a.tap.IsOpen() && a.tap.Subject() == msg.Subject {
			a.tap.Append(msg.Lines)
		}
		return a, nil
	case TapEndedMsg:
		if a.tap.IsOpen() && a.tap.Subject() == msg.Subject {
			if msg.Err != nil {
				a.tap.Append([]string{ErrorStyle.Render("tap ended: " + msg.Err.Error())})
			} else {
				a.tap.Append([]string{"-- tap ended --"})
			}
		}
		// The view stays up so the operator can read what it caught; only the
		// pump is finished.
		a.tapStop = nil
		if msg.Err != nil && !a.tap.IsOpen() {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("%s tap: %v", msg.Subject, msg.Err)))
		}
		return a, nil
	case ForwardStatusMsg:
		a.forwardsModal.ApplyEvent(msg.Event)
		a.refreshForwardMarks()
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
			return a, DoListForwards(a.client, false, cli.ForwardListQuery{})
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
		a.cmdresult.Append(execResultLine(msg.TaskID, msg.Label, msg.Result))
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

	case IdleWatcherListMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("await-idle ls: %v", msg.Err)))
			return a, nil
		}
		a.idleWatchersModal.ApplySnapshot(msg.Watchers)
		if msg.ToCmdresult {
			for _, line := range cli.AwaitIdleWatcherLines(msg.Watchers) {
				a.cmdresult.Append(line)
			}
		}
		return a, nil

	case IdleWatcherKillMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("await-idle kill %d: %v", msg.WatcherID, msg.Err)))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("killed await-idle watcher %d", msg.WatcherID)))
		// A killed row must leave the list, and only a fresh fetch can say so
		// — watchers have no push subscription.
		if a.idleWatchersModal.IsOpen() {
			return a, DoIdleWatcherList(a.client, "", false, nil)
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

	case OverviewFetchMsg:
		// A result for an opening the operator has left is dropped, and with
		// it the chain: only the current one re-arms the tick.
		if msg.gen != a.overview.Gen() || !a.overview.IsOpen() {
			return a, nil
		}
		a.overviewIn = msg
		a.overviewHave = true
		a.rebuildOverview()
		return a, overviewTick(msg.gen)

	case overviewTickMsg:
		if msg.gen != a.overview.Gen() || !a.overview.IsOpen() {
			return a, nil
		}
		if a.client == nil {
			// Disconnected: keep the chain alive so the overlay resumes
			// refreshing once the client is back.
			return a, overviewTick(msg.gen)
		}
		return a, DoOverviewFetch(a.client, msg.gen)

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

	case BoardChainsMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("chains: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplyChains(msg.Rows)
		return a, nil

	case BoardSubscribersMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("subscribers: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplySubscribers(msg.Topic, msg.Rows)
		return a, nil

	case BoardOlderMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("older: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.PrependOlder(msg.Topic, msg.Msgs)
		return a, nil

	case BoardMembershipMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(msg.Label())
		return a, DoBoardSubscribers(a.client, msg.Topic)

	case BoardContactsClearMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("clear-contacts: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(cli.ClearContactsLabel(msg.Task, msg.Topic, msg.Removed, msg.Found))
		return a, DoBoardSubscribers(a.client, msg.RefreshTopic)

	case BoardPurgeMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("purge: " + msg.Err.Error())
			return a, nil
		}
		if !msg.Found {
			a.boardModal.SetStatus("purge: not found")
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("purged %d msg(s)", msg.Purged))
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
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("retracted #%d (still readable here)", msg.Seq))
		// Re-read rather than edit the local copy: the message stays in this
		// view, moved to the withdrawn list, and only the server knows what it
		// looks like now.
		return a, DoBoardRead(a.client, msg.Topic)

	case BoardSendMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("send: " + msg.Err.Error())
			return a, nil
		}
		wake := "on"
		if msg.Params.NoWake {
			wake = "off"
		}
		dest := msg.Params.Topic
		if dest == "" {
			dest = fmt.Sprintf("reply to #%d", msg.Params.InReplyTo)
		}
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("sent #%d (%s, delivered_to=%d, wake %s)",
			msg.Result.Seq, dest, msg.Result.DeliveredTo, wake))
		// A reply lands on the author's topic, not necessarily the open one;
		// re-reading what is on screen still shows it where it belongs and
		// installs the status either way.
		if mode := a.boardModal.Mode(); mode == boardChainList || mode == boardChains {
			return a, DoBoardChains(a.client)
		}
		return a, DoBoardRead(a.client, a.boardModal.CurTopic())

	case BoardRetainMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("retain %s: %s", msg.Topic, msg.Label))
		return a, DoBoardTopics(a.client)

	case BoardFlushMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("flushed %s through seq %d (%d left memory; O pages them from disk)",
			msg.Topic, msg.Through, msg.Dropped))
		if a.boardModal.Mode() == boardMessages {
			return a, tea.Batch(DoBoardTopics(a.client), DoBoardRead(a.client, msg.Topic))
		}
		return a, DoBoardTopics(a.client)

	case BoardArchiveMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatus(fmt.Sprintf("archived %s → %s (%d msgs through seq %d; %s lists archives)",
			msg.Topic, msg.Row.ID, msg.Row.Count, msg.Row.Through, modalKeys.BoardArchives))
		return a, nil

	case BoardArchivesMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("archives: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.ApplyArchives(msg.Topic, msg.Rows)
		return a, nil

	case BoardArchiveReadMsg:
		switch {
		case msg.Err != nil:
			a.boardModal.SetStatus("archive " + msg.ID + ": " + msg.Err.Error())
		case !msg.Found:
			a.boardModal.SetStatus("archive " + msg.ID + " no longer exists")
		case msg.Older:
			a.boardModal.PrependArchiveOlder(msg.ID, msg.Msgs)
		default:
			a.boardModal.ApplyArchiveMessages(msg.ID, msg.Topic, msg.Msgs)
		}
		return a, nil

	case BoardArchivePurgeMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Err.Error())
			return a, nil
		}
		if !msg.Found {
			a.boardModal.SetStatus("archive " + msg.ID + ": nothing to purge")
			return a, nil
		}
		a.boardModal.SetStatusAfterRefresh(fmt.Sprintf("purged %d message(s) from archive %s", msg.Purged, msg.ID))
		if msg.Seq != 0 && a.boardModal.CurArchive() == msg.ID {
			// The status rides on the re-read, which ApplyMessages clears.
			a.boardModal.SetStatus(fmt.Sprintf("purged seq %d from archive %s", msg.Seq, msg.ID))
			return a, DoBoardArchiveRead(a.client, msg.ID, a.boardModal.CurTopic(), 0)
		}
		return a, DoBoardArchives(a.client, a.boardModal.ArchivesTopic())

	case BoardWakeMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus("wake: " + msg.Err.Error())
			return a, nil
		}
		a.boardModal.SetStatus(fmt.Sprintf("woke %d task(s) on %s", msg.Woken, msg.Topic))
		return a, nil

	case BoardThreadOpMsg:
		if msg.Err != nil {
			a.boardModal.SetStatus(msg.Verb + "-thread: " + msg.Err.Error())
			return a, nil
		}
		// Summary() carries every category with its count, zeros included, so
		// the status line says what happened rather than only that something
		// did. A run that stopped reports what completed first and the reason
		// after it -- the partial result is the useful half.
		status := msg.Verb + "-thread:" + msg.Result.Summary()
		if msg.Result.Err != nil {
			status += "  stopped: " + msg.Result.Err.Error()
		}
		a.boardModal.SetStatusAfterRefresh(status)
		// Re-collect rather than edit the local copy. After a retract the rows
		// are still here, marked; after a purge they are gone -- and which of
		// those happened is the server's to say.
		return a, DoBoardChains(a.client)

	case NotifyEventMsg:
		a.notify.Append(msg.Event)
		return a, nil

	case BindClientMsg:
		a.client = msg.Client
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
				return a, a.refreshSnapshot()
			}
		case msg.Topic == topics.RunnersStatus():
			if msg.Resubscribed && a.client != nil {
				return a, a.refreshSnapshot()
			}
		}
		return a, nil

	case ConnectionMsg:
		a.connected = msg.Connected
		switch {
		case msg.Connected:
			// Nothing to do. This and BindClientMsg originate from the
			// same PersistLoop reconnect, but this one is emitted first —
			// cli/persist.go calls emit(Connected) synchronously and only
			// then spawns the goroutine that sends BindClientMsg — so
			// a.client may still be the dead connection's here.
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

	case RecordListMsg:
		a.detail.SetRecordList(msg.TaskID, msg.Records, msg.Err)
		return a, nil
	case TranscriptListMsg:
		a.detail.SetTranscriptList(msg.TaskID, msg.Sessions, msg.Note, msg.Err)
		return a, nil
	case RecordKeepResultMsg:
		if a.recordKeep.IsOpen() && a.recordKeep.Ref() == msg.Ref {
			if msg.Err != nil {
				a.recordKeep.Answered(msg.Err)
				return a, nil
			}
			a.recordKeep.Close()
		}
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(msg.Err.Error()))
			return a, nil
		}
		a.cmdresult.Append(msg.Text)
		// The popup lists the new expiry.
		if id, _ := a.detail.Records(); a.detail.IsOpen() && id != "" && a.client != nil {
			return a, DoRecordList(a.client, id)
		}
		return a, nil
	case RecordResultMsg:
		if a.bgform.Pending() && a.bgform.IsSchedule() &&
			(msg.Verb == "schedule "+verb.SubNew || msg.Verb == "schedule "+verb.SubEdit) {
			a.bgform.Answered(msg.Err)
		}
		if msg.Err != nil {
			if msg.Line != "" {
				a.cmdresult.Append(ErrorStyle.Render(msg.Line))
			} else {
				a.cmdresult.Append(ErrorStyle.Render(msg.Verb + " failed: " + msg.Err.Error()))
			}
			// A failed schedule or credential verb may still have changed
			// something (a rm whose answer was lost): the tables follow the
			// server.
			if (strings.HasPrefix(msg.Verb, "schedule ") || strings.HasPrefix(msg.Verb, "credential ")) && a.client != nil {
				return a, a.refreshSnapshot()
			}
			return a, nil
		}
		for _, line := range strings.Split(msg.Text, "\n") {
			a.cmdresult.Append(line)
		}
		// A schedule verb changed (or may have changed) what the Schedules
		// table lists, and a run/rm moves a bound task too: refresh with the
		// snapshot, which carries both. A credential verb changed a task row
		// (an issue's name and expiry come only with a snapshot).
		if (strings.HasPrefix(msg.Verb, "schedule ") || strings.HasPrefix(msg.Verb, "credential ")) && a.client != nil {
			return a, a.refreshSnapshot()
		}
		return a, nil
	case ScheduleListMsg:
		a.schedListInFlight = false
		a.applySchedules(msg.Rows, msg.Err)
		if a.schedListPending {
			// Events arrived while that list was in flight: one more, whose
			// answer is later than all of them.
			a.schedListPending = false
			return a, a.requestScheduleList()
		}
		return a, nil
	case ScheduleShowMsg:
		a.onScheduleShowMsg(msg)
		return a, nil
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
			// A D restart whose cancel failed is over: nothing to resume.
			delete(a.x11Restarts, msg.Resolved)
			a.cmdresult.Append(ErrorStyle.Render("cancel failed: " + msg.Err.Error()))
			return a, nil
		}
		short := msg.Resolved
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("cancelled ") + short)
		return a, nil

	case x11ProbedMsg:
		return a, a.onX11Probed(msg)

	case x11RestartTimeoutMsg:
		if a.x11Restarts[msg.TaskID] {
			delete(a.x11Restarts, msg.TaskID)
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("D %s: the task did not finish within %s; nothing restarted", shortTaskID(msg.TaskID), x11RestartWait)))
		}
		return a, nil

	case SessionModeResultMsg:
		if a.bgform.Pending() {
			a.bgform.Answered(msg.Err)
		}
		short := shortTaskID(msg.TaskID)
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("session mode " + short + ": " + msg.Err.Error()))
			return a, nil
		}
		mode := "foreground"
		if msg.Background {
			mode = "background"
		}
		a.cmdresult.Append(OKStyle.Render("session "+short+" is now ") + mode)
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
			if d := protocol.AwaitIdleFiredDetail(msg.FiredBy, msg.Lifecycle, msg.ProgramState, msg.ProgramKind); d != "" {
				line += " (" + d + ")"
			} else if msg.LastOutputAt > 0 {
				line += " (last output " + formatNanoTs(msg.LastOutputAt) + ")"
			}
			a.cmdresult.Append(OKStyle.Render(line))
		case protocol.AwaitIdleStatus_Armed:
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("await-idle %s: armed (watcher %d)", short, msg.WatcherID)))
		case protocol.AwaitIdleStatus_Cancelled:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-idle %s: cancelled (watcher %d killed)", short, msg.WatcherID)))
		case protocol.AwaitIdleStatus_SessionStopped:
			a.cmdresult.Append(WarnStyle.Render("await-idle " + short + ": session stopped before going idle"))
		case protocol.AwaitIdleStatus_NotFound:
			a.cmdresult.Append(WarnStyle.Render("await-idle " + short + ": no live session for this task"))
		default:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-idle %s: %v", short, msg.Status)))
		}
		return a, nil

	case ProgressClearMsg:
		short := shortTaskID(msg.TaskID)
		switch {
		case msg.Err != nil:
			a.cmdresult.Append(ErrorStyle.Render("clear progress " + short + ": " + msg.Err.Error()))
		case msg.Status == protocol.ProgressSetStatus_Ok:
			a.cmdresult.Append(OKStyle.Render("cleared progress of " + short))
		default:
			a.cmdresult.Append(WarnStyle.Render("clear progress " + short + ": " + cli.ProgressSetStatusString(msg.Status)))
		}
		return a, nil

	case AwaitProgressResultMsg:
		short := shortTaskID(msg.TaskID)
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("await-progress " + short + ": " + msg.Err.Error()))
			return a, nil
		}
		switch msg.Status {
		case protocol.AwaitProgressStatus_Fired:
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("await-progress %s: %q", short, msg.Progress)))
		case protocol.AwaitProgressStatus_Armed:
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("await-progress %s: armed (watcher %d)", short, msg.WatcherID)))
		case protocol.AwaitProgressStatus_Cancelled:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-progress %s: cancelled (watcher %d killed)", short, msg.WatcherID)))
		case protocol.AwaitProgressStatus_TaskEnded:
			a.cmdresult.Append(WarnStyle.Render("await-progress " + short + ": task ended without a new line"))
		case protocol.AwaitProgressStatus_NotFound:
			a.cmdresult.Append(WarnStyle.Render("await-progress " + short + ": no such task"))
		default:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-progress %s: %s", short, cli.AwaitProgressStatusString(msg.Status))))
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
			if msg.SkippedActive > 0 {
				a.cmdresult.Append(WarnStyle.Render(cli.PruneActiveHint))
			}
		} else {
			a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("pruned %d task(s)", msg.Removed)))
		}
		return a, a.refreshSnapshot()

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
		return a, a.refreshSnapshot()

	case SetParentResultMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("set-parent failed: " + msg.Err.Error()))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(cli.SetParentMessage(msg.Opts, msg.Res)))
		return a, a.refreshSnapshot()

	case ActionResultMsg:
		a.appendRunnerKeyResult(RunnerKeyResultMsg(msg))
		// The Actions modal hides the results pane: say it there too.
		if a.actionsModal.IsOpen() && a.actionsModal.mode != actionsForm {
			if msg.Err != nil {
				a.actionsModal.status = fmt.Sprintf("%s: %v", msg.Verb, msg.Err)
			} else {
				a.actionsModal.status = strings.TrimSpace(msg.Text)
			}
		}
		return a, nil
	case ScheduleFormMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("schedule edit %s: %v", shortTaskID(msg.ID), msg.Err)))
			return a, nil
		}
		a.bgform.OpenScheduleEdit(msg.ID, msg.Form, msg.Auth)
		return a, nil
	case ActionViewsMsg:
		switch msg.For {
		case "modal":
			a.actionsModal.SetViews(msg.Views, msg.Err)
		case "run":
			a.actionRunForm.SetViews(msg.Views, msg.Err)
		case "picker":
			if a.authorityPicker.IsOpen() {
				if msg.Err != nil {
					a.authorityPicker.SetActionViewsErr(msg.Err)
				} else {
					a.authorityPicker.SetActionViews(msg.Views)
				}
			}
		}
		return a, nil
	case ActionFormResultMsg:
		switch {
		case msg.Err != nil:
			a.cmdresult.Append(ErrorStyle.Render(msg.Err.Error()))
		case msg.OK:
			a.cmdresult.Append(OKStyle.Render(strings.TrimSpace(msg.Text)))
		default:
			a.cmdresult.Append(ErrorStyle.Render(strings.TrimSpace(msg.Text)))
		}
		a.actionsModal.ApplyResult(msg)
		if msg.OK && a.client != nil {
			return a, DoActionViews(a.client, "modal")
		}
		return a, nil
	case RunnerKeyResultMsg:
		a.appendRunnerKeyResult(msg)
		// A change made from the Runner keys screen: back to its list, read
		// again, unless the server refused — then the form stays.
		// The modal hides the results pane, so the answer is shown in it.
		if a.runnerKeysModal.IsOpen() && msg.Verb != "runner keys" && msg.Verb != "runner dial ls" {
			switch {
			case msg.Err != nil:
				a.runnerKeysModal.formErr = fmt.Sprintf("%s: %v", msg.Verb, msg.Err)
			case msg.OK:
				a.runnerKeysModal.mode, a.runnerKeysModal.status = rkList, strings.TrimSpace(msg.Text)
			default:
				a.runnerKeysModal.formErr = strings.TrimSpace(msg.Text)
				if a.runnerKeysModal.mode == rkList {
					a.runnerKeysModal.status = a.runnerKeysModal.formErr
				}
			}
			if a.client != nil {
				return a, tea.Batch(DoRunnerKeyViews(a.client), a.refreshSnapshot())
			}
			return a, nil
		}
		if msg.Refresh {
			return a, a.refreshSnapshot()
		}
		return a, nil
	case RunnerKeyViewsMsg:
		if msg.Err != nil {
			a.runnerKeysModal.loadErr = msg.Err
			return a, nil
		}
		a.runnerKeysModal.SetViews(msg.Keys, msg.Dials)
		return a, nil
	case RunnerOfferMsg:
		if msg.Kind == "kick" {
			a.applyKickOffer(msg)
			if msg.Offer.OK {
				return a, a.refreshSnapshot()
			}
			return a, nil
		}
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render("runner unenroll: " + msg.Err.Error()))
			a.runnerKeysModal.ApplyOffer(cli.ForceOffer{})
			return a, nil
		}
		if msg.Offer.OK {
			a.cmdresult.Append(OKStyle.Render(strings.TrimSpace(msg.Offer.Text)))
		} else {
			a.cmdresult.Append(ErrorStyle.Render(strings.TrimSpace(msg.Offer.Text)))
		}
		a.runnerKeysModal.ApplyOffer(msg.Offer)
		if !msg.Offer.NeedsForce && a.client != nil {
			return a, tea.Batch(DoRunnerKeyViews(a.client), a.refreshSnapshot())
		}
		return a, nil

	case ServerDialResultMsg:
		line, ok := cli.FormatServerDialRunner(msg.RunnerCID, msg.Via, msg.Status)
		if msg.Err != nil {
			line, ok = fmt.Sprintf("server dial-runner %s: %v", msg.RunnerCID, msg.Err), false
		}
		// From the Runner keys screen's "dial once": answer in its form.
		if a.runnerKeysModal.IsOpen() && a.runnerKeysModal.mode == rkDialOnce {
			if ok {
				a.runnerKeysModal.mode, a.runnerKeysModal.status = rkList, line
			} else {
				a.runnerKeysModal.formErr = line
			}
		}
		if !ok {
			a.cmdresult.Append(ErrorStyle.Render(line))
			if msg.Err != nil {
				return a, nil
			}
		} else {
			a.cmdresult.Append(OKStyle.Render(line))
		}
		return a, a.refreshSnapshot()

	case FilePickerListingMsg:
		var pcmd tea.Cmd
		a.filepicker, pcmd = a.filepicker.Update(msg)
		return a, pcmd

	case FileEditRequestMsg:
		a.cmdresult.Append("loading " + msg.Rel + " for edit…")
		return a, DoFileEditLoad(a.client, msg.TaskID, msg.Rel,
			// The route is a command-line choice; the interactive widgets
			// take the default.
			protocol.DataPlaneRoute_Splice)

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
			return a, DoFileEditCreate(a.client, msg.TaskID, msg.Name, msg.Text, msg.Doc, protocol.DataPlaneRoute_Splice)
		}
		return a, DoFileEditCommit(a.client, msg.TaskID, msg.Doc, msg.Text, force, protocol.DataPlaneRoute_Splice)

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

	case GridOpenChatMsg:
		return a, a.openChat(msg.TaskID)

	case GridRunModeMsg:
		if t, ok := a.tasksByID[msg.TaskID]; ok {
			return a, a.switchRunMode(&t, msg.TaskID)
		}
		return a, nil

	case InteractiveReadyMsg:
		// The open is over, whatever its outcome: a number it was handed is
		// either on its ForwardX11 entry now (Started is sent first) or free.
		delete(a.x11Reserved, msg.X11Display)
		// Only the opens that offer a picker (`S` and the r/R/u/U resumes)
		// stamp a Retry on their result; every other path keeps an ambiguous
		// answer a flat error.
		if msg.Err != nil {
			var are *cli.AmbiguousRunnerError
			if msg.Retry != nil && errors.As(msg.Err, &are) {
				return a, a.runnerPicker.Open(are.Candidates, msg.Retry, a.client)
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
			if msg.Stream != nil {
				_ = msg.Stream.Close()
			}
			return a, nil
		}
		if msg.X11Warn != "" {
			a.cmdresult.Append(WarnStyle.Render("x11: " + msg.X11Warn))
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("attaching ") + short + " — Ctrl+] to detach client; Ctrl+D / `exit` ends the session")
		a.termReleased = true
		ex := &interactiveExec{stream: msg.Stream}
		return a, execWithoutSuspend(a.program, ex, func(err error) tea.Msg {
			return InteractiveDoneMsg{TaskID: msg.TaskID, Err: err, EndReason: ex.endReason}
		})

	case SessionStartedMsg:
		if a.bgform.Pending() {
			a.bgform.Answered(msg.Err)
		}
		if msg.Err != nil {
			// Only the starts that offer a picker (the `s` popup and `session
			// new -d`) stamp a Retry, as for InteractiveReadyMsg.
			var are *cli.AmbiguousRunnerError
			if msg.Retry != nil && errors.As(msg.Err, &are) {
				return a, a.runnerPicker.Open(are.Candidates, msg.Retry, a.client)
			}
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
		return a, a.refreshSnapshot()

	case InteractiveDoneMsg:
		a.termReleased = false
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		if msg.Err != nil {
			a.cmdresult.Append(WarnStyle.Render("interactive ended: ") + short + " (" + msg.Err.Error() + ")")
		} else {
			a.cmdresult.Append(OKStyle.Render("interactive ended: ") + short)
		}
		if msg.EndReason != "" {
			a.cmdresult.Append(WarnStyle.Render(short+": session ended: ") + msg.EndReason)
		}
		return a, a.refreshSnapshot()

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
		a.activeForwards[msg.ID] = &PortForwardSession{ID: msg.ID, TaskID: msg.TaskID, Direction: msg.Direction, Spec: msg.Spec, Cancel: msg.Cancel, ForwardID: msg.ForwardID, FromWorkspace: msg.FromWorkspace, X11Display: msg.X11Display}
		a.cmdresult.Append(OKStyle.Render("forward started: ") + pfShortID(msg.TaskID) + "  " + msg.Direction.flag() + " " + msg.Spec)
		if msg.ForwardID != 0 { // -R: the bind is confirmed and the id known now
			a.finishPendingKeep(msg.ID)
		}
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
		a.finishPendingKeep(msg.ID)
		return a, nil

	case PortForwardStoppedMsg:
		// The forward goroutine exited (stopped, or never started on bind
		// failure). Drop it so it no longer shows in the stop picker. If it was
		// already removed (e.g. the user killed it via the picker), this is a
		// no-op and we skip the duplicate log.
		delete(a.pendingKeep, msg.ID) // never registered: nothing to keep
		if s, ok := a.activeForwards[msg.ID]; ok {
			// A forward the runner or server closed ends its serve goroutine
			// but not the ctx it ran under, nor the status drainer on it.
			if s.Cancel != nil {
				s.Cancel()
			}
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
	a.overview.SetSize(a.width, a.height)
	a.forwardsModal.SetSize(a.width, a.height)
	a.tap.SetSize(a.width, a.height)
	a.execsModal.SetSize(a.width, a.height)
	a.idleWatchersModal.SetSize(a.width, a.height)
	a.capsAuditModal.SetSize(a.width, a.height)
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
	// `q` quits when not in the cmdline (which must accept a literal 'q').
	if a.focus != focusCmdline && msg.String() == mainKeys.Quit {
		return a, a.quit()
	}
	// `?` shows every binding. The footer is one row and drops what does
	// not fit (see footerHints), so this is where the full list lives.
	// Reuses the read-only DetailPopup — same Esc-closes / swallow-all
	// handling as the `d` detail view.
	if a.focus != focusCmdline && msg.String() == mainKeys.Help {
		a.detail.Open("keys", keyHelpBody())
		return a, nil
	}
	// Tab cycles focus over the shown table, notify, cmdresult and cmdline;
	// ←/→ switch between the three tables while one has focus (anywhere else
	// they are the pane's own: the command line's cursor).
	switch msg.Type {
	case tea.KeyTab:
		a.cycleFocus(+1)
		return a, nil
	case tea.KeyShiftTab:
		a.cycleFocus(-1)
		return a, nil
	case tea.KeyLeft, tea.KeyRight:
		if isTableFocus(a.focus) {
			d := +1
			if msg.Type == tea.KeyLeft {
				d = -1
			}
			a.setFocus(tableOrder[(tableIndex(a.focus)+d+len(tableOrder))%len(tableOrder)])
			return a, nil
		}
	}
	if cmd, ok := a.dispatchMainKey(msg); ok {
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
		// -h is a request that was answered. It arrived here as an error and
		// was rendered in ErrorStyle, which told an operator who asked for help
		// that they had made a mistake -- and, because the panel is narrower
		// than the terminal, folded it at the left margin. Both are fixed by
		// treating it as the output it is.
		var help *verb.HelpRequested
		if errors.As(err, &help) {
			a.cmdresult.Append("> " + input)
			for _, l := range help.Lines(a.cmdresult.Width()) {
				a.cmdresult.Append(l)
			}
			return a, nil
		}
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
	case focusSchedules:
		a.schedules, cmd = a.schedules.Update(msg)
	case focusCredentials:
		a.credentials, cmd = a.credentials.Update(msg)
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

// tableOrder is the four tables in strip order, the order ←/→ step through.
var tableOrder = []focus{focusRunners, focusTasks, focusSchedules, focusCredentials}

func tableIndex(f focus) int {
	for i, t := range tableOrder {
		if t == f {
			return i
		}
	}
	return 0
}

// tabStops is the Tab cycle: the four tables count as ONE stop (the table
// shown), then notify, cmdresult and cmdline.
const tabStops = 4

// focusStop is f's position in the Tab cycle.
func focusStop(f focus) int {
	switch f {
	case focusNotify:
		return 1
	case focusCmdresult:
		return 2
	case focusCmdline:
		return 3
	}
	return 0
}

func (a *App) cycleFocus(delta int) {
	switch (focusStop(a.focus) + delta + tabStops) % tabStops {
	case 0:
		a.setFocus(a.shownTable())
	case 1:
		a.setFocus(focusNotify)
	case 2:
		a.setFocus(focusCmdresult)
	default:
		a.setFocus(focusCmdline)
	}
}

// setFocus moves focus straight to one pane, blurring the rest.
func (a *App) setFocus(f focus) {
	a.runners.Blur()
	a.tasks.Blur()
	a.schedules.Blur()
	a.credentials.Blur()
	a.notify.Blur()
	a.cmdresult.Blur()
	a.cmdline.Blur()

	a.focus = f
	if isTableFocus(f) {
		a.lastTable = f
	}

	switch a.focus {
	case focusRunners:
		a.runners.Focus()
	case focusTasks:
		a.tasks.Focus()
	case focusSchedules:
		a.schedules.Focus()
	case focusCredentials:
		a.credentials.Focus()
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

// layout computes per-panel sizes from a.width / a.height. Header 1, table
// strip 1, notify border-inclusive 6, cmdresult border-inclusive 7, cmdline 1,
// footer 1 = 17 fixed rows; ONE table (runners, tasks, schedules or credentials, see shownTable) takes
// the rest, less its own 2 border rows, so tableHeight = a.height - 19 (5 at
// the 24-row minimum). All three tables are sized to the full width: only one is
// drawn, and the tasks table is the one that needs the room. The task log
// pane that used to sit under the tables held those rows (A3, A-D30).
//
// bubbles' tables scroll with the cursor, so the height is a window onto the
// rows, not a truncation of them. What DID need fixing once was a row the
// table held and did not draw (see setTableRows).
func (a *App) layout() {
	if a.width < 80 || a.height < 24 {
		return
	}
	tableHeight := a.height - 19
	a.runners.SetSize(a.width-2, tableHeight)
	a.tasks.SetSize(a.width-2, tableHeight)
	a.schedules.SetSize(a.width-2, tableHeight)
	a.credentials.SetSize(a.width-2, tableHeight)
	a.notify.SetSize(a.width-2, 4)
	a.cmdresult.SetSize(a.width-2, 5)
	a.cmdline.Width = a.width - 4
}

// shownTable is which table is drawn: the focused one, or — while focus is on
// notify / cmdresult / cmdline — the one focused last (tasks at startup).
func (a *App) shownTable() focus {
	if isTableFocus(a.focus) {
		return a.focus
	}
	return a.lastTable
}

// isTableFocus reports whether f is one of the four tables.
func isTableFocus(f focus) bool {
	return f == focusRunners || f == focusTasks || f == focusSchedules || f == focusCredentials
}

// keysToReach is the Tab / S-Tab (Shift-Tab) presses that take focus from one
// pane to another along the Tab cycle (tables → notify → cmdresult → cmdline →
// tables): the shorter direction, Tab on a tie, with a "*N" repeat count past
// one press. It reads focusStop, so the strip below cannot say a key that the
// cycle does not honour — an earlier strip said "Tab switches" and was wrong
// (the test presses every hint it prints). Terse on purpose: the hints and a
// fleet summary with two-digit counts share 80 cells.
func keysToReach(from, to focus) string {
	fwd := (focusStop(to) - focusStop(from) + tabStops) % tabStops
	back := tabStops - fwd
	key, n := "Tab", fwd
	if back < fwd {
		key, n = "S-Tab", back
	}
	if n > 1 {
		key += fmt.Sprintf("*%d", n)
	}
	return key
}

// tableNames are the tables as the tab bar names them, in full.
var tableNames = map[focus]string{focusRunners: "Runners", focusTasks: "Tasks", focusSchedules: "Schedules", focusCredentials: "Credentials"}

// tableStrip is the one line above the table: a tab bar naming every table in
// tableOrder — the order ←/→ step through — with the shown one bracketed, then
// the fleet summary after " | ". It carries no per-table key hints: ←/→ move to
// the neighbour in the order the bar lists.
//
// One hint stays. While focus is off the tables (notify / cmdresult / cmdline)
// the arrows are that pane's own, and Tab from there may not reach the tables
// in one press, so the shown table carries the keys that go back onto it from
// where focus IS, in place of its brackets: Tasks:Tab*2. An earlier strip said
// "Tab switches" and was wrong; the tests press what this one prints.
//
// Width, one decision at every terminal width: with every name in full and the
// longest realistic summary (two-digit counts and an offline bucket, 35
// cells) the bar is 76 cells on a table and 80 off one, under both East Asian
// width readings. Brackets and the keys together would be 82, which is why
// the keys replace the brackets rather than join them.
func tableStrip(shown, from focus, rs []protocol.RunnerInfo) string {
	entries := make([]string, 0, len(tableOrder))
	for _, f := range tableOrder {
		switch {
		case f == shown && isTableFocus(from):
			entries = append(entries, "["+tableNames[f]+"]")
		case f == shown:
			entries = append(entries, tableNames[f]+":"+keysToReach(from, shown))
		default:
			entries = append(entries, tableNames[f])
		}
	}
	return " " + strings.Join(entries, " ") + " | " + cli.RunnerSummary(rs)
}

func (a *App) View() string {
	if a.width < 80 || a.height < 24 {
		return "terminal too small (need at least 80x24)"
	}

	connectedTag := ErrorStyle.Render("DISCONNECTED")
	if a.connected {
		connectedTag = OKStyle.Render("CONNECTED")
	}
	header := HeaderStyle.Render(fmt.Sprintf("harness-tui · %s · %s · %s", a.server, connectedTag, a.principal))

	// One table at a time: the one the focus last rested on. The tab bar
	// above it names every table and carries the fleet summary, so the hidden
	// ones are never out of sight, out of mind.
	var top string
	switch a.shownTable() {
	case focusRunners:
		top = a.runners.View()
	case focusSchedules:
		top = a.schedules.View()
	case focusCredentials:
		top = a.credentials.View()
	default:
		top = a.tasks.View()
	}
	if a.runners.IsFocused() || a.tasks.IsFocused() || a.schedules.IsFocused() || a.credentials.IsFocused() {
		top = PanelStyleFocused.Render(top)
	} else {
		top = PanelStyle.Render(top)
	}
	strip := MutedStyle.Render(clipLine(tableStrip(a.shownTable(), a.focus, a.runnersSnapshot), 0, a.width))

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
	footer := FooterStyle.Render(clipLine(footerHints(a.focus, a.width), 0, a.width))
	if a.schedDeleteID != "" {
		// The question is on screen, not only in cmdresult, which may be
		// scrolled or narrow.
		footer = WarnStyle.Render(clipLine(schedDeletePrompt(a.schedDeleteID), 0, a.width))
	}
	if a.credRevokeID != "" {
		footer = WarnStyle.Render(clipLine(credRevokePrompt(a.credRevokeID), 0, a.width))
	}
	if a.kickAsk != nil {
		footer = WarnStyle.Render(clipLine(a.kickAsk.prompt, 0, a.width))
	}
	if a.x11RestartAsk != "" {
		footer = WarnStyle.Render(clipLine(x11RestartPrompt(a.x11RestartAsk), 0, a.width))
	}

	view := strings.Join([]string{
		header,
		strip,
		top,
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
	if a.bgform.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.bgform.View())
	}
	if a.popup.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.popup.View())
	}
	if a.recordKeep.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.recordKeep.View())
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
	if a.watchPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.watchPicker.View())
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
	if a.overview.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Left, lipgloss.Top, a.overview.View())
	}
	// The tap is opened from the execs modal and the forwards pane and is
	// drawn over both.
	if a.tap.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.tap.View())
	}
	if a.execsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.execsModal.View())
	}
	if a.actionRunForm.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.actionRunForm.View())
	}
	if a.actionsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.actionsModal.View())
	}
	if a.runnerKeysModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.runnerKeysModal.View())
	}
	if a.idleWatchersModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.idleWatchersModal.View())
	}
	if a.capsAuditModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.capsAuditModal.View())
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

// applySchedules takes a schedule list answer. Listing needs no capability (the
// caller's reach filters the rows, S-D17), so a failure is a transport or
// server problem: the table keeps its rows and the reason is said once per
// distinct text, not on every refresh.
func (a *App) applySchedules(rows []protocol.ScheduleRow, err error) {
	if err != nil {
		if e := err.Error(); e != a.schedErr {
			a.schedErr = e
			a.cmdresult.Append(WarnStyle.Render("schedules: could not list: " + e))
		}
		return
	}
	a.schedErr = ""
	a.schedules.SetRows(rows)
}

// applyEventSchedule copies the schedule binding from a task event into ti.
// EVERY task event carries it (a binding-change event, task_schedule, exists
// to say so), so it is overwritten rather than merged: an event with no
// binding unbinds the row, and the task popup follows without a list.
func applyEventSchedule(ti *protocol.TaskInfo, ev protocol.TaskStatusEvent) {
	id := ev.ScheduleId()
	if ev.SchedulePresent() && id != nil {
		ti.SetSchedulePresent(true)
		ti.SetScheduleId(*id)
		return
	}
	ti.SetSchedulePresent(false)
}

// applyEventProgress copies the progress line and the live title from a task
// event into ti. Every task event carries both (a parent-hop subscriber's
// arrive emptied, as its List row does), so the About column follows a
// progress write or a title change without a snapshot. It stamps the local
// receipt time the row's age is advanced from.
func (a *App) applyEventProgress(ti *protocol.TaskInfo, id string, ev protocol.TaskStatusEvent) {
	ti.SetProgress(ev.Progress)
	ti.ProgressAt = ev.ProgressAt
	ti.ProgressAgeMs = ev.ProgressAgeMs
	ti.SetLiveTerminal(ev.LiveTerminal())
	ti.SetTitle(ev.Title)
	ti.SetNoPty(ev.NoPty())
	if ev.ProgressAt != 0 {
		a.progressRecvAt[id] = time.Now()
	} else {
		delete(a.progressRecvAt, id)
	}
	// Lifecycle and the program's own report ride every event too.
	ti.Lifecycle = ev.Lifecycle
	ti.LifecycleAt = ev.LifecycleAt
	ti.LifecycleAgeMs = ev.LifecycleAgeMs
	ti.ProgramState = ev.ProgramState
	ti.ProgramKind = ev.ProgramKind
	ti.SetProgramMsg(ev.ProgramMsg)
	if ev.LifecycleAt != 0 {
		a.lifecycleRecvAt[id] = time.Now()
	} else {
		delete(a.lifecycleRecvAt, id)
	}
}

// agedTask is t with every wire age advanced by the local time elapsed since
// its receipt (wire age is server-clock, elapsed is local — no cross-host
// skew), because the TUI fetches nothing by itself between snapshots. Busy
// badges are NOT aged; they flip only via the server's idle-edge event. Every
// view of a task — the table, the d popup through it, the caps audit — reads
// through here, so none of them shows an age frozen at receipt.
func (a *App) agedTask(id string, t protocol.TaskInfo, now time.Time) protocol.TaskInfo {
	if t.LastOutputAt > 0 && time.Duration(t.OutputIdleMs)*time.Millisecond >= protocol.ActivityBusyThreshold {
		if rt, ok := a.actRecvAt[id]; ok {
			t.OutputIdleMs += uint64(now.Sub(rt) / time.Millisecond)
		}
	}
	if t.ProgressAt != 0 {
		if rt, ok := a.progressRecvAt[id]; ok {
			t.ProgressAgeMs += uint64(now.Sub(rt) / time.Millisecond)
		}
	}
	if t.LifecycleAt != 0 {
		if rt, ok := a.lifecycleRecvAt[id]; ok {
			t.LifecycleAgeMs += uint64(now.Sub(rt) / time.Millisecond)
		}
	}
	if t.AuthorityAt != 0 {
		if rt, ok := a.authorityRecvAt[id]; ok {
			t.AuthorityAgeMs += uint64(now.Sub(rt) / time.Millisecond)
		}
	}
	return t
}

// refreshTasksTable rebuilds the tasks table from tasksByID, sorted by
// descending CreatedAt: the rows protocol.ListKeep keeps (every live task, the
// newest finished ones H allows, their creators) plus the caps audit's pins.
// Idle badges are aged here: the
// rendered idle duration is the wire value plus the local time elapsed since
// receipt (wire age is server-clock, elapsed is local — no cross-host skew).
// Busy badges are NOT aged; they flip only via the server's idle-edge event.
func (a *App) refreshTasksTable() {
	all := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	now := time.Now()
	for id, t := range a.tasksByID {
		all = append(all, a.agedTask(id, t, now))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	// Every credential, ended ones included, before the history cut below:
	// `credential ls` lists them all, and a revoked one is what `r` re-keys.
	a.credentials.SetRows(all)
	// The cut is the server's own rule (protocol.ListKeep), applied again
	// here because the snapshot may hold more than the table shows: the
	// whole history while the caps audit is in use (snapshotFinished), and
	// tasks events added since. The audit's pins survive it: an old task it
	// pointed at is the case it exists for.
	limit := verb.FinishedDefault
	if a.allFinished {
		limit = verb.FinishedAll
	}
	n := len(all)
	keep, omitted := protocol.ListKeep(n, func(i int) protocol.ListRow {
		t := all[n-1-i] // ListKeep reads creation order, oldest first
		return protocol.ListRow{ID: FormatTaskID(t.Id), Creator: t.CreatorTaskId, Status: t.Status}
	}, func(int) bool { return true }, limit)
	kept := all[:0:0]
	for i, t := range all {
		switch {
		case keep[n-1-i]:
		case a.auditPins[FormatTaskID(t.Id)]:
			if protocol.FinishedStatus(t.Status) {
				omitted--
			}
		default:
			continue
		}
		kept = append(kept, t)
	}
	all = kept
	a.tasks.SetFinishedNote(finishedNote(a.allFinished, a.snapshotOmitted+omitted))
	a.tasks.SetRows(all, a.runnersSnapshot)
	if a.capsAuditModal.IsOpen() {
		a.capsAuditModal.SetTasks(a.auditTasks())
	}
}

// snapshotFinished is the bound this TUI's snapshots ask for: every finished
// task when H asked for them, and also while the caps audit is open or has
// pinned rows, because the audit counts ended tasks however old (a resume
// brings their caps back). In that second case the table still cuts to the
// default itself (refreshTasksTable), so only the audit sees the rest.
func (a *App) snapshotFinished() uint32 {
	if a.allFinished || a.capsAuditModal.IsOpen() || len(a.auditPins) > 0 {
		return verb.FinishedAll
	}
	return verb.FinishedDefault
}

// refreshSnapshot is RefreshSnapshot with this TUI's bound: every snapshot
// goes through it, so none can ask for a different history than the table
// is showing.
func (a *App) refreshSnapshot() tea.Cmd { return RefreshSnapshot(a.client, a.snapshotFinished()) }

// finishedNote is the task table's last row: which bound the table is under,
// what it leaves out (omitted, finished tasks) and what H does about it.
// Present with nothing left out too, as `ls`'s last line is: "none left out"
// is what says the table is the whole history, and the bound is there before
// the history outgrows it.
func finishedNote(all bool, omitted uint32) string {
	switch {
	case all:
		return fmt.Sprintf("── every finished task listed (%s: newest %d only) ──", mainKeys.FinishedAll, verb.FinishedDefault)
	case omitted > 0:
		return fmt.Sprintf("── %d older finished task(s) not listed (%s lists them) ──", omitted, mainKeys.FinishedAll)
	}
	return fmt.Sprintf("── newest %d finished tasks kept, none older left out (%s: every one) ──", verb.FinishedDefault, mainKeys.FinishedAll)
}

// gridPaneTasks is the current, locally aged row of each task the grid shows.
func (a *App) gridPaneTasks() []protocol.TaskInfo {
	now := time.Now()
	ids := a.grid.PaneTaskIDs()
	out := make([]protocol.TaskInfo, 0, len(ids))
	for _, id := range ids {
		if t, ok := a.tasksByID[id]; ok {
			out = append(out, a.agedTask(id, t, now))
		}
	}
	return out
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
			"grid %s: no live session in this set (%d task(s) in it)", label, len(set))))
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
// runner snapshot, in stable (sorted) order — used to populate the one-shot
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
// snapshot, in stable (sorted) order — used to populate the one-shot popup's
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
// populate the one-shot popup's agent selector (§6 TUI compose flow). A
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

// cmdlinePlaceholder is the cmdline's ghost text: a width-limited SUMMARY, so
// unlike cmdlineHelpLines it does not name every verb. What it must not do is
// name one the cmdline cannot parse, which is what
// TestPlaceholderNamesNothingUnreachable checks -- the placeholder is the
// first thing an operator reads, and a verb listed there is a promise.
const cmdlinePlaceholder = "session / file / forward / conns / ssh-gateway / server / workspace / cancel / notify / prune / repo / caps / clear / help / quit"

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
		"commands: session / file / git / forward / exec / conns / grid / workspace / " +
			"cancel / notify / prune / restore / repo / caps / caps set-defaults / credential / refresh / clear / help / quit",
	}
	for _, path := range verb.PathsForSurface(verb.TUI) {
		sp, ok := verb.Lookup(strings.Fields(path)...)
		if !ok {
			continue
		}
		// Synopsis and description on separate lines. `session new`'s declared
		// flags run past 300 characters, and this panel is narrower than a
		// terminal -- one line carrying both put the description where a
		// reader never reaches it.
		// Synopsis and notes both come from the declaration now. The notes
		// used to live here in a tuiVerbHelp map, which is the shape that
		// needed a completeness test to stay in step with the table; a
		// TUI-scoped note in the table cannot fall behind the verb it
		// describes, because it IS the verb's declaration.
		lines := sp.For(verb.TUI).UsageLines()
		out = append(out, lines[0])
		for _, n := range lines[1:] {
			out = append(out, "    - "+n)
		}
	}
	return append(out, tuiKeyHelp...)
}

// principalLabel words what this process connects AS for the header.
//
// It asks cli for the same decision the handshake makes rather than inspecting
// the env here: a second reading of HARNESS_* is how the header would come to
// claim one thing while the connection is another.
func principalLabel() string {
	kind, tid := connauth.EffectiveClientKind(protocol.ClientKind_Tui)
	if kind != protocol.ClientKind_Agent && kind != protocol.ClientKind_Credential {
		return "operator"
	}
	hexID := hex.EncodeToString(tid.Id[:])
	if len(hexID) > 8 {
		hexID = hexID[:8]
	}
	if kind == protocol.ClientKind_Credential {
		return "credential " + hexID
	}
	return "agent " + hexID
}

// DoScheduleList fetches the schedule list as a command of its own, with its
// own budget: a server that does not answer the schedule kinds never replies,
// and inside RefreshSnapshot's budget that would hold every runners / tasks
// refresh.
func DoScheduleList(list func(context.Context) ([]protocol.ScheduleRow, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rows, err := list(ctx)
		return ScheduleListMsg{Rows: rows, Err: err}
	}
}

// requestScheduleList asks for a fresh list, one in flight at a time: a request
// made while one is out only marks a follow-up (handled when the answer
// lands), so a burst of task events costs at most two lists. nil when there is
// nothing to ask (no client) or a list is already out.
func (a *App) requestScheduleList() tea.Cmd {
	list := a.schedList
	if list == nil {
		if a.client == nil {
			return nil
		}
		list = a.client.ScheduleList
	}
	if a.schedListInFlight {
		a.schedListPending = true
		return nil
	}
	a.schedListInFlight = true
	return DoScheduleList(list)
}

// scheduleListFor is the list refresh a task event calls for: one that names a
// schedule binding, or is the binding changing (task_schedule, which an unbind
// sends with none). A later firing resumes the bound task, so its events are
// the only signal that the schedule's next / last / task status moved.
func (a *App) scheduleListFor(ev protocol.TaskStatusEvent) tea.Cmd {
	if ev.SchedulePresent() || ev.Kind == protocol.StatusEventKind_TaskSchedule {
		return a.requestScheduleList()
	}
	return nil
}

// resolveSchedulePrefix turns a typed schedule id prefix into the full 32 hex
// the server takes, over the rows the table lists (as resolveTaskIDPrefix does
// over tasks). A full id the table does not list yet (a schedule just created)
// is passed through for the server to judge.
func (a *App) resolveSchedulePrefix(prefix string) (string, string) {
	p := strings.ToLower(prefix)
	var matches []string
	for _, r := range a.schedules.rows {
		if id := hex.EncodeToString(r.Id.Id[:]); strings.HasPrefix(id, p) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		if _, err := hex.DecodeString(p); err == nil && len(p) == 32 {
			return p, ""
		}
		return "", "no schedule matches " + prefix
	case 1:
		return matches[0], ""
	default:
		return "", fmt.Sprintf("ambiguous prefix %q matches %d schedules", prefix, len(matches))
	}
}

// x11DisplayMax is the highest display number an X11 open may take; the verb
// table refuses --x11-display above it too (cli/verb/spawn.go).
const x11DisplayMax = 99

// pickX11Display hands out the lowest display number from preferred to
// x11DisplayMax that none of this TUI's X11 forwards holds and no open in
// flight was handed, and reserves it. Every number used to default to 10, so
// once a tunnel outlived its attach, a second X11 task on the same runner
// could not bind 6010. Another client's tunnels are invisible here; their
// collision is a bind failure, reported by the open.
func (a *App) pickX11Display(preferred int) (int, bool) {
	held := map[int]bool{}
	for _, s := range a.activeForwards {
		if s.Direction == ForwardX11 {
			held[s.X11Display] = true
		}
	}
	for n := preferred; n <= x11DisplayMax; n++ {
		if !held[n] && !a.x11Reserved[n] {
			a.x11Reserved[n] = true
			return n, true
		}
	}
	return 0, false
}

// allocForwardID hands out the next client-side forward registry id. Every
// site that starts a forward takes its id here: the start modal used to bump
// and then use the counter while the workspace apply and the X11 opens used
// and then bumped it, so a modal forward followed by one of those got the same
// id, and the second PortForwardStartedMsg overwrote the first entry.
func (a *App) allocForwardID() int {
	a.nextForwardID++
	return a.nextForwardID
}
