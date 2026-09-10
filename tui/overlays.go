package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// overlay is a surface that owns every key while it is open. updateKey hands
// the key to the FIRST open entry of appOverlays and nothing below it sees it,
// so the order of that table is the stacking order: an overlay drawn on top of
// another must come before it.
type overlay struct {
	open func(*App) bool
	key  func(*App, tea.KeyMsg) tea.Cmd
}

var appOverlays = []overlay{
	{func(a *App) bool { return a.detail.IsOpen() }, (*App).inDetail},
	{func(a *App) bool { return a.connsModal.IsOpen() }, (*App).inConnsModal},
	{func(a *App) bool { return a.execsModal.IsOpen() }, (*App).inExecsModal},
	// Opened from the forwards pane and drawn over it, so it comes first.
	{func(a *App) bool { return a.forwardTap.IsOpen() }, (*App).inForwardTap},
	{func(a *App) bool { return a.forwardsModal.IsOpen() }, (*App).inForwardsModal},
	{func(a *App) bool { return a.grid.IsOpen() }, (*App).inGrid},
	{func(a *App) bool { return a.chat.IsOpen() }, (*App).inChat},
	{func(a *App) bool { return a.boardModal.IsOpen() }, (*App).inBoardModal},
	{func(a *App) bool { return a.gitModal.IsOpen() }, (*App).inGitModal},
	// Opened from the file picker and drawn over it, so it comes first.
	{func(a *App) bool { return a.fileEditor.IsOpen() }, (*App).inFileEditor},
	{func(a *App) bool { return a.filepicker.IsOpen() }, (*App).inFilePicker},
	{func(a *App) bool { return a.popup.IsOpen() }, (*App).inSubmitPopup},
	{func(a *App) bool { return a.workspacePicker.IsOpen() }, (*App).inWorkspacePicker},
	{func(a *App) bool { return a.authorityPicker.IsOpen() }, (*App).inAuthorityPicker},
	{func(a *App) bool { return a.runnerPicker.IsOpen() }, (*App).inRunnerPicker},
	{func(a *App) bool { return a.forwardPicker.IsOpen() }, (*App).inForwardPicker},
	{func(a *App) bool { return a.portForwardModal.IsOpen() }, (*App).inPortForwardModal},
	{func(a *App) bool { return a.rawModal.IsOpen() }, (*App).inRawModal},
}

// Detail popup is read-only — Esc closes, all other keys swallowed
// so cursor movement / 'q' / etc. don't leak through.
func (a *App) inDetail(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.detail.Close()
		return nil
	}
	// Everything else scrolls: the body can be taller than the screen
	// (`?` is), and a popup you cannot scroll hides its own top.
	var cmd tea.Cmd
	a.detail, cmd = a.detail.Update(msg)
	return cmd
}

// Connections modal: Esc closes; `t` switches between the two questions about
// the same rows — which connections exist, and what each one's transport is
// doing. In the reading, enter aims it at the selected runner and `s` back at
// the server. Arrow keys scroll the table; all other keys (q, etc.) are
// swallowed so they don't leak through.
func (a *App) inConnsModal(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.connsModal.Close()
		return nil
	}
	switch msg.String() {
	case modalKeys.ConnsTrsf:
		if !a.connsModal.ToggleTrsf() {
			// Back to the identity rows. The poll stops with the mode — the
			// tick handler drops any that is still in flight.
			return nil
		}
		return a.startTrsfPoll()
	case modalKeys.ConnsReadRunner:
		// Only in the reading, and only on a runner row: any other role has no
		// separate transport to ask about. Elsewhere it falls through to the
		// table rather than looking like it did something.
		if a.connsModal.IsTrsf() && a.connsModal.TargetSelectedRunner() {
			return a.readTrsfOnce()
		}
	case modalKeys.ConnsReadServer:
		if a.connsModal.IsTrsf() {
			a.connsModal.TargetServer()
			return a.readTrsfOnce()
		}
	}
	var cmd tea.Cmd
	a.connsModal, cmd = a.connsModal.Update(msg)
	return cmd
}

// startTrsfPoll takes the first reading and arms the ticker for the rest,
// under a fresh generation so any chain from an earlier visit to this mode
// stops rescheduling. readTrsfOnce is the first half alone, for a retarget:
// the chain in flight keeps the cadence, so arming a second one would double
// it.
func (a *App) startTrsfPoll() tea.Cmd {
	a.trsfGen++
	return tea.Batch(a.readTrsfOnce(), trsfTick(a.connsModal.TrsfEvery(), a.trsfGen))
}

