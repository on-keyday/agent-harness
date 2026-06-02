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
	Kind       taskActionKind
	ResumeArgs []string // claude args for actionResume (["--continue"] or nil)
	Hint       string   // shown for actionNone
}

// resumeReattachAction decides what r (withContinue=true) / R (withContinue=false)
// do for the selected task: reattach a live Detached+Detachable session, resume a
// finished task into a new detachable session (with or without --continue), or
// nothing (with a hint) for anything else.
func resumeReattachAction(t *protocol.TaskInfo, withContinue bool) taskAction {
	if t == nil {
		return taskAction{Kind: actionNone, Hint: "no task selected"}
	}
	if t.Status == protocol.TaskStatus_Detached && t.Detachable() {
		return taskAction{Kind: actionReattach}
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		var args []string
		if withContinue {
			args = []string{"--continue"}
		}
		return taskAction{Kind: actionResume, ResumeArgs: args}
	}
	return taskAction{Kind: actionNone,
		Hint: "r/R: pick a detached session (reattach) or a finished task (resume)"}
}
