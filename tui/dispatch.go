package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// tuiVerbs implements verb.TUIDispatch: one method per verb the declaration
// gives this surface.
//
// The assertion below is the point of the whole shape. Go has no exhaustive
// switch, so "the TUI answers every declared verb" used to be a TEST -- and a
// test only sees what it was told to look at, which is how `session attach
// <id> --view` came to answer "too many arguments" on the one surface where
// --view is declared, and how twelve declared paths sat behind hand-written
// token walks nothing checked. A type that misses a method does not build.
//
// What it does NOT unify is the bodies: D3 keeps execute per-surface, and
// these return a tea.Cmd because that is what this surface answers with.
type tuiVerbs struct{ a *App }

var _ verb.TUIDispatch[tea.Cmd] = tuiVerbs{}

func (h tuiVerbs) Prune(v verb.PruneAction) tea.Cmd {
	a := h.a
	if len(v.TaskIDs) > 0 {
		a.cmdresult.Append(fmt.Sprintf("prune: asking server to forget %d task id(s) (force=%t)", len(v.TaskIDs), v.Force))
	} else {
		a.cmdresult.Append(fmt.Sprintf("prune: cutoff = %s; asking server to forget terminal tasks", cli.FormatPruneCutoff(v.Before)))
	}
	return DoPruneTasks(a.client, v.Before, v.TaskIDs, v.Force)
}

func (h tuiVerbs) Cancel(v verb.CancelAction) tea.Cmd {
	a := h.a
	// The declaration carries the id as typed; resolving a half-typed one
	// against the live task list is this surface's job, not the grammar's.
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoCancel(a.client, v.TaskID, full)
}

func (h tuiVerbs) Restore(v verb.RestoreAction) tea.Cmd {
	a := h.a
	// Ids as typed, not prefix-resolved: a pruned task is not in the list
	// a prefix would be resolved against, which is the whole reason this
	// verb exists.
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render("restore: not connected to server"))
		return nil
	}
	ids := v.TaskIDs
	if v.List {
		ids = nil // the bare form lists; --list says so out loud
	}
	return DoRestore(a.client, ids)
}

func (h tuiVerbs) CapsSet(v verb.SetCapsAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoSetCaps(a.client, cli.SetCapsOpts{
		TaskID: full, Caps: v.Caps, Scope: v.Scope, Overrides: v.Overrides,
		Cascade: v.Cascade, KeepConns: v.KeepConns,
	})
}

func (h tuiVerbs) CapsSetParent(v verb.SetParentAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	// An empty ParentID IS the detach request, which is why the
	// declaration refuses `--parent ""` at the value: no condition here
	// could tell it from --none, since the presence rule has already
	// decided which of the three was picked.
	parentFull := ""
	if v.ParentID != "" {
		parentFull, errStr = a.resolveTaskIDPrefix(v.ParentID)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render(errStr))
			return nil
		}
	}
	return DoSetParent(a.client, cli.SetParentOpts{
		TaskID: full, ParentID: parentFull, Swap: v.Swap,
	})
}

func (h tuiVerbs) Grid(v verb.GridAction) tea.Cmd {
	a := h.a
	// Prefixes are resolved HERE, before the set is built: cli.GridSet
	// compares full ids, and a half-typed one must be reported as
	// ambiguous rather than silently matching nothing.
	anchor := ""
	if v.Anchor != "" {
		full, errStr := a.resolveTaskIDPrefix(v.Anchor)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("grid: " + errStr))
			return nil
		}
		anchor = full
	}
	var ids []string
	for _, p := range v.IDs {
		full, errStr := a.resolveTaskIDPrefix(p)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("grid: " + errStr))
			return nil
		}
		ids = append(ids, full)
	}
	return a.openGrid(v.Mode, anchor, ids)
}

func (h tuiVerbs) Notify(v verb.NotifyAction) tea.Cmd {
	a := h.a
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render("notify: not connected to server"))
		return nil
	}
	return DoNotify(a.client, v.Level, v.Title, v.Text)
}