func (a *App) readTrsfOnce() tea.Cmd {
	if a.client == nil {
		a.connsModal.SetTrsfError(errNotConnected)
		return nil
	}
	return DoTrsfState(a.client, a.connsModal.TrsfTarget())
}

// Running-exec list: Esc closes; `x` arms a y/n kill confirmation for the
// selected row, through the same DoExecRunKill the cmdline verb uses. While
// the confirmation is up every other key is swallowed, as in inForwardsModal.
func (a *App) inExecsModal(msg tea.KeyMsg) tea.Cmd {
	if a.execsModal.IsConfirming() {
		switch msg.String() {
		case modalKeys.ConfirmYes, modalKeys.ConfirmYesUpper:
			if id, ok := a.execsModal.ConfirmKill(); ok {
				return DoExecRunKill(a.client, id)
			}
			return nil
		case modalKeys.ConfirmNo, modalKeys.ConfirmNoUpper, modalKeys.Escape:
			a.execsModal.CancelKillConfirm()
			return nil
		}
		// Swallow everything else so the table cannot move under the
		// operator mid-confirm, as inForwardsModal does.
		return nil
	}
	if msg.Type == tea.KeyEsc {
		a.execsModal.Close()
		return nil
	}
	if msg.String() == modalKeys.ForwardKill {
		a.execsModal.BeginKillConfirm()
		return nil
	}
	var cmd tea.Cmd
	a.execsModal, cmd = a.execsModal.Update(msg)
	return cmd
}

// The tap view is opened from the forwards pane and drawn on top of it, so
// appOverlays lists it BEFORE that pane and it owns the keys while it is up.
func (a *App) inForwardTap(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.stopForwardTap()
		// Straight back onto a pane whose counters moved while the tap
		// was up — refetch rather than show the numbers from before.
		if a.forwardsModal.IsOpen() {
			return DoListForwards(a.client, false)
		}
		return nil
	}
	cmd := a.forwardTap.Update(msg)
	return cmd
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
func (a *App) inForwardsModal(msg tea.KeyMsg) tea.Cmd {
	if a.forwardsModal.IsConfirming() {
		switch msg.String() {
		case modalKeys.ConfirmYes, modalKeys.ConfirmYesUpper:
			if id, taskID, spec, ok := a.forwardsModal.ConfirmKill(); ok {
				return DoKillForward(a.client, id, taskID, spec)
			}
			return nil
		case modalKeys.ConfirmNo, modalKeys.ConfirmNoUpper, modalKeys.Escape:
			a.forwardsModal.CancelKillConfirm()
			return nil
		}
		// Swallow every other key while a kill is pending so the
		// table (and its j/k navigation) can't move — and nothing
		// else can be triggered — mid-confirm.
		return nil
	}
	if msg.Type == tea.KeyEsc {
		a.forwardsModal.Close()
		return nil
	}
	if msg.String() == modalKeys.ForwardKill {
		a.forwardsModal.BeginKillConfirm()
		return nil
	}
	if msg.String() == modalKeys.ForwardTap {
		if id, ok := a.forwardsModal.SelectedID(); ok {
			return a.startForwardTap(verb.ForwardTapAction{ForwardID: id, Dir: "both"})
		}
		return nil
	}
	// The pane fetches once on open and forwards have no push
	// subscription, so its rows age in place. That was harmless while a
	// row was pure configuration; now it carries counters, and a stale
	// pane shows 0/0 while bytes are crossing — which reads as "this
	// forward is idle", the one thing the counters exist to answer.
	if msg.String() == modalKeys.ForwardRefresh {
		return DoListForwards(a.client, false)
	}
	var cmd tea.Cmd
	a.forwardsModal, cmd = a.forwardsModal.Update(msg)
	return cmd
}

// Live session viewer grid: full-screen overlay, intercepts ALL
// keys when open (focus movement / Enter / x / Esc-q are handled
// inside GridModel.Update).
func (a *App) inGrid(msg tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	a.grid, cmd = a.grid.Update(msg)
	return cmd
}

// Chat: the event-stream kind's full-screen driving surface. Like the
// grid it intercepts ALL keys — the text input is focused, so anything
// not claimed by ChatModel.Update is a character being typed.
func (a *App) inChat(msg tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	a.chat, cmd = a.chat.Update(msg)
	return cmd
}

