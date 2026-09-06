// Package hostcmd starts the short-lived helper processes the harness runs on
// the host — git, xauth — with the platform attributes they need.
//
// On Windows a console application started by exec.Command gets its own
// console window, which appears on the desktop for as long as the process
// lives. The runner shells out to git constantly (every git_query, every
// worktree create and cleanup), so the operator sees terminals blinking in and
// out while nothing is wrong. CREATE_NO_WINDOW suppresses it.
//
// This is a constructor rather than a "call Hide(cmd) after building it"
// helper on purpose. A helper is one more thing every future call site has to
// remember, and a site that forgets is invisible until somebody watches a
// Windows desktop. hostcmd_test.go fails the build if a bare exec.Command
// starts git or xauth anywhere in the runner or cli packages.
//
// NOT for processes the operator is meant to see: `file edit` launches the
// user's $EDITOR on purpose (cli/file_edit.go), which keeps os/exec.
//
// This exclusion used to name runner/process.go as well, on the ground that
// "the agent runs in a PTY the operator attaches to". That is true of the
// ATTACHABLE agent and it is not that file: the attachable one goes through
// agentexec.ExecuteCommandWithOption (runner/session.go, exec_run.go,
// streamtask.go), which carries the flag itself, while runner/process.go is the
// ONESHOT path whose stdout goes to a LogSink and whose console nobody ever
// looks at. So every oneshot task on a Windows runner popped a window for its
// whole life, reported from the desktop it happened on. The exclusion was
// written per FILE where it meant per PATH.
//
// One host-helper path does NOT come through here and cannot: `exec` runs its
// child inside objtrsf (exec.ExecuteCommandWithOption), a different module, so
// hostcmd_test.go's source walk has no way to reach it. objtrsf carries the
// same flag itself, defaulted on and spelled as an opt-out
// (ExecuteOption.ShowConsoleWindow), with this package named as where the
// discipline came from. Anything else that starts a host process from OUTSIDE
// this module owes the same, and no test here will say so.
package hostcmd

import (
	"context"
	"os/exec"
)

// Command mirrors exec.Command.
func Command(name string, args ...string) *exec.Cmd {
	return configure(exec.Command(name, args...))
}

// CommandContext mirrors exec.CommandContext.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return configure(exec.CommandContext(ctx, name, args...))
}