func (h tuiVerbs) ServerDialRunner(v verb.ServerDialRunnerAction) tea.Cmd {
	a := h.a
	if a.client == nil {
		a.cmdresult.Append(ErrorStyle.Render("server dial-runner: not connected to server"))
		return nil
	}
	return DoServerDialRunner(a.client, v.RunnerCID, v.Via)
}

func (h tuiVerbs) ForwardLs(v verb.ForwardLsAction) tea.Cmd {
	a := h.a
	// true: this IS `forward ls` — the text dump is the whole point.
	// TaskFilter was declared for this surface and dropped here, so
	// `forward ls --task <id>` listed every forward.
	filter := ""
	if v.TaskFilter != "" {
		full, errStr := a.resolveTaskIDPrefix(v.TaskFilter)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("forward ls: " + errStr))
			return nil
		}
		filter = full
	}
	return DoListForwardsFiltered(a.client, true, filter)
}

func (h tuiVerbs) ForwardKill(v verb.ForwardKillAction) tea.Cmd {
	a := h.a
	// EVERY id, as on the CLI. No task/spec context: the operator supplied
	// bare ids. This killed ForwardIDs[0] and said nothing about the rest,
	// so one declared verb meant two things depending on where it was
	// typed.
	var cmds []tea.Cmd
	for _, id := range v.ForwardIDs {
		cmds = append(cmds, DoKillForward(a.client, id, "", ""))
	}
	return tea.Batch(cmds...)
}

func (h tuiVerbs) ForwardTap(v verb.ForwardTapAction) tea.Cmd {
	a := h.a
	return a.startForwardTap(v)
}

func (h tuiVerbs) FileLs(v verb.FileLsAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoFileLs(a.client, full, v.RelPath)
}

func (h tuiVerbs) FilePush(v verb.FilePushAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoFilePush(a.client, full, v.LocalSrc, v.RemoteDst, v.Recursive, v.Force, v.Parents)
}

func (h tuiVerbs) FilePull(v verb.FilePullAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoFilePull(a.client, full, v.RemoteSrc, v.LocalDst, v.Recursive, v.Force,
		cli.FileTransferRange{Offset: v.Offset, Length: v.Length})
}

func (h tuiVerbs) FileMkdir(v verb.FileMkdirAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoFileMkdir(a.client, full, v.RelPath, v.Parents)
}

func (h tuiVerbs) FileDelete(v verb.FileDeleteAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	return DoFileDelete(a.client, full, v.RelPath, v.Recursive, v.Force)
}

func (h tuiVerbs) FileEdit(v verb.FileEditAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	rel := v.RelPath
	return func() tea.Msg { return FileEditRequestMsg{TaskID: full, Rel: rel} }
}

func (h tuiVerbs) FileNew(v verb.FileNewAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return nil
	}
	// OpenNew seeds a directory; this caller knows the whole path.
	a.fileEditor.SetSize(a.width, a.height)
	a.fileEditor.OpenNew(full, "")
	a.fileEditor.SetName(v.RelPath)
	return nil
}

// --- git ---------------------------------------------------------------
//
// One case with an inner switch became six methods. The declaration already
// fixes the sub-verb (Const "Sub"), so re-deciding it here was a second copy
// of a choice the table had made -- and the copy is where `--max` went
// unread, because the shared prologue built the query from the modal's state.

