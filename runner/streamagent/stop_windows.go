//go:build windows

package streamagent

import "os/exec"

// stopGracefully leaves exec.CommandContext's kill in place: Windows has no
// SIGTERM to send a console process.
func stopGracefully(cmd *exec.Cmd) {}