// Board modal: two-mode overlay (topics / messages).
// The App dispatches Do* cmds for Enter/r/x/X; the modal handles
// table/viewport navigation itself.
func (a *App) inBoardModal(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		if m := a.boardModal.Mode(); m == boardMessages || m == boardSubscribers {
			a.boardModal.PopToTopics()
			return nil
		}
		a.boardModal.Close()
		return nil
	}
	if a.boardModal.Mode() == boardTopics {
		switch msg.Type {
		case tea.KeyEnter:
			topic := a.boardModal.SelectedTopicName()
			if topic != "" {
				return DoBoardRead(a.client, topic)
			}
			return nil
		}
		switch msg.String() {
		case modalKeys.BoardRefresh:
			return DoBoardTopics(a.client)
		case modalKeys.BoardPurgeTopic:
			topic := a.boardModal.SelectedTopicName()
			if topic != "" {
				return DoBoardPurge(a.client, topic, 0)
			}
			return nil
		case modalKeys.BoardSubscribers:
			topic := a.boardModal.SelectedTopicName()
			if topic != "" {
				return DoBoardSubscribers(a.client, topic)
			}
			return nil
		}
	} else if a.boardModal.Mode() == boardSubscribers {
		if msg.String() == modalKeys.BoardSubscribers {
			return DoBoardSubscribers(a.client, a.boardModal.CurTopic())
		}
	} else {
		// boardMessages mode
		switch msg.String() {
		case modalKeys.BoardPurgeMsg:
			seq := a.boardModal.SelectedMsgSeq()
			if seq != 0 {
				return DoBoardPurge(a.client, a.boardModal.CurTopic(), seq)
			}
			return nil
		case modalKeys.BoardRetractMsg:
			seq := a.boardModal.SelectedMsgSeq()
			if seq != 0 {
				return DoBoardRetract(a.client, a.boardModal.CurTopic(), seq)
			}
			return nil
		case modalKeys.BoardRefresh:
			return DoBoardRead(a.client, a.boardModal.CurTopic())
		}
	}
	var cmd tea.Cmd
	a.boardModal, cmd = a.boardModal.Update(msg)
	return cmd
}

// Git modal: row picker over a diff viewport. The App dispatches the
// keys that need a client (Enter / r / s); the modal owns selection,
// the baseline and scrolling.
func (a *App) inGitModal(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.gitModal.Close()
		return nil
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
			return a.gitReload(taskID, nil)
		}
		kind, target, rev := a.gitModal.GitQueryForRow(row)
		q := a.gitModal.Query()
		q.BaseRev = rev
		if kind == protocol.GitQueryKind_Show {
			q.Kind = protocol.GitQueryKind_Show
			a.gitModal.RecordContentQuery(q)
			return DoGitShow(a.client, taskID, q)
		}
		q.Target = target
		q.Kind = protocol.GitQueryKind_Diff
		a.gitModal.RecordContentQuery(q)
		return DoGitDiff(a.client, taskID, q)
	}
	switch msg.String() {
	case modalKeys.GitOpenFile:
		// Toggle: into the whole file, or back to the diff it came from.
		if a.gitModal.LeaveFileView() {
			q := a.gitModal.LastContentQuery()
			if q.Kind == protocol.GitQueryKind_Show {
				return DoGitShow(a.client, taskID, q)
			}
			return DoGitDiff(a.client, taskID, q)
		}
		fq, ok := a.gitModal.OpenFileQuery()
		if !ok {
			a.gitModal.SetError("no file here — scroll to a diff hunk, or the file was deleted")
			return nil
		}
		return DoGitFile(a.client, taskID, fq)
	case modalKeys.BoardRefresh:
		return a.gitReload(taskID, nil)
	case modalKeys.GitStatus:
		a.gitStatusToContent = true
		return DoGitStatus(a.client, taskID, a.gitModal.Query())
	case modalKeys.GitUp:
		if !a.gitModal.LeaveSubrepo() {
			return nil
		}
		a.gitModal.SetSize(a.width, a.height)
		return a.gitReload(taskID, nil)
	case modalKeys.GitSubmodule:
		a.gitModal.ToggleSubmodule()
		// Re-issue the current row so the toggle is visible at once
		// rather than on the next unrelated keypress.
		row := a.gitModal.SelectedRow()
		if row.Kind == gitRowSubrepo {
			return nil
		}
		kind, target, rev := a.gitModal.GitQueryForRow(row)
		q := a.gitModal.Query()
		q.BaseRev = rev
		if kind == protocol.GitQueryKind_Show {
			q.Kind = protocol.GitQueryKind_Show
			a.gitModal.RecordContentQuery(q)
			return DoGitShow(a.client, taskID, q)
		}
		q.Target = target
		q.Kind = protocol.GitQueryKind_Diff
		a.gitModal.RecordContentQuery(q)
		return DoGitDiff(a.client, taskID, q)
	}
	var cmd tea.Cmd
	a.gitModal, cmd = a.gitModal.Update(msg)
	return cmd
}

