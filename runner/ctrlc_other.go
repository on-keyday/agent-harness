//go:build !windows

package runner

// clearInheritedCtrlCIgnore is a no-op off Windows: there is no inheritable
// "ignore CTRL+C" process attribute to undo. A Unix PTY's line discipline turns
// an incoming 0x03 into SIGINT for the foreground process group with nothing to
// arrange. See ctrlc_windows.go for what this exists to fix.
func clearInheritedCtrlCIgnore() {}
