package tui

import (
	"fmt"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// dispatchMainKey runs the mainKeyBindings row that names msg, when that row
// applies to the focused pane. It reports false when no row claims the key, or
// the row's handler declines it, and the key then goes on to the pane.
//
// A row's Scope IS the guard: the key fires only in a pane its bits name.
// Pressed elsewhere it is not consumed — except that a row with an OutsideHint
// says why it did nothing, unless the focus is the command line, which types
// the letter. Nothing fires while the logs filter is being typed; that pane's
// one lettered binding is handled inside LogsModel.
func (a *App) dispatchMainKey(msg tea.KeyMsg, logsEditing bool) (tea.Cmd, bool) {
	if logsEditing {
		return nil, false
	}
	key := msg.String()
	scope := scopeForFocus(a.focus)
	for i := range mainKeyBindings {
		b := &mainKeyBindings[i]
		if b.Do == nil || !slices.Contains(b.Keys, key) {
			continue
		}
		if b.Scope&scope == 0 {
			if b.OutsideHint != "" && a.focus != focusCmdline {
				a.cmdresult.Append(WarnStyle.Render(b.OutsideHint))
				return nil, true
			}
			return nil, false
		}
		return b.Do(a, msg)
	}
	return nil, false
}

// `s` opens the submit popup when not in cmdline focus / filter edit.
func (a *App) onSubmit(msg tea.KeyMsg) (tea.Cmd, bool) {
	a.popup.SetRepoChoices(uniqueRepoPaths(a.runnersSnapshot), a.defaultRepo)
	a.popup.SetHostChoices(uniqueHostnames(a.runnersSnapshot))
	a.popup.SetAgentChoices(uniqueAgentProfiles(a.runnersSnapshot))
	a.popup.Open()
	return nil, true
}

// `C` (capital) opens the live connections view. It fetches the
// initial snapshot via ConnListWith (long-lived client, no new dial)
// and subscribes to conns.status for live updates. Esc closes.
func (a *App) onConns(msg tea.KeyMsg) (tea.Cmd, bool) {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("conns: not connected"))
		return nil, true
	}
	a.connsModal.Open()
	a.connsModal.SetSize(a.width, a.height)
	return DoConnSnapshot(a.client), true
}

// `f` opens the full-screen port-forward list: every forward visible to
// this operator on the server (DoListForwards / ForwardsSnapshotMsg),
// not just ones this TUI process started. Esc closes; `x` (then y/n)
// kills the selected row (inForwardsModal). The tasks pane's P/B keys
// remain a shortcut for stopping
// this TUI's own forwards, now routed through the same DoKillForward
// RPC. false: this is the modal-refresh path, not `forward ls` — no
// text dump into cmdresult (see ForwardsSnapshotMsg).
func (a *App) onForwards(msg tea.KeyMsg) (tea.Cmd, bool) {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("forwards: not connected"))
		return nil, true
	}
	a.forwardsModal.SetSize(a.width, a.height)
	a.forwardTap.SetSize(a.width, a.height)
	a.forwardsModal.Open()
	return DoListForwards(a.client, false), true
}

// `e` opens the full-screen running-exec list: every exec visible to
// this operator on the server, not just ones this TUI started. Esc
// closes; `x` (then y/n) kills the selected row through the same
// DoExecRunKill the cmdline verb uses. The task pane's Obs cell says
// HOW MANY are running; this is the one surface that says WHICH.
// false: the modal-refresh path, not `exec ls` — no text dump.
func (a *App) onExecs(msg tea.KeyMsg) (tea.Cmd, bool) {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("execs: not connected"))
		return nil, true
	}
	a.execsModal.SetSize(a.width, a.height)
	a.execsModal.Open()
	return DoExecRunList(a.client, "", false), true
}

// `g` opens the live session viewer grid: a full-screen overlay
// tiling read-only PaneStreamers for the live interactive sessions,
// replacing the task-list view (task-list model state is preserved
// and restored on Esc/q — this is a full-screen takeover, not a
// split). Reuses the long-lived client (no fresh dial) and never
// sends a PTY size (the grid has no size authority).
func (a *App) onGrid(msg tea.KeyMsg) (tea.Cmd, bool) {
	return a.openGrid(cli.GridAll, "", nil), true
}

