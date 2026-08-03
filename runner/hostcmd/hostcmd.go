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
// NOT for processes the operator is meant to see: the agent itself runs in a
// PTY the operator attaches to (runner/process.go), and `file edit` launches
// the user's $EDITOR on purpose (cli/file_edit.go). Both keep os/exec.
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
