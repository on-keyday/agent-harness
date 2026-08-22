package cli

import "io"

// InputModeReset stops the local terminal from SENDING things, and is written
// to it when a client hands the terminal back after an attach.
//
// Why a client has to do this at all. The server re-establishes the session's
// DEC private modes on every attach (server/mode_tracker.go preamble), because
// a mode whose controlling sequence has scrolled out of the ring would
// otherwise be lost to the reattaching emulator. That replay does not
// distinguish modes that change how the screen LOOKS from modes that change
// what the terminal SENDS — so attaching to a session whose app enabled mouse
// tracking or bracketed paste turns those on in the operator's own terminal.
// Nothing turned them off again: leaving raw mode restores termios, which is
// the kernel line discipline and has no bearing on emulator state.
//
// The observed result, after detaching from a session that had run a
// mouse-using, palette-probing TUI: the terminal kept emitting SGR mouse
// reports at the operator's shell prompt, where readline consumed the `ESC[<`
// introducer as an unbound key sequence, rang the bell and self-inserted the
// remainder — `35;65;36M` — on every mouse movement. Reproduced by feeding
// those exact bytes to a bash pty.
//
// Reset unconditionally rather than from tracked state: the client does not
// know what was set, resetting an unset mode is a no-op, and the point is to
// leave the terminal in the state a fresh shell expects however it got here.
// This is what any full-screen program does on exit, and what `tmux detach`
// does — the harness was simply not doing it.
//
// Screen-affecting modes are deliberately NOT in this list. Cursor visibility,
// autowrap and the alternate screen decide what the operator SEES; clearing
// them here would be a display change nobody asked for, and the alternate
// screen in particular is content, not a flag (see modeTracker's
// excludedFromPreamble for the same boundary drawn server-side).
const InputModeReset = "" +
	"\x1b[?1l" + // DECCKM: cursor keys back to normal (not application) encoding
	"\x1b[?9l" + // X10 mouse reporting
	"\x1b[?66l" + // DECNKM: numeric keypad
	"\x1b[?1000l" + // X11 mouse: button press/release
	"\x1b[?1001l" + // highlight mouse tracking
	"\x1b[?1002l" + // button-event tracking (motion while pressed)
	"\x1b[?1003l" + // any-event tracking (motion always) — the noisiest one
	"\x1b[?1004l" + // focus in/out reporting
	"\x1b[?1005l" + // UTF-8 mouse coordinates
	"\x1b[?1006l" + // SGR mouse coordinates
	"\x1b[?1015l" + // urxvt mouse coordinates
	"\x1b[?1016l" + // SGR-pixel mouse coordinates
	"\x1b[?2004l" + // bracketed paste
	"\x1b[?2031l" // colour-scheme change notifications

// RestoreLocalInputModes writes InputModeReset to w, which must be the local
// terminal. Errors are ignored on purpose: this runs on the way out, the
// terminal may already be gone, and there is nothing a caller could do about
// it that would not be worse than a terminal it no longer owns.
func RestoreLocalInputModes(w io.Writer) {
	_, _ = io.WriteString(w, InputModeReset)
}
