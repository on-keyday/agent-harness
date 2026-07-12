package tui

import "github.com/on-keyday/agent-harness/runner/protocol"

// taskActionKind is the intent decided by resumeReattachAction.
type taskActionKind int

const (
	actionNone taskActionKind = iota
	actionReattach
	actionResume
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
	if t.Kind == protocol.TaskKind_Interactive &&
		(t.Status == protocol.TaskStatus_Detached || t.Status == protocol.TaskStatus_Running) {
		return taskAction{Kind: actionReattach}
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return taskAction{Kind: actionResume, ResumeConversation: withContinue}
	}
	// Nothing applies — say WHY for this specific task, not just what r/R
	// could have done on some other one.
	switch {
	case t.Status == protocol.TaskStatus_Running && t.Kind == protocol.TaskKind_Oneshot:
		// A prompt-driven one-shot (claude -p) has no PTY, so there is
		// nothing to attach while it runs. The takeover path is manual and
		// destructive (kills the in-flight turn), so it stays two explicit
		// keystrokes instead of hiding behind r.
		return taskAction{Kind: actionNone,
			Hint: "one-shot task is still running: no PTY to attach — c cancels it, then r reopens the conversation as an interactive session"}
	case t.Status == protocol.TaskStatus_Queued:
		return taskAction{Kind: actionNone,
			Hint: "task is still queued: nothing to reattach yet — r resumes it after it runs, c cancels it"}
	}
	return taskAction{Kind: actionNone,
		Hint: "r/R: pick a live session (take over) or a finished task (resume)"}
}
