package server

import (
	"sort"
	"strconv"
	"sync"
)

// modeTracker watches a terminal output byte stream for DEC private mode
// set/reset sequences (CSI ? Pm [;Pm...] h|l) and remembers the last value of
// each mode. On reattach the server replays a byte-ring snapshot to a new
// client whose emulator starts in its default state; any mode whose
// controlling sequence has already scrolled out of the ring window would
// otherwise be lost. The classic case: an app hides the cursor once at startup
// with ESC[?25l, that frame is later evicted, the reattaching emulator defaults
// to cursor-visible, and the app never re-emits the hide — leaving a stray
// blinking cursor over a UI that assumes none. preamble() reconstructs the
// missing modes so the replay starts from the right terminal state.
//
// Only content-independent 1-bit modes are reconstructable this way. Modes that
// imply screen *content* (alternate screen 47/1047/1048/1049) or are transient
// framing (synchronized output 2026) are tracked-but-never-replayed: emitting
// them without the content they wrap would make the display worse, not better.
// Reconstructing alt-screen content needs a full terminal-state model, which
// this is deliberately not.
type modeTracker struct {
	mu    sync.Mutex
	modes map[int]bool // private mode number -> last seen value (true = set/h)

	// Incremental parser state. It persists across feed() calls so a sequence
	// split across two frames is still recognised at the boundary.
	st     parseState
	params []byte
}

type parseState int

const (
	stNormal parseState = iota
	stEsc
	stCSI
	stCSIIgnore
	stCSIPrivate
)

// maxModeParams caps the accumulated parameter bytes; malformed/oversized input
// resyncs to NORMAL rather than growing without bound.
const maxModeParams = 64

func newModeTracker() *modeTracker {
	return &modeTracker{modes: make(map[int]bool)}
}

// decCursorVisible is DECTCEM. It is replayed like any other tracked mode, but
// its POSITION in the preamble is load-bearing on Windows, so it is emitted by
// hand ahead of the alternate-screen entry rather than in the sorted loop.
const decCursorVisible = 25

// excludedFromPreamble reports modes we track but never replay (see type doc).
func excludedFromPreamble(mode int) bool {
	switch mode {
	case 47, 1047, 1048, 1049, // alternate screen / cursor save+restore: imply content
		2026: // synchronized output: transient begin/end framing
		return true
	}
	return false
}

// feed advances the parser over one chunk of terminal output (the payload of a
// Stdout/Stderr frame, headers already stripped).
func (t *modeTracker) feed(payload []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range payload {
		switch t.st {
		case stNormal:
			if b == 0x1b {
				t.st = stEsc
			}
		case stEsc:
			switch b {
			case '[':
				t.st = stCSI
			case 0x1b:
				// ESC ESC: keep waiting for the intro byte.
			default:
				t.st = stNormal
			}
		case stCSI:
			switch {
			case b == '?':
				t.st = stCSIPrivate
				t.params = t.params[:0]
			case b >= 0x40 && b <= 0x7e:
				t.st = stNormal // a parameterless non-private CSI ended
			default:
				t.st = stCSIIgnore // non-private CSI: skip to its final byte
			}
		case stCSIIgnore:
			if b >= 0x40 && b <= 0x7e {
				t.st = stNormal
			}
		case stCSIPrivate:
			switch {
			case (b >= '0' && b <= '9') || b == ';':
				if len(t.params) >= maxModeParams {
					t.st = stNormal
					t.params = t.params[:0]
				} else {
					t.params = append(t.params, b)
				}
			case b == 'h' || b == 'l':
				t.applyParams(b == 'h')
				t.st = stNormal
				t.params = t.params[:0]
			default: // any other final byte (e.g. ESC[?...$p query): ignore
				t.st = stNormal
				t.params = t.params[:0]
			}
		}
	}
}

// applyParams records each ';'-separated mode number in t.params with value set.
// Caller holds t.mu.
func (t *modeTracker) applyParams(set bool) {
	start := 0
	for i := 0; i <= len(t.params); i++ {
		if i == len(t.params) || t.params[i] == ';' {
			if i > start {
				if n, err := strconv.Atoi(string(t.params[start:i])); err == nil {
					t.modes[n] = set
				}
			}
			start = i + 1
		}
	}
}

