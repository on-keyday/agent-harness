package vtgrid

// The inverse of ANSI(): where that renders the grid as text, this renders it as
// a program that puts a terminal into this state. It lives here rather than in
// the server that sends it because it is a property of the model — and because
// vtgrid's own gates must keep calling the real sequence rather than a copy.

import (
	"fmt"
	"strings"
)

// Repaint synthesises the bytes that make a terminal show THIS screen, whatever
// state it was in before: the screen selection, the modes needed to address
// cells absolutely, every row, the cursor, and the window title.
//
// It is the counterpart ANSI() names and declines to be — "There is no cursor
// positioning and no screen clear: the result is a block of text, not a program
// that repaints a terminal." This is that program.
//
// Every ordering decision in here was made by a measurement, not by taste:
//
//   - The screen selection comes FIRST. Switching buffers is entitled to reset
//     the margins, so resetting them before the switch would be undone by it.
//
//   - Autowrap is off for the duration. Writing the last cell of the last row
//     with it on can scroll the screen out from under the paint.
//
//   - Each row is ERASED BEFORE it is painted, not after. Painting a row that
//     fills the width leaves the cursor ON the last column, so a trailing EL
//     erases the cell just written. That costs nothing on short lines and
//     everything on a TUI: with the erase trailing, htop scored 25/40 rows and
//     vim-split 21/40, while shell output stayed at 40/40.
//
//   - Rows are addressed absolutely rather than joined with ANSI()'s CRLF,
//     which sidesteps the deferred-wrap a full-width row leaves pending.
//
//   - DECTCEM (cursor visibility) is emitted BEFORE the screen selection, while
//     the cursor's POSITION is restored at the very end. Splitting the two
//     halves of "put the cursor back" looks arbitrary and is not: on a real
//     Windows console, DECTCEM issued while the alternate buffer is active does
//     not take effect on it and lands on the MAIN buffer instead — so the
//     hidden cursor stays visible AND the buffer underneath is corrupted.
//     Measured on conhost in both directions; issued before the switch it
//     survives into the alt buffer, which is why it sits here. The three
//     emulators that do honour it either way are indifferent to the move.
//
//     Which is only half of it, hence the ESC[?1049l that now precedes the
//     whole thing when the target is the alternate buffer. Conhost freezes the
//     alternate buffer's cursor visibility at whatever MAIN held when the app
//     issued its own ESC[?1049h, and nothing emitted from inside can move it in
//     EITHER direction. An observer already on that buffer — which a real
//     attach reaches routinely, see replayTailSizes — would otherwise be stuck
//     with what the app inherited: htop keeps a cursor it wanted hidden (a
//     stray block in a blank corner, cosmetic) and opencode-tui LOSES the one
//     it wanted shown, which is its input caret sitting at the typing
//     position. Forcing the leave makes the entry a real switch again, so the
//     DECTCEM has something to ride across. Measured in both directions on
//     conhost.
//
//     The cost is a genuine buffer switch on every attach to an alt-screen
//     session. Whether that is a PERCEPTIBLE flash is not known: the sequence
//     arrives as one contiguous write, so the terminal sees both halves within
//     a frame and may never present the main buffer at all. Nobody has watched
//     it on a live attach yet. If it does flash, the fix is to condition the
//     leave on the visibility actually needing to change — but that requires
//     knowing the observer's state, which the server does not.
//
// The window TITLE is carried even though it is not on the grid, because the
// alternative is that it is nowhere. It reaches an observer exactly once — in
// the OSC that set it — and lives only in this model afterwards, so a replay
// whose ring no longer holds that OSC hands the observer a screen with no title
// at all. An app that titles itself at startup and then runs for an hour is the
// ordinary case rather than the corner, and the symptom is `session snapshot`
// reporting an empty title for a session that plainly has one.
//
// Emitted only when non-empty. This model cannot tell "never titled" from
// "titled with an empty string" — Title() answers "" to both — and of the two
// readings, the one that leaves the observer's own title alone is the safer
// default.
//
// Not carried here, on purpose: the input-affecting private modes (bracketed
// paste, mouse, application cursor keys). vtgrid neither tracks nor exposes
// them; server/mode_tracker.go's preamble does, and the two are complementary.
// The preamble must be emitted AFTER this sequence — it restores DECOM/DECAWM,
// which this one deliberately clears in order to address cells absolutely.
func (t *Terminal) Repaint() []byte {
	_, rows := t.Size()
	x, y, vis := t.Cursor()
	var b strings.Builder
	// Leave the alternate buffer before entering it, so that the entry below is
	// a REAL switch even for an observer already sitting on it. Without this the
	// ESC[?1049h is a no-op and the DECTCEM has nothing to ride across; see the
	// Windows note above for why that is not recoverable afterwards.
	if t.AltScreen() {
		b.WriteString("\x1b[?1049l")
	}
	// DECTCEM before the screen selection, and NOT beside the final cursor
	// positioning where it would read more naturally. On a real Windows
	// console the two halves of "restore the cursor" belong on opposite sides
	// of the switch; see the Windows note above.
	if vis {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	if t.AltScreen() {
		b.WriteString("\x1b[?1049h")
	} else {
		b.WriteString("\x1b[?1049l")
	}
	b.WriteString("\x1b[r")   // DECSTBM: full screen
	b.WriteString("\x1b[?6l") // DECOM off
	b.WriteString("\x1b[?7l") // DECAWM off
	body := strings.Split(t.ANSI(), "\r\n")
	for y := 0; y < rows; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0m\x1b[K", y+1)
		if y < len(body) {
			b.WriteString(body[y])
		}
	}
	b.WriteString("\x1b[0m")
	b.WriteString("\x1b[?7h")
	// No measured ordering constraint here, unlike everything above it: an OSC
	// writes no cell and moves no cursor. It sits before the cursor positioning
	// only so that "the cursor is restored at the very end" stays literally
	// true.
	if title := t.Title(); title != "" {
		b.WriteString(titleOSC(title))
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
	return []byte(b.String())
}

// titleOSC renders a window title as `OSC 0 ; title BEL` — command 0 because it
// sets the icon name as well, which is the form the agents here emit, and BEL
// because it is the terminator every emulator honours (this parser's own reason
// for refusing the eight-bit ST is in Terminal.str).
//
// C0 controls and DEL are DROPPED rather than escaped or passed through. The
// parser cannot have stored a BEL or an ESC — both end the string — but nothing
// else in that range is filtered on the way IN, so a title can hold a CR or a
// CAN. Terminals disagree about those inside an OSC: xterm treats CAN as an
// abort, which ends the sequence and prints its tail onto the screen this
// function has just painted. Dropping them costs a byte of a hostile title and
// removes the disagreement. Filtering by byte is safe for UTF-8 — every byte of
// a multi-byte rune is >= 0x80, so no filtered byte can be part of one.
func titleOSC(title string) string {
	var s strings.Builder
	s.Grow(len(title) + 5)
	s.WriteString("\x1b]0;")
	for i := 0; i < len(title); i++ {
		if b := title[i]; b < 0x20 || b == 0x7f {
			continue
		}
		s.WriteByte(title[i])
	}
	s.WriteString("\x07")
	return s.String()
}
