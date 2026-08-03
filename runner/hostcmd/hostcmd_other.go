//go:build !windows

package hostcmd

import "os/exec"

// Nothing to do: no other platform pops a window for a console process.
func configure(cmd *exec.Cmd) *exec.Cmd { return cmd }