// The editor popup sits on top of the file picker (which is what opens it),
// so appOverlays lists it BEFORE the picker, which swallows everything it sees.
func (a *App) inFileEditor(msg tea.KeyMsg) tea.Cmd {
	var ecmd tea.Cmd
	a.fileEditor, ecmd = a.fileEditor.Update(msg)
	return ecmd
}

// File picker intercepts ALL keys when open.
func (a *App) inFilePicker(msg tea.KeyMsg) tea.Cmd {
	var pcmd tea.Cmd
	a.filepicker, pcmd = a.filepicker.Update(msg)
	return pcmd
}

// Submit popup intercepts ALL keys when open.
func (a *App) inSubmitPopup(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		a.popup.Close()
		return nil
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
			return nil
		}
		// repo is irrelevant on resume — server uses the existing
		// task's RepoPath. Only require it for fresh submits.
		if repo == "" && resumeID == "" {
			a.cmdresult.Append(WarnStyle.Render("submit cancelled (no repo — wait for a runner to register, then reopen with `s`)"))
			return nil
		}
		return DoSubmitWithOpts(a.client, repo, prompt, host, extraArgs, resumeID, a.authority(), false, resumeConversation, agent)
	case tea.KeyTab:
		a.popup.CycleRepo(+1)
		return nil
	case tea.KeyShiftTab:
		a.popup.CycleHost(+1)
		return nil
	case tea.KeyCtrlA:
		a.popup.CycleAgent(+1)
		return nil
	case tea.KeyCtrlE:
		a.popup.ToggleFocus()
		return nil
	case tea.KeyCtrlR:
		a.popup.ToggleResumeConversation()
		return nil
	}
	var pcmd tea.Cmd
	a.popup, pcmd = a.popup.Update(msg)
	return pcmd
}

// Workspace-save picker: j/k move, space includes a task, r/u/g/s cycle its
// resume / runner / grid / gateway choices, f edits its forward lines, a/n
// tick all or none, enter writes, esc cancels.
func (a *App) inWorkspacePicker(msg tea.KeyMsg) tea.Cmd {
	// The forward editor owns every key while it is up, or typing "-L
	// 3000:…" would be read as move/include/cycle commands. Only Enter
	// and Esc are claimed back.
	if a.workspacePicker.IsEditing() {
		switch msg.Type {
		case tea.KeyEsc:
			a.workspacePicker.CancelEdit()
			return nil
		case tea.KeyEnter:
			if err := a.workspacePicker.CommitEdit(); err != nil {
				a.cmdresult.Append(ErrorStyle.Render("workspace save: " + err.Error()))
			}
			return nil
		}
		return a.workspacePicker.UpdateInput(msg)
	}
	switch {
	case msg.Type == tea.KeyEsc:
		a.workspacePicker.Close()
		a.cmdresult.Append("workspace save: cancelled")
		return nil
	case msg.Type == tea.KeyUp:
		a.workspacePicker.Move(-1)
		return nil
	case msg.Type == tea.KeyDown:
		a.workspacePicker.Move(1)
		return nil
	case msg.Type == tea.KeySpace:
		a.workspacePicker.Toggle()
		return nil
	case msg.Type == tea.KeyEnter:
		return a.commitWorkspacePicker()
	case msg.Type == tea.KeyRunes:
		// Per rune, not msg.String(): fast key-repeat batches a "jjj"
		// burst into ONE KeyMsg, and comparing the string would make it
		// an unknown key rather than three moves.
		for _, r := range msg.Runes {
			switch r {
			case 'j':
				a.workspacePicker.Move(1)
			case 'k':
				a.workspacePicker.Move(-1)
			case ' ':
				a.workspacePicker.Toggle()
			case 'r':
				a.workspacePicker.CycleResume()
			case 'u':
				a.workspacePicker.CycleRunner()
			case 'g':
				a.workspacePicker.CycleGrid()
			case 's':
				a.workspacePicker.CycleGateway()
			case 'f':
				a.workspacePicker.BeginEdit()
			case 'a':
				a.workspacePicker.SetAll(true)
			case 'n':
				a.workspacePicker.SetAll(false)
			}
		}
		return nil
	}
	return nil
}