// gitOpen is everything the six share: resolve the id, open the modal at the
// command line's root, and issue the three panel queries with the limits the
// line asked for.
func (h tuiVerbs) gitOpen(v verb.GitAction) (full string, limits func(cli.GitQuery) cli.GitQuery, cmds []tea.Cmd, ok bool) {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render(errStr))
		return "", nil, nil, false
	}
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("git: not connected"))
		return "", nil, nil, false
	}
	// The cmdline route lands in the same modal the G key opens, rather than
	// dumping a diff into the result pane where it cannot be scrolled.
	a.gitModal.Open(full)
	a.gitModal.SetSize(a.width, a.height)
	a.gitStatusToContent = v.Sub == "status"
	// Root and submodule come from the command line here, and everything
	// below reads them back off the modal — so the keyboard route and the
	// cmdline route cannot drift apart.
	a.gitModal.SetRoot(v.Subrepo, v.Submodule)
	if v.BaseRev != "" && v.Sub == "diff" {
		a.gitModal.SetBaseRev(v.BaseRev)
	}
	limits = func(q cli.GitQuery) cli.GitQuery {
		if v.Max > 0 {
			q.MaxCommits = uint32(v.Max)
		}
		if v.MaxBytes > 0 {
			q.MaxBytes = uint32(v.MaxBytes)
		}
		return q
	}
	return full, limits, []tea.Cmd{a.gitReload(full, limits)}, true
}

// GitLog, GitStatus and GitSubrepos are answered by gitReload's three queries.
func (h tuiVerbs) GitLog(v verb.GitAction) tea.Cmd      { return h.gitPanelOnly(v) }
func (h tuiVerbs) GitStatus(v verb.GitAction) tea.Cmd   { return h.gitPanelOnly(v) }
func (h tuiVerbs) GitSubrepos(v verb.GitAction) tea.Cmd { return h.gitPanelOnly(v) }

func (h tuiVerbs) gitPanelOnly(v verb.GitAction) tea.Cmd {
	_, _, cmds, ok := h.gitOpen(v)
	if !ok {
		return nil
	}
	return tea.Batch(cmds...)
}

func (h tuiVerbs) GitFile(v verb.GitAction) tea.Cmd {
	full, limits, cmds, ok := h.gitOpen(v)
	if !ok {
		return nil
	}
	q := limits(h.a.gitModal.Query())
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
	return tea.Batch(append(cmds, DoGitFile(h.a.client, full, q))...)
}

func (h tuiVerbs) GitShow(v verb.GitAction) tea.Cmd {
	full, limits, cmds, ok := h.gitOpen(v)
	if !ok {
		return nil
	}
	q := limits(h.a.gitModal.Query())
	q.BaseRev = v.BaseRev
	q.Path = v.Path
	return tea.Batch(append(cmds, DoGitShow(h.a.client, full, q))...)
}

func (h tuiVerbs) GitDiff(v verb.GitAction) tea.Cmd {
	full, limits, cmds, ok := h.gitOpen(v)
	if !ok {
		return nil
	}
	q := limits(h.a.gitModal.Query())
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
	return tea.Batch(append(cmds, DoGitDiff(h.a.client, full, q))...)
}

// --- exec --------------------------------------------------------------

func (h tuiVerbs) Exec(v verb.ExecRunAction) tea.Cmd {
	a := h.a
	full, errStr := a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		a.cmdresult.Append(ErrorStyle.Render("exec: " + errStr))
		return nil
	}
	a.cmdresult.Append(fmt.Sprintf("exec %s: %s …", pfShortID(full), execArgvLabel(v.Argv)))
	return DoExecRun(a.client, full, v.Argv, ExecRunFlags{Shell: v.Shell, SshdParent: v.SshdParent}, a.program)
}

func (h tuiVerbs) ExecLs(v verb.ExecRunAction) tea.Cmd {
	a := h.a
	// TaskFilter, not TaskID: `exec ls --task <id>` NARROWS a listing, where
	// every other exec verb's id names the task to act on.
	filter := ""
	if v.TaskFilter != "" {
		full, errStr := a.resolveTaskIDPrefix(v.TaskFilter)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("exec ls: " + errStr))
			return nil
		}
		filter = full
	}
	// true: this is the cmdline verb, so its text belongs in the result pane.
	return DoExecRunList(a.client, filter, true)
}

