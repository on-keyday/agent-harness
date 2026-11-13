//go:build !windows

package streamagent

import (
	"os/exec"
	"syscall"
	"time"
)

// agentStopGrace is how long the agent has between SIGTERM and SIGKILL. It
// matches objtrsf's tree-kill grace, the other half of the same stop.
const agentStopGrace = 2 * time.Second

// stopGracefully makes a cancelled context SIGTERM the agent and SIGKILL it
// only after a grace, instead of exec.CommandContext's bare SIGKILL. claude
// ends the tool processes it started in sessions of their own when it gets a
// signal it can handle, and strands them when it is killed (pillar A spec, F5).
func stopGracefully(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = agentStopGrace
}