// `z` / `Z` open the same grid narrowed to the SELECTED task's subtree —
// itself plus every task it spawned (z), or only what it spawned (Z),
// for when that one session is already on screen in another terminal
// and its workers are what is missing. Both narrow through cli.GridSet,
// the same call behind the `grid` verb and the WebUI's button, so no
// two surfaces can disagree about who is whose child.
func (a *App) onGridSubtree(msg tea.KeyMsg) (tea.Cmd, bool) {
	mode := cli.GridSubtree
	if msg.String() == mainKeys.GridDescendants {
		mode = cli.GridDescendants
	}
	anchor := a.tasks.SelectedID()
	if anchor == "" {
		a.cmdresult.Append(WarnStyle.Render("grid: no task selected"))
		return nil, true
	}
	return a.openGrid(mode, anchor, nil), true
}

// `O` (capital) opens the agentboard topics view. Fetches the topic
// list on open via DoBoardTopics (long-lived client, no new dial).
// Enter drills into a topic; Esc closes or returns to the topic list.
func (a *App) onBoard(msg tea.KeyMsg) (tea.Cmd, bool) {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("board: not connected"))
		return nil, true
	}
	a.boardModal.Open()
	a.boardModal.SetSize(a.width, a.height)
	return DoBoardTopics(a.client), true
}

// `T` flips the task table between flat order and creator-tree order.
// Purely local: the rows are already in hand, so it re-renders from the
// last snapshot instead of waiting for the next poll — a toggle that
// visibly does nothing for five seconds reads as broken.
func (a *App) onTree(msg tea.KeyMsg) (tea.Cmd, bool) {
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
	return nil, true
}

// `i` opens a new interactive PTY session in the default repo. The
// dance is two-stage: the Cmd dispatches the RPC, the response arrives
// as InteractiveReadyMsg, and Update then hands the terminal to the
// PTY (suspend.go) after gating on termReleased. The session is
// detachable (like `S`); `i` differs only in skipping the ambiguous-
// runner picker. Reattach lives on `r` (onResume).
func (a *App) onInteractive(msg tea.KeyMsg) (tea.Cmd, bool) {
	return DoOpenInteractive(a.client, a.defaultRepo, a.authority()), true
}

// `S` (capital) opens a new detachable interactive PTY session in the
// default repo (equivalent to `harness-cli session new`).
func (a *App) onSession(msg tea.KeyMsg) (tea.Cmd, bool) {
	a.pendingInteractive = pendingInteractive{
		repo: a.defaultRepo, resumeTaskID: "", extraArgs: nil,
		auth: a.authority(), capsOverride: false,
	}
	a.pickerArmed = true
	return DoOpenDetachableSession(a.client, a.defaultRepo, cli.SelectorOpts{}, nil, "", a.authority(), false, false, ""), true
}

// `G` opens the read-only git view for the selected task: its commit
// list, its uncommitted diff, and a baseline the operator can move.
// Unlike the file picker this does NOT require a live worktree — a
// finished task still answers through its retained harness/<id>
// branch (server/git_query.go). Pressed outside the tasks pane, the
// row's OutsideHint says so.
func (a *App) onGit(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("git: no task selected"))
		return nil, true
	}
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("git: not connected"))
		return nil, true
	}
	a.gitModal.Open(taskID)
	a.gitModal.SetSize(a.width, a.height)
	a.gitStatusToContent = false
	return a.gitReload(taskID, nil), true
}

// `F` opens the file picker for the selected task. No-op when no task is
// selected or its worktree is gone (the cmdresult line explains); pressed
// outside the tasks pane, the row's OutsideHint says so.
func (a *App) onFilePicker(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("file picker: no task selected"))
		return nil, true
	}
	// Only a Running or Detached task has a worktree the runner can
	// reach; the server answers NoSuchTask for anything else
	// (server/file_transfer.go). Say so here rather than opening a
	// picker whose first listing fails.
	if t := a.tasks.SelectedTask(); t != nil && !taskSessionAlive(t.Status) {
		a.cmdresult.Append(WarnStyle.Render(
			"file picker: task is " + taskStatusStr(t.Status) + " — only Running or Detached tasks have a worktree"))
		return nil, true
	}
	cmd := a.filepicker.OpenFor(a.client, taskID)
	a.filepicker.SetSize(a.width, a.height)
	return cmd, true
}

