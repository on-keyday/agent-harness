package tui

import "github.com/on-keyday/agent-harness/runner/protocol"

// taskActionKind is the intent decided by resumeReattachAction.
type taskActionKind int

const (
	actionNone taskActionKind = iota
	actionReattach
	actionResume
	// actionChat opens the event-stream kind's conversation view. It is what
	// reattach means for a kind with no terminal to hand over: the operator
	// still drives the agent, through turns and approvals rather than through
	// keystrokes.
	actionChat
	// actionX11Restart is D on a live session this TUI holds no X11 tunnel
	// for: after a y/n it is cancelled and resumed with X11, conversation
	// kept, because a running agent's environment cannot gain DISPLAY.
	actionX11Restart
)

// taskAction is what the r/R keys should do for the selected task.
type taskAction struct {
	Kind               taskActionKind
	ResumeConversation bool   // actionResume only; asks the runner to continue agent memory
	Hint               string // shown for actionNone
}

// resumeReattachAction decides what r (withContinue=true) / R (withContinue=false)
// do for the selected task: reattach a live interactive session (Detached, or
// Running via takeover — the server force-closes the prior client), resume a
// finished task into a new detachable session (with or without --continue), or
// nothing (with a hint) for anything else.
func resumeReattachAction(t *protocol.TaskInfo, withContinue bool) taskAction {
	if t == nil {
		return taskAction{Kind: actionNone, Hint: "no task selected"}
	}
	// A live interactive session can be re-entered whether it is Detached (no
	// client) or Running (takeover — SessionMux.Attach force-closes the prior
	// client), matching the WebUI's kind==Interactive && Running||Detached
	// reattach gate. Deliberately NOT gated on t.Detachable(): a task first
	// seen via a tasks.status event is stubbed into the table from
	// TaskStatusEvent, which carries kind+status but no detachable bit, so a
	// real `session new` session would show Detachable=false until a snapshot
	// refresh lands — and be spuriously refused here. The server is the
	// authority anyway: attaching something truly non-attachable returns a
	// clean AttachSession error (not_detachable / not_interactive).
	// IsPTYKind, not IsSessionKind: reattach hands the terminal to a PTY
	// splice, which would paint an event stream's NDJSON as terminal bytes.
	// The stream kind is not waiting on a renderer — it has one, the chat.
	// What it lacks is a TAKEOVER, which is a PTY concept: there is no seat to
	// take. See the live-stream case below for what r does instead.
	if protocol.IsPTYKind(t.Kind) && taskSessionAlive(t.Status) {
		return taskAction{Kind: actionReattach}
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return taskAction{Kind: actionResume, ResumeConversation: withContinue}
	}
	// Nothing applies — say WHY for this specific task, not just what r/R
	// could have done on some other one.
	switch {
	case t.Kind == protocol.TaskKind_Stream && taskSessionAlive(t.Status):
		// Not a take-over — there is no seat — but not "nothing" either: the
		// chat view drives this kind, sending turns and answering approvals.
		// It briefly returned a hint pointing at the logs pane, which was true
		// and half the story: following is read-only, and r is where an
		// operator reaches to DRIVE something.
		return taskAction{Kind: actionChat}
	case t.Status == protocol.TaskStatus_Queued:
		return taskAction{Kind: actionNone,
			Hint: "task is still queued: nothing to reattach yet — r resumes it after it runs, c cancels it"}
	}
	return taskAction{Kind: actionNone,
		Hint: "r/R: pick a live session (take over) or a finished task (resume)"}
}

// x11OpenAction decides what D does for the selected task. held reports
// whether this TUI holds an X11 tunnel (a ForwardX11 registry entry) for it.
// A live session with one is reattached, since its X is already there; a
// live session without one can only gain DISPLAY by restarting, which is
// destructive and so asked first (the one-shot takeover above is two explicit
// keystrokes for the same reason); a finished task is resumed with X11. A
// background task is refused by the server for X11 (no client to host the
// tunnel), and its resume stays in background, so both say so here.
func x11OpenAction(t *protocol.TaskInfo, held bool) taskAction {
	if t == nil {
		return taskAction{Kind: actionNone, Hint: "no task selected"}
	}
	background := t.RunMode == protocol.RunMode_Background
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		if background {
			return taskAction{Kind: actionNone,
				Hint: "D: a background task resumes in background, which has no client to host the X tunnel"}
		}
		return taskAction{Kind: actionResume, ResumeConversation: true}
	}
	if !protocol.IsPTYKind(t.Kind) {
		return taskAction{Kind: actionNone, Hint: "D: X11 needs a terminal session, and this task has none"}
	}
	if !taskSessionAlive(t.Status) {
		return taskAction{Kind: actionNone,
			Hint: "D: task is still queued — D gives it X11 once it runs or after it finishes"}
	}
	if background {
		return taskAction{Kind: actionNone,
			Hint: "D: a background session has no client to host the X tunnel — E switches it to foreground first"}
	}
	if held {
		return taskAction{Kind: actionReattach}
	}
	return taskAction{Kind: actionX11Restart}
}
