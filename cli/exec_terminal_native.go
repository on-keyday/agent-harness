//go:build !js

package cli

import agentexec "github.com/on-keyday/objtrsf/exec"

// execRemoteShell hands an exec's stream to this process's terminal. Its own
// file because RemoteShell touches a local tty, which a browser does not have.
func execRemoteShell(s *agentexec.CommandExecutionStream) error {
	return s.RemoteShell()
}