func (h tuiVerbs) ExecKill(v verb.ExecRunAction) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range v.ExecIDs {
		cmds = append(cmds, DoExecRunKill(h.a.client, id))
	}
	return tea.Batch(cmds...)
}

// --- spawn -------------------------------------------------------------
//
// Three verbs, one action, and the Kind the declaration fixes. The prologue
// (repo default, caps resolution, the authority half) is what they share; the
// call they end in is what they do not.

func (h tuiVerbs) spawnPrologue(v verb.SpawnAction) (repo string, auth Authority, capsOverride bool) {
	a := h.a
	repo = v.Repo
	if repo == "" {
		repo = a.defaultRepo
	}
	caps, capsOverride := a.resolveSpawnCaps(v.Caps, v.ResumeTaskID != "")
	return repo, a.spawnAuthority(v.Scope, v.Overrides, v.ResumeTaskID, caps), capsOverride
}

func (h tuiVerbs) Submit(v verb.SpawnAction) tea.Cmd {
	repo, auth, capsOverride := h.spawnPrologue(v)
	return DoSubmitWithOpts(h.a.client, repo, v.Task, "", v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
}

func (h tuiVerbs) Interactive(v verb.SpawnAction) tea.Cmd {
	repo, auth, capsOverride := h.spawnPrologue(v)
	return DoOpenInteractiveWithOpts(h.a.client, repo, "", v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
}

func (h tuiVerbs) SessionNew(v verb.SpawnAction) tea.Cmd {
	a := h.a
	repo, auth, capsOverride := h.spawnPrologue(v)
	sel := cli.SelectorOpts{Host: v.Host, Runner: v.Runner, IP: v.IP}
	if v.X11 {
		return DoOpenX11Session(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, int(v.X11Display), a.program, auth, capsOverride, v.ResumeConversation, v.Agent)
	}
	if v.Detach {
		return DoStartDetachedSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent, TermSize{Rows: uint16(a.height), Cols: uint16(a.width)}, v.Stream)
	}
	// Stream without Detach cannot reach here: the declaration refuses it.
	return DoOpenDetachableSession(a.client, repo, sel, v.ExtraArgs, v.ResumeTaskID, auth, capsOverride, v.ResumeConversation, v.Agent)
}

// --- session -----------------------------------------------------------
//
// Eleven verbs shared one action and one inner switch. `session attach <id>
// --view` answered "too many arguments" on the one surface where --view is
// declared, because five of these were TUI-local action types with five
// hand-written parsers before the migration.

// sessionTask resolves the id every one of these needs but `ls`.
func (h tuiVerbs) sessionTask(v verb.SessionAction) (string, bool) {
	full, errStr := h.a.resolveTaskIDPrefix(v.TaskID)
	if errStr != "" {
		h.a.cmdresult.Append(ErrorStyle.Render(errStr))
		return "", false
	}
	return full, true
}

func (h tuiVerbs) SessionLs(v verb.SessionAction) tea.Cmd { return DoSessionList(h.a.client) }

func (h tuiVerbs) SessionAttach(v verb.SessionAction) tea.Cmd {
	full, ok := h.sessionTask(v)
	if !ok {
		return nil
	}
	mode := protocol.AttachMode_Control
	if v.View {
		mode = protocol.AttachMode_View
	}
	return DoAttachSession(h.a.client, full, mode)
}

func (h tuiVerbs) SessionKill(v verb.SessionAction) tea.Cmd {
	full, ok := h.sessionTask(v)
	if !ok {
		return nil
	}
	return DoCancel(h.a.client, v.TaskID, full)
}

func (h tuiVerbs) SessionStreamAttach(v verb.SessionAction) tea.Cmd {
	a := h.a
	full, ok := h.sessionTask(v)
	if !ok {
		return nil
	}
	// Following an event-stream task in the TUI IS following its log: the
	// runner renders this kind's events into the task log, so the logs pane is
	// the follower — the same content the CLI's `session stream attach` shows.
	a.cmdresult.Append(fmt.Sprintf("stream attach %s: following its events in the logs pane", shortTaskID(full)))
	a.setFocus(focusLogs)
	return a.followTask(full)
}

func (h tuiVerbs) SessionStreamTurn(v verb.SessionAction) tea.Cmd      { return h.streamWrite(v) }
func (h tuiVerbs) SessionStreamApprove(v verb.SessionAction) tea.Cmd   { return h.streamWrite(v) }
func (h tuiVerbs) SessionStreamInterrupt(v verb.SessionAction) tea.Cmd { return h.streamWrite(v) }
func (h tuiVerbs) SessionStreamFinish(v verb.SessionAction) tea.Cmd    { return h.streamWrite(v) }

func (h tuiVerbs) streamWrite(v verb.SessionAction) tea.Cmd {
	full, ok := h.sessionTask(v)
	if !ok {
		return nil
	}
	return DoStreamWrite(h.a.client, v, full)
}

func (h tuiVerbs) SessionAwaitIdle(v verb.SessionAction) tea.Cmd {
	a := h.a
	full, ok := h.sessionTask(v)
	if !ok {
		return nil
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
	// appCtx, not a round-trip timeout: the reply sink long-polls until the
	// session actually goes idle. The cmd goroutine carries it; the UI stays
	// interactive meanwhile.
	return DoAwaitIdle(a.appCtx, a.client, full, uint32(v.ThresholdMs), sink, v.Topic)
}

// --- ssh-gateway and workspace -----------------------------------------
//
// Both still route through one helper each, because their inner switches are
// the BODIES rather than a choice the declaration already made -- each branch
// is a different operation on this process's own state. What the split buys
// here is that the method exists at all, so a new sub-verb in the table
// cannot be dispatched to nothing.

func (h tuiVerbs) SshGatewayStart(v verb.SSHGatewayAction) tea.Cmd { return h.a.runSSHGatewayAction(v) }
func (h tuiVerbs) SshGatewayStop(v verb.SSHGatewayAction) tea.Cmd  { return h.a.runSSHGatewayAction(v) }
func (h tuiVerbs) SshGatewayStatus(v verb.SSHGatewayAction) tea.Cmd {
	return h.a.runSSHGatewayAction(v)
}

func (h tuiVerbs) WorkspaceSave(v verb.WorkspaceAction) tea.Cmd   { return h.a.runWorkspaceAction(v) }
func (h tuiVerbs) WorkspaceRm(v verb.WorkspaceAction) tea.Cmd     { return h.a.runWorkspaceAction(v) }
func (h tuiVerbs) WorkspaceLs(v verb.WorkspaceAction) tea.Cmd     { return h.a.runWorkspaceAction(v) }
func (h tuiVerbs) WorkspaceShow(v verb.WorkspaceAction) tea.Cmd   { return h.a.runWorkspaceAction(v) }
func (h tuiVerbs) WorkspaceApply(v verb.WorkspaceAction) tea.Cmd  { return h.a.runWorkspaceAction(v) }
func (h tuiVerbs) WorkspaceDetach(v verb.WorkspaceAction) tea.Cmd { return h.a.runWorkspaceAction(v) }

// --- the screen-state family -------------------------------------------
//
// clear / quit / help / refresh / trsf / diag / repo. They were the TUI's
// hand-written switch, kept out of the table on the grounds that only this
// surface has them -- but "only this surface" is what Surfaces says, and
// every verb here already has a surface-local BODY. What they got by staying
// out was a second parse, a second name list in the help test, and no way for
// anything else to know the words exist.
//
// One ScreenAction carries all of them, keyed by a declared Const, so the
// generated dispatcher routes on a value the table fixes rather than on a
// type each file spells for itself.

func (h tuiVerbs) Clear(v verb.ScreenAction) tea.Cmd {
	a := h.a
	a.cmdresult.Clear()
	return nil
}

// Quit also answers `exit`: two declared paths, one Const, one method.
func (h tuiVerbs) Quit(v verb.ScreenAction) tea.Cmd {
	a := h.a
	return a.quit()
}

func (h tuiVerbs) Help(v verb.ScreenAction) tea.Cmd {
	a := h.a
	for _, l := range cmdlineHelpLines() {
		a.cmdresult.Append(l)
	}
	return nil
}

// Refresh also answers `sync`.
func (h tuiVerbs) Refresh(v verb.ScreenAction) tea.Cmd {
	a := h.a
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("refresh: not connected"))
		return nil
	}
	a.cmdresult.Append("refreshing snapshot…")
	return RefreshSnapshot(a.client)
}

