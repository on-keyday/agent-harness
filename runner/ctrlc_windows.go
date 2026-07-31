//go:build windows

package runner

import "syscall"

// clearInheritedCtrlCIgnore undoes the "ignore CTRL+C" attribute this process
// inherited from the way it was launched, so that PTY children spawned after
// this call can receive Ctrl-C.
//
// scripts/daemon.py:154-157 starts the runner with
// DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP. CREATE_NEW_PROCESS_GROUP disables
// CTRL+C for the process, and that attribute is INHERITED by every descendant —
// so agent-runner → ConPTY → cmd.exe → whatever it runs all began life ignoring
// Ctrl-C. The visible symptom was that injecting 0x03 into a Windows session
// interrupted nothing, while cmd.exe still reacted (its line editor reads the
// byte) and ssh still worked (it forwards the byte to a remote PTY) — neither of
// which involves a console control event.
//
// Measured on Windows before writing this (see the notes below), because none of
// it is obvious:
//   - The failure reproduces in an ordinary non-ConPTY console too, so it is not
//     about pseudoconsole or process-group topology, and no in-console helper or
//     AttachConsole dance is needed.
//   - ConPTY does translate an injected 0x03 into a real CTRL_C_EVENT. That path
//     was never broken.
//   - ENABLE_PROCESSED_INPUT was already set (0x1f7); it only governs programs
//     actively reading console input, not ones waiting for control events.
//   - SetConsoleCtrlHandler(handler, TRUE) does NOT clear the inherited ignore.
//     Only SetConsoleCtrlHandler(NULL, FALSE) does.
//   - The call succeeds in this process even though it has NO console
//     (GetConsoleWindow()==0, GetConsoleProcessList fails): the ignore flag is
//     plain per-process state, not something routed through a console object,
//     which is why it is settable and inheritable without one.
//
// It only has to run before the first PTY child is spawned — the attribute is
// read at CreateProcess time — so it need not precede pseudoconsole creation.
//
// Called from Connect and ListenAndServe, the two entry points agent-runner
// actually uses (dial mode hands Connect to cli.PersistLoop; listen mode calls
// ListenAndServe). Connect runs again on every reconnect, so this is not
// once-per-process — that is fine, it sets idempotent process state. It was
// first attached to runner.Run, which only the integration suite calls, and so
// never executed in production at all.
//
// This cannot regress daemon.py's shutdown path: that uses CTRL_BREAK_EVENT, and
// the inherited ignore is Ctrl-C-specific. Nor can it expose the runner itself
// to a stray Ctrl-C, since the runner has no console to deliver one.
//
// Errors are deliberately swallowed: a runner that cannot clear the flag should
// still run, just without interruptible children.
func clearInheritedCtrlCIgnore() {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	// SetConsoleCtrlHandler(NULL, FALSE): remove the inheritable ignore.
	_, _, _ = proc.Call(0, 0)
}
