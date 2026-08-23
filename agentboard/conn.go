package agentboard

import (
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ConnState is per-attached-client transient state. The persistent piece —
// subscription pattern set — lives in the shared *taskState (one per
// (runner_id, task_id)) so it survives across the short-lived per-subcommand
// harness-cli connections.
type ConnState struct {
	notify chan struct{} // pinged when a relevant publish happens
	task   *taskState

	// done is closed by Board.Detach when the agent's connection goes away.
	// A blocked Board.Wait selects on it: the server builds the wait context
	// from context.Background() plus the client's timeout, so without this a
	// killed CLI leaves the wait — and the waiter count that suppresses this
	// task's wake — alive for the rest of that timeout, up to five minutes.
	closeOnce sync.Once
	done      chan struct{}
}

func newConnState(task *taskState) *ConnState {
	return &ConnState{
		notify: make(chan struct{}, 1),
		task:   task,
		done:   make(chan struct{}),
	}
}

// close marks the connection gone. Idempotent: Board.Detach can be reached
// more than once for one ConnState.
func (c *ConnState) close() {
	c.closeOnce.Do(func() { close(c.done) })
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

// Identity returns the authenticated (RunnerID, TaskID, hostname, agentProfile)
// captured at Attach time. The server uses this to attribute published messages
// to the correct sender without trusting agent-supplied fields.
func (c *ConnState) Identity() (protocol.RunnerID, protocol.TaskID, string, string) {
	if c == nil || c.task == nil {
		return protocol.RunnerID{}, protocol.TaskID{}, "", ""
	}
	return c.task.identity()
}
