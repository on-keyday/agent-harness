//go:build js

package cli

import (
	"errors"

	agentexec "github.com/on-keyday/objtrsf/exec"
)

// execRemoteShell: a browser has no local terminal to hand over. The WebUI
// never sets Terminal (exec -t is CLI-only); this exists so the package builds.
func execRemoteShell(*agentexec.CommandExecutionStream) error {
	return errors.New("exec: a terminal exec needs a local terminal, which the browser does not have")
}