// `w` arms an await-idle watcher on the selected task: the fire lands
// as a result line in cmdresult when the session's output goes idle.
// `W` routes the fire through the operator-notification egress
// instead (notify feed + --notify-hook, e.g. the phone) — for when
// you're about to walk away.
func (a *App) onAwaitIdle(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("await-idle: no task selected"))
		return nil, true
	}
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("await-idle: not connected"))
		return nil, true
	}
	sink := protocol.AwaitIdleSink_Reply
	if msg.String() == mainKeys.AwaitIdleNotify {
		sink = protocol.AwaitIdleSink_Notify
	} else {
		a.cmdresult.Append(fmt.Sprintf("await-idle %s: watching (result lands here when the session goes idle)…", shortTaskID(taskID)))
	}
	return DoAwaitIdle(a.appCtx, a.client, taskID, 0, sink, ""), true
}

// `d` opens the detail popup for the focused row (runners or tasks).
func (a *App) onDetail(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch a.focus {
	case focusRunners:
		if r := a.runners.SelectedRunner(); r != nil {
			a.detail.Open("Runner detail", formatRunnerDetail(*r))
		} else {
			a.cmdresult.Append(WarnStyle.Render("no runner selected"))
		}
		return nil, true
	case focusTasks:
		if t := a.tasks.SelectedTask(); t != nil {
			a.detail.Open("Task detail", formatTaskDetail(*t))
		} else {
			a.cmdresult.Append(WarnStyle.Render("no task selected"))
		}
		return nil, true
	}
	return nil, false
}

// `c` cancels the selected task when tasks panel is focused.
func (a *App) onCancel(msg tea.KeyMsg) (tea.Cmd, bool) {
	id := a.tasks.SelectedID()
	if id == "" {
		a.cmdresult.Append(WarnStyle.Render("no task selected"))
		return nil, true
	}
	return DoCancel(a.client, id, id), true
}

// `a` re-grants the selected task's authority: it opens the authority
// picker prefilled from the task's stored caps/scope. Opening needs
// no client; applying goes through the picker's Enter handler.
// Unconditionally available — a TUI connection is an operator
// connection by construction (spec §7). The typed
// `caps set <id> --caps/--scope` cmdline form remains for scripting.
func (a *App) onReGrant(msg tea.KeyMsg) (tea.Cmd, bool) {
	t := a.tasks.SelectedTask()
	if t == nil {
		a.cmdresult.Append(WarnStyle.Render("no task selected"))
		return nil, true
	}
	a.authorityPicker.SetSize(a.width, a.height)
	a.authorityPicker.OpenRegrant(*t, a.tasks.Rows())
	return nil, true
}

// `A` opens the same picker as a parent chooser: re-point the selected
// task's parent link (root / swap / another task). Operator-only
// server-side, like re-grant; applying goes through the picker's
// Enter handler.
func (a *App) onSetParent(msg tea.KeyMsg) (tea.Cmd, bool) {
	t := a.tasks.SelectedTask()
	if t == nil {
		a.cmdresult.Append(WarnStyle.Render("no task selected"))
		return nil, true
	}
	a.authorityPicker.SetSize(a.width, a.height)
	a.authorityPicker.OpenParent(*t, a.tasks.Rows())
	return nil, true
}

