//go:build windows

package hostcmd

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. The process runs without a console
// window; it still gets a console, so stdout/stderr redirection is unaffected.
// Spelled out rather than imported from x/sys/windows, which is only an
// indirect dependency here.
const createNoWindow = 0x08000000

func configure(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	return cmd
}