// onAltScreen reports whether the session is currently in an alternate-screen
// buffer (any of the 47 / 1047 / 1049 modes last seen set). Reattach uses this
// to decide replay scope: while a full-screen app is live we replay the whole
// ring (the app repaints over any partial frame), but once it has exited we
// skip the now-meaningless alt-screen episode — replaying its absolute-cursor
// frame fragments onto the primary screen is exactly what corrupts the display.
func (t *modeTracker) onAltScreen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modes[47] || t.modes[1047] || t.modes[1049]
}

// preamble returns the escape-sequence bytes that re-establish every tracked,
// non-excluded mode at its last-known value, in ascending mode order for
// determinism. nil if there is nothing to restore.
//
// Special case — the alternate screen. The alt-screen modes (47/1047/1049) are
// excluded from the generic loop because we must never replay a *reset*
// (ESC[?1049l) on the primary screen, and replaying a stale *set* when the app
// has already exited would strand the client on an empty alt buffer. But when
// the session is CURRENTLY in the alt screen (a full-screen app is live), the
// reattaching client must enter it too — otherwise, if the establishing
// ESC[?1049h has already been evicted from the ring window, the app's
// absolute-cursor frame fragments replay onto the primary screen and corrupt
// it. So we emit the entry near the front (before the mouse modes and the
// replayed ring), and only when on the alt screen. The reset direction is left
// to SnapshotFrom, which trims a finished episode instead.
//
// Three sequences therefore lead, in this order, and the order is not
// cosmetic. On a real Windows console the alternate buffer's cursor visibility
// is FROZEN at whatever the primary buffer held at the instant of the
// ESC[?1049h: DECTCEM issued afterwards, from inside the alternate buffer,
// does not reach it in either direction — it silently lands on the primary
// buffer instead. Measured on conhost; vtgrid/repaint_console_windows_test.go
// asserts it.
//
//  1. ESC[?1049l, so that the entry below is a REAL switch even when the
//     client's terminal is already on the alternate buffer (a reattach into
//     the same terminal, or a replay that already carried it there). Against a
//     client on the primary buffer this is inert.
//  2. DECTCEM at its tracked value, while the primary buffer is current, so
//     the switch carries it across.
//  3. ESC[?1049h.
//
// With DECTCEM after the entry — where ascending mode order would put it — a
// full-screen app that hides its cursor AFTER going full-screen, which is the
// ordinary pattern, leaves every reattaching Windows client with a cursor it
// asked to have hidden, and one that hides before going full-screen loses the
// cursor it asked to keep. Neither is recoverable later in the stream.
func (t *modeTracker) preamble() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []byte
	emit := func(mode int, set bool) {
		final := byte('l')
		if set {
			final = 'h'
		}
		out = append(out, 0x1b, '[', '?')
		out = append(out, strconv.Itoa(mode)...)
		out = append(out, final)
	}
	onAlt := t.modes[1049] || t.modes[1047] || t.modes[47]
	cursorVis, haveCursor := t.modes[decCursorVisible]
	// The leave exists ONLY to give the cursor bit a real switch to ride, so it
	// is emitted only when there is one to carry. With no tracked DECTCEM the
	// entry alone is what it always was, and a client already on the alternate
	// buffer is not dragged through a switch that would buy it nothing.
	if onAlt && haveCursor {
		emit(1049, false)
	}
	if haveCursor {
		emit(decCursorVisible, cursorVis)
	}
	if onAlt {
		emit(1049, true)
	}
	keys := make([]int, 0, len(t.modes))
	for m := range t.modes {
		if m == decCursorVisible {
			continue // already emitted ahead of the screen switch
		}
		if !excludedFromPreamble(m) {
			keys = append(keys, m)
		}
	}
	sort.Ints(keys)
	for _, m := range keys {
		emit(m, t.modes[m])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