// `r` / `R` re-enter the selected session: reattach a live Detached
// session, or resume a finished task into a new detachable session.
// r resumes with --continue (keep claude's memory); R resumes fresh.
// `u` / `U` are the same resume variants but intentionally skip the
// assigned-runner preference so ambiguous runner selection can be
// reopened even when the previous runner is still available.
func (a *App) onResume(msg tea.KeyMsg) (tea.Cmd, bool) {
	t := a.tasks.SelectedTask()
	unpinnedResume := msg.String() == mainKeys.ResumeAnyContinue || msg.String() == mainKeys.ResumeAnyFresh
	act := resumeReattachAction(t, msg.String() == mainKeys.ResumeAssignedContinue || msg.String() == mainKeys.ResumeAnyContinue)
	switch act.Kind {
	case actionReattach:
		if unpinnedResume {
			a.cmdresult.Append(WarnStyle.Render("u/U: pick a finished task to resume without assigned runner"))
			return nil, true
		}
		return DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_Control), true
	case actionChat:
		// The event-stream kind's take-over-equivalent. Pinning is
		// meaningless here: the task is already RUNNING on a runner and
		// this attaches to it, so u/U (whose whole point is to leave the
		// runner unpinned for a fresh spawn) has nothing to offer.
		if unpinnedResume {
			a.cmdresult.Append(WarnStyle.Render("u/U: pick a finished task to resume without assigned runner"))
			return nil, true
		}
		a.chat.Open(a.appCtx, a.client, a.program, a.tasks.SelectedID())
		a.chat.SetSize(a.width, a.height)
		return chatTickCmd(), true
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
			return DoOpenDetachableSession(a.client, "", cli.SelectorOpts{}, nil, a.tasks.SelectedID(), a.authority(), false, act.ResumeConversation, ""), true
		}
		// Pinned (r/R): default to the task's own recorded profile
		// (§4b) so the pinned path stays one keypress — no picker.
		return DoResumeSession(a.client, t.AssignedTo, nil, a.tasks.SelectedID(), a.authority(), false, act.ResumeConversation, string(t.AgentProfile)), true
	case actionNone:
		a.cmdresult.Append(WarnStyle.Render(act.Hint))
		return nil, true
	}
	return nil, false
}

// `v` view-attaches the selected live session in read-only mode (no input sent).
func (a *App) onViewOnly(msg tea.KeyMsg) (tea.Cmd, bool) {
	act := resumeReattachAction(a.tasks.SelectedTask(), true)
	if act.Kind == actionReattach {
		return DoAttachSession(a.client, a.tasks.SelectedID(), protocol.AttachMode_View), true
	}
	return nil, false
}

// onForward is the Do of the two forward rows, which pair start and stop by
// DIRECTION (p/P local, b/B remote) because that is how the help reads; the
// handlers below split by what the key does.
func (a *App) onForward(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case mainKeys.ForwardLocal, mainKeys.ForwardRemote:
		return a.onForwardStart(msg)
	}
	return a.onForwardStop(msg)
}

// `p` / `b` open the local / remote port-forward modal for the selected task.
func (a *App) onForwardStart(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
		return nil, true
	}
	dir := ForwardLocal
	if msg.String() == mainKeys.ForwardRemote {
		dir = ForwardRemote
	}
	a.portForwardModal.OpenMode(taskID, dir)
	return nil, true
}

// `P` / `B` stop a local / remote forward for the selected task. With more
// than one active, a digit picker is shown; with exactly one, kill now.
// Both route through DoKillForward (killLocalForward) — the same RPC
// the forwards modal's `x` (then y/n) and `forward kill` use, so there is
// exactly one way to stop a forward.
func (a *App) onForwardStop(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("forward: no task selected"))
		return nil, true
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
		return a.killLocalForward(sel[0]), true
	default:
		a.forwardPicker.Open(dir, sel)
	}
	return nil, true
}

// `t` opens the raw-connect modal for the selected task: a third way to
// start a forward, alongside `p` (-L) and `b` (-R) above, for a client
// endpoint that lives inside this TUI process rather than a socket. It
// belongs beside the start keys, not behind `f` (the registry listing,
// whose per-row action is kill) — see RawConnectModal's doc comment and
// the rawModal field comment for why P/B don't apply to it.
func (a *App) onRawConnect(msg tea.KeyMsg) (tea.Cmd, bool) {
	taskID := a.tasks.SelectedID()
	if taskID == "" {
		a.cmdresult.Append(WarnStyle.Render("raw connect: no task selected"))
		return nil, true
	}
	a.rawModal.Show(taskID)
	a.rawModal.SetSize(a.width, a.height)
	return nil, true
}
