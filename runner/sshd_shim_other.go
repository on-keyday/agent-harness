//go:build !windows

package runner

import (
	"fmt"
	"runtime"
)

// stageSSHDShim has nothing to do on a platform whose process names are not
// what anything checks.
//
// It returns an error rather than the ordinary shell, because the ONE caller
// (shellLineArgv) already refuses sshd_parent off Windows and never reaches
// here. Handing back "sh" would make this compile into a silent fallback the
// day someone removes that guard — the caller would get a working shell, the
// command would run, and the property the caller asked for would simply not be
// there with nothing to say so.
func stageSSHDShim() (string, error) {
	return "", fmt.Errorf("sshd_parent is a Windows mechanism; this runner runs %s", runtime.GOOS)
}
