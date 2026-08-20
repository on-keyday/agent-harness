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

	// live session viewer grid (full-screen overlay, `g` key)
	grid GridModel

	// agentboard view
	boardModal BoardModal

	// read-only git view of a task's worktree
	gitModal GitModal
	// gitStatusToContent routes the NEXT status answer into the content
	// pane instead of only the worktree-row summary. Set when the operator
	// asks for the listing; a background refresh leaves it false.
	gitStatusToContent bool

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
func (a App) spawnAuthority(explicit *protocol.TaskScope, resumeTaskID string, caps protocol.Capability) Authority {
	auth := Authority{Caps: caps, Scope: a.sessionScope}
	if explicit != nil {
		auth.Scope = *explicit
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
	return Authority{Caps: a.sessionCaps, Scope: a.sessionScope}
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
}

func New(cfg Config) *App {
	cmd := textinput.New()
	cmd.Prompt = "> "
	cmd.Placeholder = "submit / interactive / session / file / server / cancel / notify / prune / repo / caps / clear / help / quit"
	cmd.CharLimit = 1024
	cmd.Width = 60
	a := &App{
		server:          cfg.Server,
		defaultRepo:     cfg.DefaultRepo,
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
			a.cmdresult.Append(WarnStyle.Render("forwards snapshot: " + msg.Err.Error()))
			return a, nil
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
		a.boardModal.ApplyMessages(msg.Topic, msg.Msgs, msg.Found)
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
			if a.client != nil {
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
		return a, DoFileEditLoad(a.client, msg.TaskID, msg.Rel)

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
			return a, DoFileEditCreate(a.client, msg.TaskID, msg.Name, msg.Text, msg.Doc)
		}
		return a, DoFileEditCommit(a.client, msg.TaskID, msg.Doc, msg.Text, force)

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
			return a, nil
		}
		short := msg.TaskID
		if len(short) > 12 {
			short = short[:12]
		}
		a.cmdresult.Append(OKStyle.Render("started detached: ") + short)
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
		a.activeForwards[msg.ID] = &PortForwardSession{ID: msg.ID, TaskID: msg.TaskID, Direction: msg.Direction, Spec: msg.Spec, Cancel: msg.Cancel, ForwardID: msg.ForwardID}
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

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.layout()
		a.filepicker.SetSize(a.width, a.height)
		a.fileEditor.SetSize(a.width, a.height)
		a.connsModal.SetSize(a.width, a.height)
		a.forwardsModal.SetSize(a.width, a.height)
		a.boardModal.SetSize(a.width, a.height)
		a.gitModal.SetSize(a.width, a.height)
		a.grid.SetSize(a.width, a.height)
		a.rawModal.SetSize(a.width, a.height)
		a.authorityPicker.SetSize(a.width, a.height)
		return a, nil

	case tea.KeyMsg:
		// Detail popup is read-only — Esc closes, all other keys swallowed
		// so cursor movement / 'q' / etc. don't leak through.
		if a.detail.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.detail.Close()
				return a, nil
			}
			// Everything else scrolls: the body can be taller than the screen
			// (`?` is), and a popup you cannot scroll hides its own top.
			var cmd tea.Cmd
			a.detail, cmd = a.detail.Update(msg)
			return a, cmd
		}
		// Connections modal: Esc closes; arrow keys scroll the table; all
		// other keys (q, s, etc.) are swallowed so they don't leak through.
		if a.connsModal.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.connsModal.Close()
				return a, nil
			}
			var cmd tea.Cmd
			a.connsModal, cmd = a.connsModal.Update(msg)
			return a, cmd
		}
		// Forwards list modal: Esc closes; `x` arms a y/n kill confirmation
		// for the selected row (server RPC via DoKillForward — works on any
		// visible forward, not just this TUI's own; the target may belong to
		// another operator's `harness-cli forward` session, so it is
		// deliberately not a direct kill — see BeginKillConfirm's doc
		// comment for the confirm-gate precedent). `j`/`k` are intentionally
		// left alone here so they reach the embedded table's own
		// LineDown/LineUp navigation instead of colliding with a destructive
		// action (this repo's convention: destructive full-screen-overlay
		// actions use x/X, e.g. the grid's dismiss and the file picker's
		// delete — never k, which is universally "up" in this layer).
		if a.forwardsModal.IsOpen() {
			if a.forwardsModal.IsConfirming() {
				switch msg.String() {
				case modalKeys.ConfirmYes, modalKeys.ConfirmYesUpper:
					if id, taskID, spec, ok := a.forwardsModal.ConfirmKill(); ok {
						return a, DoKillForward(a.client, id, taskID, spec)
					}
					return a, nil
				case modalKeys.ConfirmNo, modalKeys.ConfirmNoUpper, modalKeys.Escape:
					a.forwardsModal.CancelKillConfirm()
					return a, nil
				}
				// Swallow every other key while a kill is pending so the
				// table (and its j/k navigation) can't move — and nothing
				// else can be triggered — mid-confirm.
				return a, nil
			}
			if msg.Type == tea.KeyEsc {
				a.forwardsModal.Close()
				return a, nil
			}
			if msg.String() == modalKeys.ForwardKill {
				a.forwardsModal.BeginKillConfirm()
				return a, nil
			}
			var cmd tea.Cmd
			a.forwardsModal, cmd = a.forwardsModal.Update(msg)
			return a, cmd
		}
		// Live session viewer grid: full-screen overlay, intercepts ALL
		// keys when open (focus movement / Enter / x / Esc-q are handled
		// inside GridModel.Update).
		if a.grid.IsOpen() {
			var cmd tea.Cmd
			a.grid, cmd = a.grid.Update(msg)
			return a, cmd
		}
		// Board modal: two-mode overlay (topics / messages).
		// The App dispatches Do* cmds for Enter/r/x/X; the modal handles
		// table/viewport navigation itself.
		if a.boardModal.IsOpen() {
			if msg.Type == tea.KeyEsc {
				if m := a.boardModal.Mode(); m == boardMessages || m == boardSubscribers {
					a.boardModal.PopToTopics()
					return a, nil
				}
				a.boardModal.Close()
				return a, nil
			}
			if a.boardModal.Mode() == boardTopics {
				switch msg.Type {
				case tea.KeyEnter:
					topic := a.boardModal.SelectedTopicName()
					if topic != "" {
						return a, DoBoardRead(a.client, topic)
					}
					return a, nil
				}
				switch msg.String() {
				case modalKeys.BoardRefresh:
					return a, DoBoardTopics(a.client)
				case modalKeys.BoardPurgeTopic:
					topic := a.boardModal.SelectedTopicName()
					if topic != "" {
						return a, DoBoardPurge(a.client, topic, 0)
					}
					return a, nil
				case modalKeys.BoardSubscribers:
					topic := a.boardModal.SelectedTopicName()
					if topic != "" {
						return a, DoBoardSubscribers(a.client, topic)
					}
					return a, nil
				}
			} else if a.boardModal.Mode() == boardSubscribers {
				if msg.String() == modalKeys.BoardSubscribers {
					return a, DoBoardSubscribers(a.client, a.boardModal.CurTopic())
				}
			} else {
				// boardMessages mode
				switch msg.String() {
				case modalKeys.BoardPurgeMsg:
					seq := a.boardModal.SelectedMsgSeq()
					if seq != 0 {
						return a, DoBoardPurge(a.client, a.boardModal.CurTopic(), seq)
					}
					return a, nil
				case modalKeys.BoardRefresh:
					return a, DoBoardRead(a.client, a.boardModal.CurTopic())
				}
			}
			var cmd tea.Cmd
			a.boardModal, cmd = a.boardModal.Update(msg)
			return a, cmd
		}
		// Git modal: row picker over a diff viewport. The App dispatches the
		// keys that need a client (Enter / r / s); the modal owns selection,
		// the baseline and scrolling.
		if a.gitModal.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.gitModal.Close()
				return a, nil
			}
			taskID := a.gitModal.TaskID()
			if msg.Type == tea.KeyEnter {
				row := a.gitModal.SelectedRow()
				if row.Kind == gitRowSubrepo {
					// A [REPO] row is a destination, not content: re-root and
					// reload everything, because none of it belonged to the
					// repository we are leaving.
					a.gitModal.EnterSubrepo(row.Subrepo)
					a.gitModal.SetSize(a.width, a.height)
					return a, a.gitReload(taskID)
				}
				kind, target, rev := a.gitModal.GitQueryForRow(row)
				q := a.gitModal.Query()
				q.BaseRev = rev
				if kind == protocol.GitQueryKind_Show {
					q.Kind = protocol.GitQueryKind_Show
					a.gitModal.RecordContentQuery(q)
					return a, DoGitShow(a.client, taskID, q)
				}
				q.Target = target
				q.Kind = protocol.GitQueryKind_Diff
				a.gitModal.RecordContentQuery(q)
				return a, DoGitDiff(a.client, taskID, q)
			}
			switch msg.String() {
			case modalKeys.GitOpenFile:
				// Toggle: into the whole file, or back to the diff it came from.
				if a.gitModal.LeaveFileView() {
					q := a.gitModal.LastContentQuery()
					if q.Kind == protocol.GitQueryKind_Show {
						return a, DoGitShow(a.client, taskID, q)
					}
					return a, DoGitDiff(a.client, taskID, q)
				}
				fq, ok := a.gitModal.OpenFileQuery()
				if !ok {
					a.gitModal.SetError("no file here — scroll to a diff hunk, or the file was deleted")
					return a, nil
				}
				return a, DoGitFile(a.client, taskID, fq)
			case modalKeys.BoardRefresh:
				return a, a.gitReload(taskID)
			case modalKeys.GitStatus:
				a.gitStatusToContent = true
				return a, DoGitStatus(a.client, taskID, a.gitModal.Query())
			case modalKeys.GitUp:
				if !a.gitModal.LeaveSubrepo() {
					return a, nil
				}
				a.gitModal.SetSize(a.width, a.height)
				return a, a.gitReload(taskID)
			case modalKeys.GitSubmodule:
				a.gitModal.ToggleSubmodule()
				// Re-issue the current row so the toggle is visible at once
				// rather than on the next unrelated keypress.
				row := a.gitModal.SelectedRow()
				if row.Kind == gitRowSubrepo {
					return a, nil
				}
				kind, target, rev := a.gitModal.GitQueryForRow(row)
				q := a.gitModal.Query()
				q.BaseRev = rev
				if kind == protocol.GitQueryKind_Show {
					q.Kind = protocol.GitQueryKind_Show
					a.gitModal.RecordContentQuery(q)
					return a, DoGitShow(a.client, taskID, q)
				}
				q.Target = target
				q.Kind = protocol.GitQueryKind_Diff
				a.gitModal.RecordContentQuery(q)
				return a, DoGitDiff(a.client, taskID, q)
			}
			var cmd tea.Cmd
			a.gitModal, cmd = a.gitModal.Update(msg)
			return a, cmd
		}
		// The editor popup sits on top of the file picker (which is what
		// opens it), so it must claim keys first — the picker's block below
		// swallows everything it sees.
		if a.fileEditor.IsOpen() {
			var ecmd tea.Cmd
			a.fileEditor, ecmd = a.fileEditor.Update(msg)
			return a, ecmd
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
				agent := a.popup.Agent()
				extraArgs := a.popup.ExtraArgs()
				resumeID := a.popup.ResumeTaskID()
				resumeConversation := a.popup.ResumeConversation()
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
				return a, DoSubmitWithOpts(a.client, repo, prompt, host, extraArgs, resumeID, a.authority(), false, resumeConversation, agent)
			case tea.KeyTab:
				a.popup.CycleRepo(+1)
				return a, nil
			case tea.KeyShiftTab:
				a.popup.CycleHost(+1)
				return a, nil
			case tea.KeyCtrlA:
				a.popup.CycleAgent(+1)
				return a, nil
			case tea.KeyCtrlE:
				a.popup.ToggleFocus()
				return a, nil
			case tea.KeyCtrlR:
				a.popup.ToggleResumeConversation()
				return a, nil
			}
			var pcmd tea.Cmd
			a.popup, pcmd = a.popup.Update(msg)
			return a, pcmd
		}
		// Authority picker intercepts keys when open: j/k move, space
		// toggles, enter applies, esc cancels.
		if a.authorityPicker.IsOpen() {
			switch {
			case msg.Type == tea.KeyEsc:
				a.authorityPicker.Close()
				return a, nil
			case msg.Type == tea.KeyUp:
				a.authorityPicker.Move(-1)
				return a, nil
			case msg.Type == tea.KeyDown:
				a.authorityPicker.Move(1)
				return a, nil
			case msg.Type == tea.KeySpace:
				a.authorityPicker.Toggle()
				return a, nil
			case msg.Type == tea.KeyRunes:
				// Fast key-repeat (and paste) batches runes into ONE KeyMsg,
				// so compare per rune, not msg.String() — a "jjj" burst is
				// three moves, not an unknown key.
				for _, r := range msg.Runes {
					switch r {
					case 'j':
						a.authorityPicker.Move(1)
					case 'k':
						a.authorityPicker.Move(-1)
					case ' ':
						a.authorityPicker.Toggle()
					case 'A':
						// WebUI chip row's [all] / [none] quick-set, as keys.
						a.authorityPicker.SetAllCaps(true)
					case 'N':
						a.authorityPicker.SetAllCaps(false)
					}
				}
				return a, nil
			case msg.Type == tea.KeyEnter:
				if a.authorityPicker.Mode() == PickerModeParent {
					parentHex, _, swap, ok := a.authorityPicker.ParentChoice()
					target := a.authorityPicker.TargetID()
					a.authorityPicker.Close()
					if !ok {
						return a, nil
					}
					if a.client == nil {
						a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
						return a, nil
					}
					// ParentID == "" without Swap IS the detach form on the wire.
					return a, DoSetParent(a.client, cli.SetParentOpts{
						TaskID: target, ParentID: parentHex, Swap: swap,
					})
				}
				caps, spec, cascade, keep := a.authorityPicker.Result()
				mode, target := a.authorityPicker.Mode(), a.authorityPicker.TargetID()
				a.authorityPicker.Close()
				if mode == PickerModeSession {
					a.sessionCaps = caps
					if spec == "" {
						a.sessionScope = protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
					} else {
						sc, err := cli.ParseScope(spec)
						if err != nil {
							a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error()))
							return a, nil
						}
						a.sessionScope = sc
					}
					a.cmdresult.Append(OKStyle.Render("defaults set: ") + capsLabel(a.sessionCaps) +
						"  scope=" + cli.ScopeLabel(a.sessionScope))
					return a, nil
				}
				if a.client == nil {
					a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
					return a, nil
				}
				sc, err := cli.ParseScope(spec)
				if err != nil {
					// Unreachable for picker-built specs; surfaced rather
					// than swallowed in case the serializer regresses.
					a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error()))
					return a, nil
				}
				return a, DoSetCaps(a.client, cli.SetCapsOpts{
					TaskID: target, Caps: &caps, Scope: &sc,
					Cascade: cascade, KeepConns: keep,
				})
			}
			return a, nil
		}
		// Runner picker intercepts keys when open (digit picks, Esc cancels).
		if a.runnerPicker.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.runnerPicker.Close()
				a.cmdresult.Append(WarnStyle.Render("runner pick cancelled"))
				return a, nil
			}
			if c := a.runnerPicker.Pick(msg.String()); c != nil {
				p := a.pendingInteractive
				sel, agentProfile := pickerSelection(c)
				a.runnerPicker.Close()
				pickLabel := c.Hostname + "  " + c.Cid
				if agentProfile != "" {
					pickLabel += "  (" + agentProfile + ")"
				}
				a.cmdresult.Append(OKStyle.Render("pinned runner: ") + pickLabel)
				return a, DoOpenDetachableSession(a.client, p.repo, sel, p.extraArgs, p.resumeTaskID, p.auth, p.capsOverride, p.resumeConversation, agentProfile)
			}
			return a, nil
		}
		// Forward-stop picker intercepts keys when open (digit selects, Esc cancels).
		if a.forwardPicker.IsOpen() {
			if msg.Type == tea.KeyEsc {
				a.forwardPicker.Close()
				return a, nil
			}
			if sess := a.forwardPicker.Pick(msg.String()); sess != nil {
				a.forwardPicker.Close()
				return a, a.killLocalForward(sess)
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
		// Raw-connect modal intercepts ALL keys when open: the input line needs
		// every printable rune while composing a target or a send line, which
		// is also why the pane keys below are arrows and esc rather than
		// letters. Esc only HIDES — panes are connections, and closing one is
		// `x`. Enter is overloaded by which tab is selected: on [+ new] it
		// parses the entered spec and opens a pane tagged with a fresh
		// generation; on a live pane it sends the line.
		if a.rawModal.IsOpen() {
			// Three screens, three key sets. The list has no focused text
			// input, so a letter is free there (`n`); the other two take every
			// printable rune, which is why their actions are chords.
			switch a.rawModal.Mode() {
			case rawModeList:
				switch {
				case msg.Type == tea.KeyEsc:
					a.rawModal.Hide()
					return a, nil
				case msg.Type == tea.KeyEnter:
					a.rawModal.OpenSelected()
					return a, nil
				case msg.Type == tea.KeyCtrlX:
					a.rawModal.CloseSelectedPane()
					return a, nil
				case msg.String() == "n":
					a.rawModal.BeginNew()
					return a, nil
				}
				return a, a.rawModal.UpdateList(msg)

			case rawModeNew:
				switch msg.Type {
				case tea.KeyEsc:
					a.rawModal.BackToList()
					return a, nil
				case tea.KeyEnter:
					// Reported inside the modal: cmdresult is behind it.
					host, port, ok := a.rawModal.TargetOrError()
					if !ok {
						return a, nil
					}
					// Each attempt gets its own generation and its own pane, so
					// two attempts can never share one — which is what used to
					// let the loser's close tear down the winner.
					a.rawGenSeq++
					gen := a.rawGenSeq
					taskID := a.rawModal.TaskID()
					a.rawModal.AddPane(taskID, host, port, gen)
					return a, DoStartRawForward(a.client, taskID, host, port, gen, a.program)
				}
				var rmcmd tea.Cmd
				a.rawModal, rmcmd = a.rawModal.Update(msg)
				return a, rmcmd
			}

			// rawModeView.
			switch msg.Type {
			case tea.KeyEsc:
				a.rawModal.BackToList()
				return a, nil
			case tea.KeyCtrlX:
				a.rawModal.CloseSelectedPane()
				return a, nil
			case tea.KeyCtrlT:
				a.rawModal.ToggleForm()
				return a, nil
			case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
				if !a.rawModal.InForm() {
					return a, a.rawModal.ScrollViewport(msg)
				}
			}
			if a.rawModal.InForm() {
				switch msg.Type {
				case tea.KeyTab:
					a.rawModal.FormNextField()
					return a, nil
				case tea.KeyLeft, tea.KeyRight:
					d := 1
					if msg.Type == tea.KeyLeft {
						d = -1
					}
					a.rawModal.FormCycleMethod(d)
					return a, nil
				case tea.KeyEnter:
					if err := a.rawModal.SendForm(); err != nil {
						a.rawModal.SetActiveNote(err.Error())
					}
					a.rawModal.Refresh()
					return a, nil
				}
				return a, a.rawModal.UpdateForm(msg)
			}
			switch msg.Type {
			case tea.KeyCtrlR:
				a.rawModal.ToggleHex()
				return a, nil
			case tea.KeyCtrlO:
				a.rawModal.CycleNewline()
				return a, nil
			case tea.KeyEnter:
				if p := a.rawModal.ActivePane(); p != nil && p.live {
					// SendEntry applies the mode: hex sends exact bytes, text
					// appends the selected terminator. A hex typo is reported
					// on the pane and sends nothing.
					if err := a.rawModal.SendEntry(); err != nil {
						if strings.HasPrefix(err.Error(), "hex:") {
							a.rawModal.SetActiveNote(err.Error())
						} else {
							a.rawModal.MarkClosed(p.gen, "raw connect: "+err.Error())
						}
					}
					a.rawModal.Refresh()
				}
				return a, nil
			}
			var rmcmd tea.Cmd
			a.rawModal, rmcmd = a.rawModal.Update(msg)
			return a, rmcmd
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
		// `s` opens the submit popup when not in cmdline focus / filter edit.
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Submit {
			a.popup.SetRepoChoices(uniqueRepoPaths(a.runnersSnapshot), a.defaultRepo)
			a.popup.SetHostChoices(uniqueHostnames(a.runnersSnapshot))
			a.popup.SetAgentChoices(uniqueAgentProfiles(a.runnersSnapshot))
			a.popup.Open()
			return a, nil
		}
		// `C` (capital) opens the live connections view. It fetches the
		// initial snapshot via ConnListWith (long-lived client, no new dial)
		// and subscribes to conns.status for live updates. Esc closes.
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Conns {
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("conns: not connected"))
				return a, nil
			}
			a.connsModal.Open()
			a.connsModal.SetSize(a.width, a.height)
			return a, DoConnSnapshot(a.client)
		}
		// `f` opens the full-screen port-forward list: every forward visible to
		// this operator on the server (DoListForwards / ForwardsSnapshotMsg),
		// not just ones this TUI process started. Esc closes; `x` (then y/n)
		// kills the selected row (see the forwardsModal.IsOpen() key block
		// above). The tasks pane's P/B keys remain a shortcut for stopping
		// this TUI's own forwards, now routed through the same DoKillForward
		// RPC. false: this is the modal-refresh path, not `forward ls` — no
		// text dump into cmdresult (see ForwardsSnapshotMsg).
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Forwards {
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("forwards: not connected"))
				return a, nil
			}
			a.forwardsModal.SetSize(a.width, a.height)
			a.forwardsModal.Open()
			return a, DoListForwards(a.client, false)
		}
		// `g` opens the live session viewer grid: a full-screen overlay
		// tiling read-only PaneStreamers for the live interactive sessions,
		// replacing the task-list view (task-list model state is preserved
		// and restored on Esc/q — this is a full-screen takeover, not a
		// split). Reuses the long-lived client (no fresh dial) and never
		// sends a PTY size (the grid has no size authority).
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Grid {
			return a, a.openGrid(cli.GridAll, "", nil)
		}
		// `z` / `Z` open the same grid narrowed to the SELECTED task's subtree —
		// itself plus every task it spawned (z), or only what it spawned (Z),
		// for when that one session is already on screen in another terminal
		// and its workers are what is missing. Both narrow through cli.GridSet,
		// the same call behind the `grid` verb and the WebUI's button, so no
		// two surfaces can disagree about who is whose child.
		if a.focus == focusTasks && !logsEditing &&
			(msg.String() == mainKeys.GridSubtree || msg.String() == mainKeys.GridDescendants) {
			mode := cli.GridSubtree
			if msg.String() == mainKeys.GridDescendants {
				mode = cli.GridDescendants
			}
			anchor := a.tasks.SelectedID()
			if anchor == "" {
				a.cmdresult.Append(WarnStyle.Render("grid: no task selected"))
				return a, nil
			}
			return a, a.openGrid(mode, anchor, nil)
		}
		// `O` (capital) opens the agentboard topics view. Fetches the topic
		// list on open via DoBoardTopics (long-lived client, no new dial).
		// Enter drills into a topic; Esc closes or returns to the topic list.
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Board {
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("board: not connected"))
				return a, nil
			}
			a.boardModal.Open()
			a.boardModal.SetSize(a.width, a.height)
			return a, DoBoardTopics(a.client)
		}
		// `T` flips the task table between flat order and creator-tree order.
		// Purely local: the rows are already in hand, so it re-renders from the
		// last snapshot instead of waiting for the next poll — a toggle that
		// visibly does nothing for five seconds reads as broken.
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Tree {
			on := a.tasks.SetTree(!a.tasks.TreeMode())
			// Same geometry the layout pass uses; the column set changed, so
			// the widths have to be refitted before the rows are rebuilt.
			half := a.width / 2
			a.tasks.SetSize(a.width-half-2, 10)
			a.tasks.SetRows(a.tasks.Rows(), a.runnersSnapshot)
			mode := "flat"
			if on {
				mode = "creator tree"
			}
			a.cmdresult.Append("tasks: " + mode + " order")
			return a, nil
		}
		// `i` opens a new interactive PTY session in the default repo. The
		// dance is two-stage: the Cmd dispatches the RPC, the response arrives
		// as InteractiveReadyMsg, and Update then hands the terminal to the
		// PTY (suspend.go) after gating on termReleased. The session is
		// detachable (like `S`); `i` differs only in skipping the ambiguous-
		// runner picker. Reattach lives on `r` (see below).
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Interactive {
			return a, DoOpenInteractive(a.client, a.defaultRepo, a.authority())
		}
		// `S` (capital) opens a new detachable interactive PTY session in the
		// default repo (equivalent to `harness-cli session new`).
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Session {
			a.pendingInteractive = pendingInteractive{
				repo: a.defaultRepo, resumeTaskID: "", extraArgs: nil,
				auth: a.authority(), capsOverride: false,
			}
			a.pickerArmed = true
			return a, DoOpenDetachableSession(a.client, a.defaultRepo, cli.SelectorOpts{}, nil, "", a.authority(), false, false, "")
		}
		// `F` opens the file picker for the task currently focused in the
		// tasks pane. No-op when the tasks pane is not focused or no task
		// is selected (the cmdresult line explains).
		// `G` opens the read-only git view for the selected task: its commit
		// list, its uncommitted diff, and a baseline the operator can move.
		// Unlike the file picker this does NOT require a live worktree — a
		// finished task still answers through its retained harness/<id>
		// branch (server/git_query.go).
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.Git {
			if a.focus != focusTasks {
				a.cmdresult.Append(WarnStyle.Render("git: focus the tasks pane first"))
				return a, nil
			}
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("git: no task selected"))
				return a, nil
			}
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("git: not connected"))
				return a, nil
			}
			a.gitModal.Open(taskID)
			a.gitModal.SetSize(a.width, a.height)
			a.gitStatusToContent = false
			return a, a.gitReload(taskID)
		}
		if a.focus != focusCmdline && !logsEditing && msg.String() == mainKeys.FilePicker {
			if a.focus != focusTasks {
				a.cmdresult.Append(WarnStyle.Render("file picker: focus the tasks pane first"))
				return a, nil
			}
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("file picker: no task selected"))
				return a, nil
			}
			// Only a Running or Detached task has a worktree the runner can
			// reach; the server answers NoSuchTask for anything else
			// (server/file_transfer.go). Say so here rather than opening a
			// picker whose first listing fails.
			if t := a.tasks.SelectedTask(); t != nil && !taskSessionAlive(t.Status) {
				a.cmdresult.Append(WarnStyle.Render(
					"file picker: task is " + taskStatusStr(t.Status) + " — only Running or Detached tasks have a worktree"))
				return a, nil
			}
			cmd := a.filepicker.OpenFor(a.client, taskID)
			a.filepicker.SetSize(a.width, a.height)
			return a, cmd
		}
		// `w` arms an await-idle watcher on the selected task: the fire lands
		// as a result line in cmdresult when the session's output goes idle.
		// `W` routes the fire through the operator-notification egress
		// instead (notify feed + --notify-hook, e.g. the phone) — for when
		// you're about to walk away.
		if a.focus == focusTasks && !logsEditing && (msg.String() == mainKeys.AwaitIdle || msg.String() == mainKeys.AwaitIdleNotify) {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("await-idle: no task selected"))
				return a, nil
			}
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("await-idle: not connected"))
				return a, nil
			}
			sink := protocol.AwaitIdleSink_Reply
			if msg.String() == mainKeys.AwaitIdleNotify {
				sink = protocol.AwaitIdleSink_Notify
			} else {
				a.cmdresult.Append(fmt.Sprintf("await-idle %s: watching (result lands here when the session goes idle)…", shortTaskID(taskID)))
			}
			return a, DoAwaitIdle(a.appCtx, a.client, taskID, 0, sink, "")
		}
		// `d` opens the detail popup for the focused row (runners or tasks).
		if !logsEditing && msg.String() == mainKeys.Detail {
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
		if a.focus == focusTasks && msg.String() == mainKeys.Cancel {
			id := a.tasks.SelectedID()
			if id == "" {
				a.cmdresult.Append(WarnStyle.Render("no task selected"))
				return a, nil
			}
			return a, DoCancel(a.client, id, id)
		}
		// `a` re-grants the selected task's authority: it opens the authority
		// picker prefilled from the task's stored caps/scope. Opening needs
		// no client; applying goes through the picker's Enter handler.
		// Unconditionally available — a TUI connection is an operator
		// connection by construction (spec §7). The typed
		// `caps set <id> --caps/--scope` cmdline form remains for scripting.
		if a.focus == focusTasks && msg.String() == mainKeys.ReGrant {
			t := a.tasks.SelectedTask()
			if t == nil {
				a.cmdresult.Append(WarnStyle.Render("no task selected"))
				return a, nil
			}
			a.authorityPicker.SetSize(a.width, a.height)
			a.authorityPicker.OpenRegrant(*t, a.tasks.Rows())
			return a, nil
		}
		// `A` opens the same picker as a parent chooser: re-point the selected
		// task's parent link (root / swap / another task). Operator-only
		// server-side, like re-grant; applying goes through the picker's
		// Enter handler.
		if a.focus == focusTasks && msg.String() == mainKeys.SetParent {
			t := a.tasks.SelectedTask()
			if t == nil {
				a.cmdresult.Append(WarnStyle.Render("no task selected"))
				return a, nil
			}
			a.authorityPicker.SetSize(a.width, a.height)
			a.authorityPicker.OpenParent(*t, a.tasks.Rows())
			return a, nil
		}
		// `r` / `R` re-enter the selected session: reattach a live Detached
		// session, or resume a finished task into a new detachable session.
		// r resumes with --continue (keep claude's memory); R resumes fresh.
		// `u` / `U` are the same resume variants but intentionally skip the
		// assigned-runner preference so ambiguous runner selection can be
		// reopened even when the previous runner is still available.
		if a.focus == focusTasks && (msg.String() == mainKeys.ResumeAssignedContinue || msg.String() == mainKeys.ResumeAssignedFresh || msg.String() == mainKeys.ResumeAnyContinue || msg.String() == mainKeys.ResumeAnyFresh) {
			t := a.tasks.SelectedTask()
			unpinnedResume := msg.String() == mainKeys.ResumeAnyContinue || msg.String() == mainKeys.ResumeAnyFresh
			act := resumeReattachAction(t, msg.String() == mainKeys.ResumeAssignedContinue || msg.String() == mainKeys.ResumeAnyContinue)
			switch act.Kind {
			case actionReattach:
				if unpinnedResume {
					a.cmdresult.Append(WarnStyle.Render("u/U: pick a finished task to resume without assigned runner"))
					return a, nil
				}
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_Control)
			case actionResume:
				// repo is irrelevant on resume — the server reuses the task's
				// RepoPath and worktree branch. Prefer the runner the task last
				// ran on (t.AssignedTo) so resume stays one keypress even when
				// another runner ties on this repo's roots score. u/U deliberately
				// use Any instead, which can reopen the ambiguous runner picker.
				a.pendingInteractive = pendingInteractive{
					repo: "", resumeTaskID: a.tasks.SelectedID(),
					extraArgs: nil, auth: a.authority(), capsOverride: false,
					resumeConversation: act.ResumeConversation,
				}
				a.pickerArmed = true
				if unpinnedResume {
					// Unpinned (u/U): leave agentProfile unresolved — the
					// (runner, profile) picker (§4a) supplies both when the
					// combo set is ambiguous, exactly like the Any-selector
					// runner pin above.
					return a, DoOpenDetachableSession(a.client, "", cli.SelectorOpts{}, nil, a.tasks.SelectedID(), a.authority(), false, act.ResumeConversation, "")
				}
				// Pinned (r/R): default to the task's own recorded profile
				// (§4b) so the pinned path stays one keypress — no picker.
				return a, DoResumeSession(a.client, t.AssignedTo, nil, a.tasks.SelectedID(), a.authority(), false, act.ResumeConversation, string(t.AgentProfile))
			case actionNone:
				a.cmdresult.Append(WarnStyle.Render(act.Hint))
				return a, nil
			}
		}
		// `v` view-attaches the selected live session in read-only mode (no input sent).
		if a.focus == focusTasks && msg.String() == mainKeys.ViewOnly {
			act := resumeReattachAction(a.tasks.SelectedTask(), true)
			if act.Kind == actionReattach {
				return a, DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_View)
			}
		}
		// `p` / `b` open the local / remote port-forward modal for the selected task.
		if a.focus == focusTasks && (msg.String() == mainKeys.ForwardLocal || msg.String() == mainKeys.ForwardRemote) {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
				return a, nil
			}
			dir := ForwardLocal
			if msg.String() == mainKeys.ForwardRemote {
				dir = ForwardRemote
			}
			a.portForwardModal.OpenMode(taskID, dir)
			return a, nil
		}
		// `P` / `B` stop a local / remote forward for the selected task. With more
		// than one active, a digit picker is shown; with exactly one, kill now.
		// Both route through DoKillForward (killLocalForward) — the same RPC
		// the forwards modal's `x` (then y/n) and `forward kill` use, so there is
		// exactly one way to stop a forward.
		if a.focus == focusTasks && (msg.String() == mainKeys.ForwardLocalStop || msg.String() == mainKeys.ForwardRemoteStop) {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
				return a, nil
			}
			dir := ForwardLocal
			if msg.String() == mainKeys.ForwardRemoteStop {
				dir = ForwardRemote
			}
			sel := selectForwards(a.activeForwards, taskID, dir)
			switch len(sel) {
			case 0:
				a.cmdresult.Append(WarnStyle.Render("forward: no active " + dir.flag() + " forward for selected task"))
			case 1:
				return a, a.killLocalForward(sel[0])
			default:
				a.forwardPicker.Open(dir, sel)
			}
			return a, nil
		}
		// `t` opens the raw-connect modal for the selected task: a third way to
		// start a forward, alongside `p` (-L) and `b` (-R) above, for a client
		// endpoint that lives inside this TUI process rather than a socket. It
		// belongs beside the start keys, not behind `f` (the registry listing,
		// whose per-row action is kill) — see RawConnectModal's doc comment and
		// the rawModal field comment for why P/B don't apply to it.
		if a.focus == focusTasks && msg.String() == mainKeys.RawConnect {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("raw connect: no task selected"))
				return a, nil
			}
			a.rawModal.Show(taskID)
			a.rawModal.SetSize(a.width, a.height)
			return a, nil
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
	if a.authorityPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.authorityPicker.View())
	}
	if a.runnerPicker.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.runnerPicker.View())
	}
	if a.connsModal.IsOpen() {
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, a.connsModal.View())
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
	case QuitAction, ClearAction, HelpAction, RepoAction, CapsAction, ScopeAction, RefreshAction, TrsfDebugAction:
		// no client needed (Refresh/Trsf carry their own nil-client notice)
	default:
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
			return a, nil
		}
	}
	switch v := act.(type) {
	case QuitAction:
		return a, a.quit()
	case ClearAction:
		a.cmdresult.Clear()
		return a, nil
	case RefreshAction:
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("refresh: not connected"))
			return a, nil
		}
		a.cmdresult.Append("refreshing snapshot…")
		return a, RefreshSnapshot(a.client)
	case HelpAction:
		a.cmdresult.Append("commands: submit / interactive [--repo=PATH] / cancel <id> / notify <text> / prune [--before=DUR] [--force] [<task-id>...] / repo <path> / caps / scope / caps set <id> / refresh / clear / help / quit")
		a.cmdresult.Append("refresh (alias: sync)          - force a full runners+tasks snapshot re-sync now")
		a.cmdresult.Append("submit [--resume ID] [--resume-conversation] <prompt>  - submit/resume a task")
		a.cmdresult.Append("interactive [--resume ID] [--resume-conversation]      - open/resume interactive session (detachable)")
		a.cmdresult.Append("session new [--resume ID] [--resume-conversation]      - open/resume detachable interactive")
		a.cmdresult.Append("caps [<names>]              - show, or set the session-default capability mask for spawns (e.g. caps spawn,file_read / caps all / caps all,-spawn / caps none)")
		a.cmdresult.Append("scope [<spec>]              - show, or set the session-default TARGET scope for spawns: subtree (default) | none | global | [subtree+]ids:<id>[,<id>]")
		a.cmdresult.Append("caps set <id> [--caps N] [--scope S] [--cascade] [--keep-conns]")
		a.cmdresult.Append("                            - OPERATOR: re-grant a LIVE task's authority; effective on its next request, no restart. --cascade also clamps its descendants")
		a.cmdresult.Append("submit|interactive|session new --caps <names>  - capability mask for THIS spawn only, overriding the default; with --resume it re-grants that mask to the task")
		a.cmdresult.Append("notify [info|warn|error] <title> [<text>...]        - send a notification (shows in this feed + --notify-hook egress; keep it one line)")
		a.cmdresult.Append("session new [--detach] [--host NAME | --runner HEX | --ip ADDR] - open detachable interactive session (--detach: background, print id)")
		a.cmdresult.Append("session attach <id>         - reattach to a session")
		a.cmdresult.Append("session ls                  - list detachable sessions")
		a.cmdresult.Append("session kill <id>           - terminate a session")
		a.cmdresult.Append("session await-idle <id> [--threshold-ms N] [--notify | --topic T] - fire when the session's output goes idle (default: result line here; --notify: operator notification)")
		a.cmdresult.Append("grid [<task-id>...]         - live session viewer over exactly these tasks (also: g for all, z/Z for the selected task's subtree)")
		a.cmdresult.Append("grid --under <id> [--descendants] - that task's working set: its subtree PLUS the tasks its own scope names (ids:); --descendants leaves the task itself out")
		a.cmdresult.Append("git <task-id> log [--max N] [-- <path>]            - the task's commits (also: tasks-pane G)")
		a.cmdresult.Append("git <task-id> diff [--staged] [<base>] [<target>] [-- <path>] - revisions counted as git counts them: none=unstaged, one=<base> vs working tree, two=commit vs commit")
		a.cmdresult.Append("git <task-id> show [<rev>] [-- <path>]             - one commit and its diff")
		a.cmdresult.Append("git <task-id> status [-- <path>]                   - uncommitted and untracked paths (untracked appear in no diff)")
		a.cmdresult.Append("git <task-id> subrepos                             - git repos nested inside the worktree ([REPO] rows; Enter descends, u goes up)")
		a.cmdresult.Append("git <task-id> file [--staged|--rev R] <path>       - one file's whole content (also: o in the modal, from the diff you are reading)")
		a.cmdresult.Append("file ls <task-id> [<rel>]                          - list a directory in the task's worktree (root if rel omitted)")
		a.cmdresult.Append("file push [-r] [-f] [-p] <task-id> <local-src> <rel-dst>  - copy a local file/dir into the worktree (-r tar, -f overwrite, -p mkdir parents)")
		a.cmdresult.Append("file mkdir [-p] <task-id> <rel-dir>                - create a directory in the worktree (-p: mkdir -p)")
		a.cmdresult.Append("file pull [-r] [-f] [-o off] [-n len] <task-id> <rel-src> <local-dst>  - copy from the worktree to a local path")
		a.cmdresult.Append("file delete [-r [-f]] <task-id> <rel>              - remove a file (no -r) or directory (-r empty / -r -f recursive)")
		a.cmdresult.Append("file edit <task-id> <rel>                          - open a text file in the editor popup and push it back (ctrl+j save, ctrl+o $EDITOR)")
		a.cmdresult.Append("file new <task-id> <rel>                           - write a new text file in the editor popup and push it")
		a.cmdresult.Append("forward ls                                         - list every port forward visible to this operator (also: f key, kill: x then y/n)")
		a.cmdresult.Append("forward kill <forward-id>                          - close one registered forward by id (also: tasks-pane P/B on the owning task)")
		a.cmdresult.Append("server dial-runner <runner-cid>                    - ask the server to reverse-dial a Listen-mode runner (Phase A, ACL envs)")
		a.cmdresult.Append("F (tasks focus): open file picker — Enter/→ to descend a dir, Backspace/← to go back. e edit / n new / u push / g pull / d delete / D rm -rf. Esc closes.")
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
		// Only meaningful as a delta between two dumps: frozen = the run loop
		// is blocked (nothing is demuxed, so no stream ever becomes visible),
		// exploding = busy-spin, advancing slowly = congestion-blocked.
		a.cmdresult.Append(fmt.Sprintf("  loop: iterations=%d (run `trsf` twice — the delta is the signal)", st.LoopIterations))
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
	case CapsAction:
		if v.Show {
			// `caps` with no argument opens the session-default picker — it
			// shows the current value AND lets it be edited by selection.
			a.authorityPicker.SetSize(a.width, a.height)
			a.authorityPicker.OpenSession(a.sessionCaps, a.sessionScope, a.tasks.Rows())
		} else {
			a.sessionCaps = v.Caps
			a.cmdresult.Append(OKStyle.Render("caps set: ") + capsLabel(a.sessionCaps))
		}
		return a, nil
	case ScopeAction:
		if v.Show {
			// Same picker as `caps` — the session default is one caps+scope
			// pair, edited together.
			a.authorityPicker.SetSize(a.width, a.height)
			a.authorityPicker.OpenSession(a.sessionCaps, a.sessionScope, a.tasks.Rows())
		} else {
			a.sessionScope = v.Scope
			a.cmdresult.Append(OKStyle.Render("scope set: ") + cli.ScopeLabel(a.sessionScope))
		}
		return a, nil
	case SetCapsAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoSetCaps(a.client, cli.SetCapsOpts{
			TaskID: full, Caps: v.Caps, Scope: v.Scope,
			Cascade: v.Cascade, KeepConns: v.KeepConns,
		})
	case SetParentAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		parentFull := ""
		if v.ParentID != "" {
			parentFull, errStr = a.resolveTaskIDPrefix(v.ParentID)
			if errStr != "" {
				a.cmdresult.Append(ErrorStyle.Render(errStr))
				return a, nil
			}
		}
		return a, DoSetParent(a.client, cli.SetParentOpts{
			TaskID: full, ParentID: parentFull, Swap: v.Swap,
		})
	case InteractiveAction:
		caps, capsOverride := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
		return a, DoOpenInteractiveWithOpts(a.client, v.Repo, "", v.ExtraArgs, v.ResumeTaskID, a.spawnAuthority(v.Scope, v.ResumeTaskID, caps), capsOverride, v.ResumeConversation, v.AgentProfile)
	case SubmitAction:
		caps, capsOverride := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
		return a, DoSubmitWithOpts(a.client, v.Repo, v.Prompt, "", v.ExtraArgs, v.ResumeTaskID, a.spawnAuthority(v.Scope, v.ResumeTaskID, caps), capsOverride, v.ResumeConversation, v.AgentProfile)
	case CancelAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoCancel(a.client, v.IDPrefix, full)
	case PruneAction:
		if len(v.TaskIDs) > 0 {
			a.cmdresult.Append(fmt.Sprintf("prune: asking server to forget %d task id(s) (force=%t)", len(v.TaskIDs), v.Force))
		} else {
			a.cmdresult.Append(fmt.Sprintf("prune: cutoff = %s; asking server to forget terminal tasks", cli.FormatPruneCutoff(v.Before)))
		}
		return a, DoPruneTasks(a.client, v.Before, v.TaskIDs, v.Force)
	case SessionNewAction:
		repo := v.Repo
		if repo == "" {
			repo = a.defaultRepo
		}
		sel := cli.SelectorOpts{Host: v.Host, Runner: v.Runner, IP: v.IP}
		caps, capsOverride := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
		auth := a.spawnAuthority(v.Scope, v.ResumeTaskID, caps)
		if v.X11 {
			return a, DoOpenX11Session(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, v.X11Display, a.program, auth, capsOverride, v.ResumeConversation, v.AgentProfile)
		}
		if v.Detach {
			return a, DoStartDetachedSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.AgentProfile, TermSize{Rows: uint16(a.height), Cols: uint16(a.width)})
		}
		return a, DoOpenDetachableSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.AgentProfile)
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
	case SessionAwaitIdleAction:
		full, errStr := a.resolveTaskIDPrefix(v.IDPrefix)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		sink := protocol.AwaitIdleSink_Reply
		switch {
		case v.Notify:
			sink = protocol.AwaitIdleSink_Notify
		case v.Topic != "":
			sink = protocol.AwaitIdleSink_Board
		}
		if sink == protocol.AwaitIdleSink_Reply {
			a.cmdresult.Append(fmt.Sprintf("await-idle %s: watching (result lands here when the session goes idle)…", shortTaskID(full)))
		}
		// appCtx, not a round-trip timeout: the reply sink long-polls until
		// the session actually goes idle. The cmd goroutine carries it; the
		// UI stays fully interactive meanwhile.
		return a, DoAwaitIdle(a.appCtx, a.client, full, v.ThresholdMs, sink, v.Topic)
	case GridAction:
		// Prefixes are resolved HERE, before the set is built: cli.GridSet
		// compares full ids, and a half-typed one must be reported as
		// ambiguous rather than silently matching nothing.
		anchor := ""
		if v.Anchor != "" {
			full, errStr := a.resolveTaskIDPrefix(v.Anchor)
			if errStr != "" {
				a.cmdresult.Append(ErrorStyle.Render("grid: " + errStr))
				return a, nil
			}
			anchor = full
		}
		var ids []string
		for _, p := range v.IDs {
			full, errStr := a.resolveTaskIDPrefix(p)
			if errStr != "" {
				a.cmdresult.Append(ErrorStyle.Render("grid: " + errStr))
				return a, nil
			}
			ids = append(ids, full)
		}
		return a, a.openGrid(v.Mode, anchor, ids)
	case GitAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("git: not connected"))
			return a, nil
		}
		// The cmdline route lands in the same modal the G key opens, rather
		// than dumping a diff into the result pane where it cannot be scrolled
		// or navigated.
		a.gitModal.Open(full)
		a.gitModal.SetSize(a.width, a.height)
		a.gitStatusToContent = v.Sub == "status"
		// The modal's root and submodule setting come from the command line
		// here, and everything issued below reads them back off the modal — so
		// the keyboard route and the cmdline route cannot drift apart.
		a.gitModal.SetRoot(v.Subrepo, v.Submodule)
		if v.BaseRev != "" && v.Sub == "diff" {
			a.gitModal.SetBaseRev(v.BaseRev)
		}
		cmds := []tea.Cmd{a.gitReload(full)}
		switch v.Sub {
		case "log", "status", "subrepos":
			// gitReload already asked for all three.
		case "file":
			q := a.gitModal.Query()
			q.Path = v.Path
			switch {
			case v.TargetRev != "":
				q.Target = protocol.GitDiffTarget_Rev
				q.TargetRev = v.TargetRev
			case v.Staged:
				q.Target = protocol.GitDiffTarget_Index
			default:
				q.Target = protocol.GitDiffTarget_Worktree
			}
			cmds = append(cmds, DoGitFile(a.client, full, q))
		case "show":
			q := a.gitModal.Query()
			q.BaseRev = v.BaseRev
			q.Path = v.Path
			cmds = append(cmds, DoGitShow(a.client, full, q))
		default: // diff
			q := a.gitModal.Query()
			q.BaseRev = v.BaseRev
			q.TargetRev = v.TargetRev
			q.Path = v.Path
			q.Target = protocol.GitDiffTarget_Worktree
			switch {
			case v.Staged:
				q.Target = protocol.GitDiffTarget_Index
			case v.TargetRev != "":
				q.Target = protocol.GitDiffTarget_Rev
			}
			cmds = append(cmds, DoGitDiff(a.client, full, q))
		}
		return a, tea.Batch(cmds...)
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
		return a, DoFilePush(a.client, full, v.LocalSrc, v.RemoteDst, v.Recursive, v.Force, v.Parents)
	case FileMkdirAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFileMkdir(a.client, full, v.RelPath, v.Parents)
	case FileEditAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		rel := v.RelPath
		return a, func() tea.Msg { return FileEditRequestMsg{TaskID: full, Rel: rel} }
	case FileNewAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		// OpenNew seeds a directory; this caller knows the whole path.
		a.fileEditor.SetSize(a.width, a.height)
		a.fileEditor.OpenNew(full, "")
		a.fileEditor.SetName(v.RelPath)
		return a, nil
	case FilePullAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFilePull(a.client, full, v.RemoteSrc, v.LocalDst, v.Recursive, v.Force,
			cli.FileTransferRange{Offset: v.Offset, Length: v.Length})
	case FileDeleteAction:
		full, errStr := a.resolveTaskIDPrefix(v.TaskID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return a, nil
		}
		return a, DoFileDelete(a.client, full, v.RelPath, v.Recursive, v.Force)
	case ForwardLsAction:
		// true: this IS `forward ls` — the text dump is the whole point.
		return a, DoListForwards(a.client, true)
	case ForwardKillAction:
		// No task/spec context: the operator supplied only a bare id.
		return a, DoKillForward(a.client, v.ForwardID, "", "")
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
func (a *App) gitReload(taskID string) tea.Cmd {
	q := a.gitModal.Query()
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