// Authority picker: j/k move, space toggles, v toggles the see-only set,
// A/N all or none, enter applies, esc cancels.
func (a *App) inAuthorityPicker(msg tea.KeyMsg) tea.Cmd {
	switch {
	case msg.Type == tea.KeyEsc:
		a.authorityPicker.Close()
		return nil
	case msg.Type == tea.KeyUp:
		a.authorityPicker.Move(-1)
		return nil
	case msg.Type == tea.KeyDown:
		a.authorityPicker.Move(1)
		return nil
	case msg.Type == tea.KeySpace:
		a.authorityPicker.Toggle()
		return nil
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
			case 'v':
				// The task rows' second checkbox: +vis-ids:, the
				// see-only set. Space stays the action set because
				// that is the common edit; a plain second key keeps
				// both reachable without introducing a mode.
				a.authorityPicker.ToggleVisID()
			case 'A':
				// WebUI chip row's [all] / [none] quick-set, as keys.
				a.authorityPicker.SetAllCaps(true)
			case 'N':
				a.authorityPicker.SetAllCaps(false)
			}
		}
		return nil
	case msg.Type == tea.KeyEnter:
		if a.authorityPicker.Mode() == PickerModeParent {
			parentHex, _, swap, ok := a.authorityPicker.ParentChoice()
			target := a.authorityPicker.TargetID()
			a.authorityPicker.Close()
			if !ok {
				return nil
			}
			if a.client == nil {
				a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
				return nil
			}
			// ParentID == "" without Swap IS the detach form on the wire.
			return DoSetParent(a.client, cli.SetParentOpts{
				TaskID: target, ParentID: parentHex, Swap: swap,
			})
		}
		caps, spec, cascade, keep := a.authorityPicker.Result()
		// Carried, not edited: the picker never clears the target's
		// per-capability narrowings, because they travel with the scope
		// under one presence bit and an empty list would erase them.
		overrides := a.authorityPicker.Overrides()
		mode, target := a.authorityPicker.Mode(), a.authorityPicker.TargetID()
		a.authorityPicker.Close()
		if mode == PickerModeSession {
			a.sessionCaps = caps
			if spec == "" {
				a.sessionScope = protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
				a.sessionOverrides = nil
			} else {
				sc, err := cli.ParseScope(spec)
				if err != nil {
					a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error()))
					return nil
				}
				a.sessionScope = sc
				a.sessionOverrides = overrides
			}
			label := capsLabel(a.sessionCaps) + "  scope=" + cli.ScopeLabel(a.sessionScope)
			if ov := cli.OverridesLabel(a.sessionOverrides); ov != "" {
				label += " +" + ov
			}
			a.cmdresult.Append(OKStyle.Render("defaults set: ") + label)
			return nil
		}
		if a.client == nil {
			a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
			return nil
		}
		sc, err := cli.ParseScope(spec)
		if err != nil {
			// Unreachable for picker-built specs; surfaced rather
			// than swallowed in case the serializer regresses.
			a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error()))
			return nil
		}
		return DoSetCaps(a.client, cli.SetCapsOpts{
			TaskID: target, Caps: &caps, Scope: &sc, Overrides: overrides,
			Cascade: cascade, KeepConns: keep,
		})
	}
	return nil
}

// Runner picker intercepts keys when open (digit picks, Esc cancels).
func (a *App) inRunnerPicker(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.runnerPicker.Close()
		a.cmdresult.Append(WarnStyle.Render("runner pick cancelled"))
		return nil
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
		return DoOpenDetachableSession(a.client, p.repo, sel, p.extraArgs, p.resumeTaskID, p.auth, p.capsOverride, p.resumeConversation, agentProfile)
	}
	return nil
}

// Forward-stop picker intercepts keys when open (digit selects, Esc cancels).
func (a *App) inForwardPicker(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc {
		a.forwardPicker.Close()
		return nil
	}
	if sess := a.forwardPicker.Pick(msg.String()); sess != nil {
		a.forwardPicker.Close()
		return a.killLocalForward(sess)
	}
	return nil
}

