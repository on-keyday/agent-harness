package server

import "github.com/on-keyday/agent-harness/runner/protocol"

// taskAudience is what one subscriber may see of one task's status event: the
// same answer handleList gives for that task's row.
type taskAudience uint8

const (
	audienceNone      taskAudience = iota // the caller's ls does not show the task
	audienceFull                          // the task is in the caller's visibility set
	audienceParentHop                     // the caller's creator, shown redacted
)

// taskEventAudience mirrors handleList's filter (listVisibleToCaller), so a
// tasks.status subscriber is never told about a task its own `ls` would deny,
// and sees its parent exactly as `ls` shows it — redacted.
func (h *TaskHandler) taskEventAudience(subCID, taskHex string) taskAudience {
	all, allowed, parentHex := h.listVisibleToCaller(subCID)
	switch {
	case all || allowed[taskHex]:
		return audienceFull
	case parentHex != "" && parentHex == taskHex:
		return audienceParentHop
	}
	return audienceNone
}

// redactTaskStatusEvent strips an event to what a parent-hop row keeps, as
// redactParentTaskInfo does for the List row: status, kind and the live
// counters stay, and so do lifecycle and the program's state/kind
// (liveness); the progress line, the title and the program's msg are content
// and go.
func redactTaskStatusEvent(ev *protocol.TaskStatusEvent) {
	ev.SetProgress(nil)
	ev.ProgressAt = 0
	ev.ProgressAgeMs = 0
	ev.SetTitle(nil)
	ev.SetProgramMsg(nil)
}
