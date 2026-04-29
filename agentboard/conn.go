package agentboard

import "github.com/on-keyday/agent-harness/runner/protocol"

// ConnState is per-attached-client transient state. The persistent piece —
// subscription pattern set — lives in the shared *taskState (one per
// (runner_id, task_id)) so it survives across the short-lived per-subcommand
// harness-cli connections.
type ConnState struct {
	notify chan struct{} // pinged when a relevant publish happens
	task   *taskState
}

func newConnState(task *taskState) *ConnState {
	return &ConnState{
		notify: make(chan struct{}, 1),
		task:   task,
	}
}

func (c *ConnState) ping() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *ConnState) matches(topic string) bool {
	if c == nil || c.task == nil {
		return false
	}
	return c.task.matches(topic)
}

// Identity returns the authenticated (RunnerID, TaskID, hostname) captured at
// Attach time. The server uses this to attribute published messages to the
// correct sender without trusting agent-supplied fields.
func (c *ConnState) Identity() (protocol.RunnerID, protocol.TaskID, string) {
	if c == nil || c.task == nil {
		return protocol.RunnerID{}, protocol.TaskID{}, ""
	}
	return c.task.identity()
}