func (h tuiVerbs) Trsf(v verb.ScreenAction) tea.Cmd {
	a := h.a
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("trsf: not connected"))
		return nil
	}
	st := a.client.Transport().GetInternalState()
	if st == nil {
		a.cmdresult.Append(WarnStyle.Render("trsf: no internal state"))
		return nil
	}
	a.cmdresult.Append(OKStyle.Render("trsf internal state (client↔server):"))
	a.cmdresult.Append(fmt.Sprintf("  streams: send=%d recv=%d   mtu=%d", st.ActiveSendStreams, st.ActiveReceiveStreams, st.CurrentMTU))
	a.cmdresult.Append(fmt.Sprintf("  queues: send=%d recv=%d   triggers: sendAction=%d updateWin=%d cancel=%d",
		st.SendQueueLength, st.ReceiveQueueLength, st.SendActionCount, st.UpdateWindowCount, st.CancelStreamCount))
	a.cmdresult.Append(fmt.Sprintf("  cc: inflight=%dB cwnd=%dB rtt=%v (var %v) sentPkts=%d",
		st.BytesInFlight, st.CongestionWindow, st.SmoothedRTT, st.RTTVariance, len(st.SentPackets)))
	// spurious counts packets given up on and then acked: those cuts to
	// the window were taken on nothing.
	a.cmdresult.Append(fmt.Sprintf("  loss: events=%d packets=%d spurious=%d",
		st.Loss.Events, st.Loss.Packets, st.Loss.Spurious))
	// Only meaningful as a delta between two dumps: frozen = the run loop
	// is blocked (nothing is demuxed, so no stream ever becomes visible),
	// exploding = busy-spin, advancing slowly = congestion-blocked.
	a.cmdresult.Append(fmt.Sprintf("  loop: iterations=%d (run `trsf` twice — the delta is the signal)", st.LoopIterations))
	return nil
}