// Port-forward modal intercepts ALL keys when open.
func (a *App) inPortForwardModal(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		a.portForwardModal.Close()
		return nil
	case tea.KeyEnter:
		spec := a.portForwardModal.Spec()
		taskID := a.portForwardModal.TaskID()
		mode := a.portForwardModal.Mode()
		a.portForwardModal.Close()
		if spec == "" {
			a.cmdresult.Append(WarnStyle.Render("forward cancelled (empty spec)"))
			return nil
		}
		a.nextForwardID++
		if mode == ForwardRemote {
			return DoStartRemoteForward(a.client, taskID, spec, a.nextForwardID, a.program, false)
		}
		return DoStartPortForward(a.client, taskID, spec, a.nextForwardID, a.program, false)
	}
	var pfcmd tea.Cmd
	a.portForwardModal, pfcmd = a.portForwardModal.Update(msg)
	return pfcmd
}

// Raw-connect modal intercepts ALL keys when open: the input line needs
// every printable rune while composing a target or a send line, which
// is also why the pane keys below are arrows and esc rather than
// letters. Esc only HIDES — panes are connections, and closing one is
// `x`. Enter is overloaded by which tab is selected: on [+ new] it
// parses the entered spec and opens a pane tagged with a fresh
// generation; on a live pane it sends the line.
func (a *App) inRawModal(msg tea.KeyMsg) tea.Cmd {
	// Three screens, three key sets. The list has no focused text
	// input, so a letter is free there (`n`); the other two take every
	// printable rune, which is why their actions are chords.
	switch a.rawModal.Mode() {
	case rawModeList:
		switch {
		case msg.Type == tea.KeyEsc:
			a.rawModal.Hide()
			return nil
		case msg.Type == tea.KeyEnter:
			a.rawModal.OpenSelected()
			return nil
		case msg.Type == tea.KeyCtrlX:
			a.rawModal.CloseSelectedPane()
			return nil
		case msg.String() == "n":
			a.rawModal.BeginNew()
			return nil
		}
		return a.rawModal.UpdateList(msg)

	case rawModeNew:
		switch msg.Type {
		case tea.KeyEsc:
			a.rawModal.BackToList()
			return nil
		case tea.KeyEnter:
			// Reported inside the modal: cmdresult is behind it.
			host, port, ok := a.rawModal.TargetOrError()
			if !ok {
				return nil
			}
			// Each attempt gets its own generation and its own pane, so
			// two attempts can never share one — which is what used to
			// let the loser's close tear down the winner.
			a.rawGenSeq++
			gen := a.rawGenSeq
			taskID := a.rawModal.TaskID()
			a.rawModal.AddPane(taskID, host, port, gen)
			return DoStartRawForward(a.client, taskID, host, port, gen, a.program)
		}
		var rmcmd tea.Cmd
		a.rawModal, rmcmd = a.rawModal.Update(msg)
		return rmcmd
	}

	// rawModeView.
	switch msg.Type {
	case tea.KeyEsc:
		a.rawModal.BackToList()
		return nil
	case tea.KeyCtrlX:
		a.rawModal.CloseSelectedPane()
		return nil
	case tea.KeyCtrlT:
		a.rawModal.ToggleForm()
		return nil
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		if !a.rawModal.InForm() {
			return a.rawModal.ScrollViewport(msg)
		}
	}
	if a.rawModal.InForm() {
		switch msg.Type {
		case tea.KeyTab:
			a.rawModal.FormNextField()
			return nil
		case tea.KeyLeft, tea.KeyRight:
			d := 1
			if msg.Type == tea.KeyLeft {
				d = -1
			}
			a.rawModal.FormCycleMethod(d)
			return nil
		case tea.KeyEnter:
			if err := a.rawModal.SendForm(); err != nil {
				a.rawModal.SetActiveNote(err.Error())
			}
			a.rawModal.Refresh()
			return nil
		}
		return a.rawModal.UpdateForm(msg)
	}
	switch msg.Type {
	case tea.KeyCtrlR:
		a.rawModal.ToggleHex()
		return nil
	case tea.KeyCtrlO:
		a.rawModal.CycleNewline()
		return nil
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
		return nil
	}
	var rmcmd tea.Cmd
	a.rawModal, rmcmd = a.rawModal.Update(msg)
	return rmcmd
}