func (h tuiVerbs) Diag(v verb.ScreenAction) tea.Cmd {
	a := h.a
	// Bare `diag` toggles; `diag on` / `diag off` assert. The declaration
	// restricts the word to on|off (Arg.OneOfArg), so a third one is refused
	// at the parse instead of silently toggling.
	want := !GridDiagEnabled()
	if v.Arg != "" {
		want = v.Arg == "on"
	}
	// Report the state that was actually SET, not the one requested: the two
	// can only differ if this ever stops being a plain assignment, and a
	// result line that echoes the request cannot say so.
	if SetGridDiag(want) {
		a.cmdresult.Append(OKStyle.Render("grid diag: on — panes show rx/rate/size on their first row"))
	} else {
		a.cmdresult.Append(OKStyle.Render("grid diag: off"))
	}
	return nil
}

func (h tuiVerbs) Repo(v verb.ScreenAction) tea.Cmd {
	a := h.a
	// The repo string is treated as an opaque identifier — server
	// matches it byte-for-byte against runner-registered AllowedRoots.
	// We cannot filepath.Abs() here because the TUI host and runner
	// host may have different OSes (e.g. Windows TUI + Linux runner),
	// where local Abs would mangle a valid runner path into a
	// meaningless drive-prefixed one.
	path := v.Arg
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
	return nil
}
