//go:build windows

// The fourth emulator: a REAL Windows console.
//
// vtgrid, x/vt and xterm.js are all software this project chose. conhost is
// not — it is what `harness-cli session attach` writes into on a live Windows
// deployment target, and it is the only one of the four that can be asked
// whether the cursor is VISIBLE (xterm.js exposes no accessor for it at all).
// The independent parser here is conhost's own VT engine: the same bytes
// repaint_test.go builds go into a console screen buffer, and the resulting
// cells come back out with ReadConsoleOutputW. Nothing in this file models a
// terminal.
//
// It deliberately reuses buildRepaint, extCorpora, loadExtCorpus, ringTail,
// trimRows, countMatch, poison and replayTailBytes from repaint_test.go rather
// than restating any of them. A copied repaint sequence would drift from the
// real one and leave this leg gating a fossil, which is the whole reason this
// is a test in this package instead of a program beside it.
//
// Every buffer it paints is a PRIVATE screen buffer that is never made active,
// so running the suite does not disturb the terminal you launched it from.

package vtgrid_test

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// TestConsoleRepaintReconstructsScreen is the Windows leg of the repaint gate:
// it asserts that a real console lands the server's screen on an observer, from
// a byte-ring replay and from the torn-off full-screen-app state alike.
func TestConsoleRepaintReconstructsScreen(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	var totalRows, totalMatch, cursorOK, visOK int
	for _, c := range extCorpora {
		data := loadExtCorpus(t, c.Name)
		server := vtgrid.New(c.Cols, c.Rows)
		if _, err := server.Write(data); err != nil {
			t.Fatalf("%s: write corpus: %v", c.Name, err)
		}
		want := trimRows(server.Lines())
		wantX, wantY, wantVis := server.Cursor()
		repaint := buildRepaint(server)

		// The two observer states the repaint has to overcome: a byte-ring replay
		// at replayTailBytes — the whole ring, which is what `session attach`
		// asks for — and the state a torn-off full-screen app leaves behind. The
		// size matters and is not a detail: a shorter tail leaves the observer on
		// the primary buffer, which is the easy half.
		observers := []struct {
			name string
			pre  []byte
		}{
			{"after-ring", ringTail(data, replayTailBytes)},
			{"poisoned", []byte(poison)},
		}

		for _, ob := range observers {
			t.Run(c.Name+"/"+ob.name, func(t *testing.T) {
				h := newConsoleScreen(t, c.Cols, c.Rows)
				conWrite(t, h, ob.pre)
				conWrite(t, h, repaint)

				got := conReadScreen(t, h, c.Cols, c.Rows)
				match := countMatch(want, got)
				totalRows += c.Rows
				totalMatch += match
				if match != c.Rows {
					for i := range want {
						if want[i] != got[i] {
							t.Errorf("row %d\n want: %q\n got : %q", i, want[i], got[i])
						}
					}
				}

				info := conScreenInfo(t, h)
				gotX, gotY := int(info.CursorPosition.X), int(info.CursorPosition.Y)
				if gotX != wantX || gotY != wantY {
					t.Errorf("cursor position = (%d,%d), want (%d,%d)", gotX, gotY, wantX, wantY)
				} else {
					cursorOK++
				}

				// No exception any more, and its removal is the point of the
				// change that landed with it. DECTCEM issued while the
				// alternate buffer is already active does not reach it (see
				// TestConsoleDECTCEMDoesNotReachAlternateBuffer), and both
				// observer states here can arrive already on that buffer — the
				// poisoned one always, the ring one whenever the replay window
				// contains the app's own ESC[?1049h, which at a real attach
				// size it does. buildRepaint now LEAVES the alternate buffer
				// before entering it, so the entry is a real switch and the
				// pre-switch DECTCEM rides across it in either direction.
				if gotVis := conCursorVisible(t, h); gotVis != wantVis {
					t.Errorf("cursor visible = %v, want %v (alt=%v)",
						gotVis, wantVis, server.AltScreen())
				} else {
					visOK++
				}
			})
		}
	}
	t.Logf("ROWS %d/%d across %d corpora in 2 observer states; cursor position %d/%d, visibility %d/%d",
		totalMatch, totalRows, len(extCorpora), cursorOK, len(extCorpora)*2, visOK, len(extCorpora)*2)
}

// TestConsoleRepaintLandsOnTheAlternateBuffer checks the buffer the repaint
// leaves the observer on, by leaving it and watching the content change.
//
// The probe is content-based, so it is blind whenever the buffer underneath
// happens to show the same screen: leaving the alternate buffer then reveals an
// identical one and there is no change to see. Such corpora are reported and
// skipped rather than counted as failures — a bare "17/18" with no explanation
// reads as a defect to the next person.
//
// Every corpus is currently visible to it (0 blind), and that is a CONSEQUENCE
// of the repaint forcing a real leave/re-enter rather than evidence the blind
// case cannot happen: with the observer already on the alternate buffer, the
// primary underneath is no longer the same screen. It was blind for
// opencode-tui when the replay was short enough to leave the observer on the
// primary buffer and the ring alone reconstructed all 40 rows. Keep the
// handling; the worked example no longer fires.
func TestConsoleRepaintLandsOnTheAlternateBuffer(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	var checked, blind int
	for _, c := range extCorpora {
		data := loadExtCorpus(t, c.Name)
		server := vtgrid.New(c.Cols, c.Rows)
		if _, err := server.Write(data); err != nil {
			t.Fatalf("%s: write corpus: %v", c.Name, err)
		}
		h := newConsoleScreen(t, c.Cols, c.Rows)
		conWrite(t, h, ringTail(data, replayTailBytes))
		conWrite(t, h, buildRepaint(server))

		before := conReadScreen(t, h, c.Cols, c.Rows)
		conWrite(t, h, []byte("\x1b[?1049l"))
		after := conReadScreen(t, h, c.Cols, c.Rows)

		differs := countMatch(before, after) != len(before)
		switch {
		case differs == server.AltScreen():
			checked++
		case server.AltScreen() && !differs:
			// The buffer underneath shows the same screen; the probe cannot
			// see the switch it is looking for.
			blind++
			t.Logf("%s: alt-screen probe is blind here — the ring replay alone "+
				"reproduces the screen, so the main buffer underneath is identical", c.Name)
		default:
			t.Errorf("%s: screen changed on ESC[?1049l = %v, want %v (alt=%v)",
				c.Name, differs, server.AltScreen(), server.AltScreen())
		}
	}
	t.Logf("alt-screen state confirmed for %d/%d corpora (%d blind, explained above)",
		checked, len(extCorpora), blind)
}

// TestConsoleHonoursAlternateScreen gates ESC[?1049h / ESC[?1049l mid-stream.
// buildRepaint opens with one or the other, so if conhost ignored them every
// alt-screen corpus would paint onto the wrong buffer.
func TestConsoleHonoursAlternateScreen(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	h := newConsoleScreen(t, 20, 4)
	conWrite(t, h, []byte("\x1b[2J\x1b[1;1HMAIN"))
	if got := conRow(t, h, 0, 20); got != "MAIN" {
		t.Fatalf("main buffer row 0 = %q, want %q", got, "MAIN")
	}
	conWrite(t, h, []byte("\x1b[?1049h"))
	if got := conRow(t, h, 0, 20); got != "" {
		t.Errorf("row 0 after ESC[?1049h = %q, want %q (a fresh alt buffer is clear)", got, "")
	}
	conWrite(t, h, []byte("\x1b[1;1HALT"))
	if got := conRow(t, h, 0, 20); got != "ALT" {
		t.Errorf("alt buffer row 0 = %q, want %q", got, "ALT")
	}
	conWrite(t, h, []byte("\x1b[?1049l"))
	if got := conRow(t, h, 0, 20); got != "MAIN" {
		t.Errorf("row 0 after ESC[?1049l = %q, want %q (the main buffer is restored)", got, "MAIN")
	}
}

// TestConsoleScrollRegionIsHonoured is the CONTROL for the DECSTBM reset test
// below, and it exists to prove that test can fail.
//
// Without it, TestConsoleHonoursScrollRegionReset passes just as happily on a
// console that ignores ESC[...r entirely: if the region is never established,
// the full-screen scroll it looks for happens for the wrong reason. This test
// asserts the region DOES constrain scrolling, so the reset test is measuring a
// reset rather than an absence.
func TestConsoleScrollRegionIsHonoured(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	// Region rows 2..4, index from the bottom of the REGION: rows 2..4 scroll
	// and row 1 — outside the region — must be left alone.
	got := scrollRegionCase(t, false)
	if got != "R0" {
		t.Errorf("row 0 after IND at the bottom of region 2..4 = %q, want %q "+
			"(the region must scroll, not the screen)", got, "R0")
	}
}

// TestConsoleHonoursScrollRegionReset gates ESC[r. buildRepaint emits it right
// after the screen selection to clear any margins a torn-off app left behind;
// if the reset did not take, painting row-by-row would scroll inside a stale
// region. Read together with TestConsoleScrollRegionIsHonoured.
func TestConsoleHonoursScrollRegionReset(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	// Same region, then ESC[r, then index from the bottom of the SCREEN: with
	// the margins gone the whole screen scrolls, so row 0 becomes old row 1.
	got := scrollRegionCase(t, true)
	if got != "R1" {
		t.Errorf("row 0 after ESC[r then IND at the bottom of the screen = %q, want %q "+
			"(the reset must restore full-screen scrolling)", got, "R1")
	}
}

// scrollRegionCase paints R0..R5 down a 6-row screen, sets a scroll region of
// rows 2..4, optionally resets it with ESC[r, then issues ESC D (IND) from the
// bottom-most row the active region allows. What ends up in row 0 says whether
// the reset took.
func scrollRegionCase(t *testing.T, reset bool) string {
	t.Helper()
	h := newConsoleScreen(t, 10, 6)

	var b strings.Builder
	b.WriteString("\x1b[2J")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "\x1b[%d;1HR%d", i+1, i)
	}
	b.WriteString("\x1b[2;4r")
	if reset {
		b.WriteString("\x1b[r")
		b.WriteString("\x1b[6;1H") // bottom of the full screen
	} else {
		b.WriteString("\x1b[4;1H") // bottom of the region
	}
	b.WriteString("\x1bD") // IND
	conWrite(t, h, []byte(b.String()))
	return conRow(t, h, 0, 10)
}

// TestConsoleHonoursEraseInLineAfterAbsoluteCUP gates the ESC[K that
// buildRepaint issues before painting each row. Erasing BEFORE the paint (not
// after) is what keeps a full-width TUI row intact, so the erase has to mean
// "to the end of the line" from an absolutely-addressed column.
func TestConsoleHonoursEraseInLineAfterAbsoluteCUP(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	h := newConsoleScreen(t, 20, 3)
	// Autowrap off, exactly as buildRepaint leaves it while painting.
	conWrite(t, h, []byte("\x1b[?7l\x1b[2J\x1b[1;1H"+strings.Repeat("X", 20)))
	if got, want := conRow(t, h, 0, 20), strings.Repeat("X", 20); got != want {
		t.Fatalf("row filled to width = %q, want %q", got, want)
	}
	conWrite(t, h, []byte("\x1b[1;10H\x1b[K"))
	if got, want := conRow(t, h, 0, 20), strings.Repeat("X", 9); got != want {
		t.Errorf("row after CUP 1;10 + ESC[K = %q, want %q", got, want)
	}
}

// TestConsoleDECTCEMDoesNotReachAlternateBuffer pins the conhost quirk that
// decides where DECTCEM sits in buildRepaint.
//
// On a real Windows console, ESC[?25h / ESC[?25l issued while the ALTERNATE
// buffer is active does not change the alternate buffer's cursor — it changes
// the MAIN buffer's, which only becomes visible after ESC[?1049l. Both
// directions are asserted, because one alone is equally consistent with "the
// write was simply dropped".
//
// This is the justification for emitting DECTCEM before the screen selection
// rather than beside the final cursor positioning, where it would read more
// naturally. If Microsoft ever fixes conhost, this test fails loudly and the
// ordering can be revisited — which is the correct outcome, and much better
// than the ordering surviving as folklore.
func TestConsoleDECTCEMDoesNotReachAlternateBuffer(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	t.Run("hide-issued-on-alt-lands-on-main", func(t *testing.T) {
		h := newConsoleScreen(t, 20, 4)
		conWrite(t, h, []byte("\x1b[?25h"))
		if !conCursorVisible(t, h) {
			t.Fatal("main buffer cursor should start visible after ESC[?25h")
		}
		conWrite(t, h, []byte("\x1b[?1049h"))
		conWrite(t, h, []byte("\x1b[?25l"))
		if !conCursorVisible(t, h) {
			t.Error("ESC[?25l reached the alternate buffer; conhost behaviour changed " +
				"and the DECTCEM ordering in buildRepaint can be revisited")
		}
		conWrite(t, h, []byte("\x1b[?1049l"))
		if conCursorVisible(t, h) {
			t.Error("main buffer cursor should be hidden: the ESC[?25l issued on the " +
				"alternate buffer is expected to have landed here")
		}
	})

	t.Run("show-issued-on-alt-lands-on-main", func(t *testing.T) {
		h := newConsoleScreen(t, 20, 4)
		conWrite(t, h, []byte("\x1b[?25l"))
		if conCursorVisible(t, h) {
			t.Fatal("main buffer cursor should start hidden after ESC[?25l")
		}
		conWrite(t, h, []byte("\x1b[?1049h"))
		conWrite(t, h, []byte("\x1b[?25h"))
		if conCursorVisible(t, h) {
			t.Error("ESC[?25h reached the alternate buffer; conhost behaviour changed")
		}
		conWrite(t, h, []byte("\x1b[?1049l"))
		if !conCursorVisible(t, h) {
			t.Error("main buffer cursor should be visible: the ESC[?25h issued on the " +
				"alternate buffer is expected to have landed here")
		}
	})
}

// TestConsoleDECTCEMSurvivesEnteringAlternateBuffer gates the property
// buildRepaint's ordering actually relies on: visibility set BEFORE
// ESC[?1049h does carry across the switch. Without this, moving DECTCEM ahead
// of the screen selection would fix nothing.
func TestConsoleDECTCEMSurvivesEnteringAlternateBuffer(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	h := newConsoleScreen(t, 20, 4)
	conWrite(t, h, []byte("\x1b[?25l\x1b[?1049h"))
	if conCursorVisible(t, h) {
		t.Error("cursor visible on the alternate buffer: ESC[?25l issued before " +
			"ESC[?1049h did not survive the switch, so buildRepaint's ordering " +
			"no longer buys anything on Windows")
	}
}

// TestConsoleDECTCEMCannotReachAnAlreadyActiveAlternateBuffer records the case
// the ordering does NOT fix, so it is a known and named limitation rather than
// a surprise in the corpus results.
//
// When the observer is ALREADY on the alternate buffer, the repaint's
// ESC[?1049h is a no-op, there is no switch for the visibility to ride across,
// and the alternate buffer keeps the visible cursor it was created with. This
// is exactly the poisoned observer state. Leaving and re-entering would fix it
// (asserted below) at the cost of a flash of the main buffer, which is why
// buildRepaint does not currently do it.
func TestConsoleDECTCEMCannotReachAnAlreadyActiveAlternateBuffer(t *testing.T) {
	requireConsole(t)
	defer restoreOutputCP(t)()

	t.Run("no-op-switch-cannot-carry-it", func(t *testing.T) {
		h := newConsoleScreen(t, 20, 4)
		conWrite(t, h, []byte("\x1b[?1049h"))          // the observer is already here
		conWrite(t, h, []byte("\x1b[?25l\x1b[?1049h")) // what buildRepaint emits
		if !conCursorVisible(t, h) {
			t.Error("the cursor was hidden on an already-active alternate buffer; " +
				"conhost behaviour changed and the poisoned-state exception in " +
				"TestConsoleRepaintReconstructsScreen can be dropped")
		}
	})

	t.Run("leaving-and-re-entering-would-fix-it", func(t *testing.T) {
		h := newConsoleScreen(t, 20, 4)
		conWrite(t, h, []byte("\x1b[?1049h"))
		conWrite(t, h, []byte("\x1b[?1049l\x1b[?25l\x1b[?1049h"))
		if conCursorVisible(t, h) {
			t.Error("forcing a real switch did not carry the hidden cursor either; " +
				"there may be no way to hide the alt-screen cursor on this console")
		}
	})
}

// ---- console plumbing ---------------------------------------------------
//
// Bound by hand rather than via golang.org/x/sys/windows, which exports
// neither ReadConsoleOutputW nor SetConsoleWindowInfo. Keeping it to syscall +
// LazyDLL adds no dependency to the module.

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateConsoleScreenBuffer  = kernel32.NewProc("CreateConsoleScreenBuffer")
	procSetConsoleScreenBufferSize = kernel32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleWindowInfo       = kernel32.NewProc("SetConsoleWindowInfo")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procReadConsoleOutputW         = kernel32.NewProc("ReadConsoleOutputW")
	procGetConsoleCursorInfo       = kernel32.NewProc("GetConsoleCursorInfo")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procGetConsoleOutputCP         = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
)

const (
	conGenericRead  = 0x80000000
	conGenericWrite = 0x40000000
	conShareRead    = 0x00000001
	conShareWrite   = 0x00000002
	conTextmodeBuf  = 0x00000001

	conEnableProcessedOutput           = 0x0001
	conEnableWrapAtEOLOutput           = 0x0002
	conEnableVirtualTerminalProcessing = 0x0004

	// The CHAR_INFO attribute bit marking the second cell of a double-width
	// glyph.
	conTrailingByte = 0x0200

	conUTF8 = 65001
)

type conCoord struct{ X, Y int16 }

type conSmallRect struct{ Left, Top, Right, Bottom int16 }

type conScreenBufferInfo struct {
	Size              conCoord
	CursorPosition    conCoord
	Attributes        uint16
	Window            conSmallRect
	MaximumWindowSize conCoord
}

type conCharInfo struct {
	Char uint16 // the UnicodeChar arm of the CHAR_INFO union
	Attr uint16
}

type conCursorInfoStruct struct {
	Size    uint32
	Visible int32 // BOOL
}

func conPackCoord(c conCoord) uintptr {
	return uintptr(uint32(uint16(c.X)) | uint32(uint16(c.Y))<<16)
}

// requireConsole skips the suite when the process has no console, so
// `go test ./vtgrid/` still works headless on Windows (CI, a service, an ssh
// session without a pty).
//
// The probe opens CONOUT$ and calls GetConsoleMode on it. It deliberately does
// NOT probe the standard output handle: `go test` pipes the test binary's
// stdout, so GetConsoleMode on it fails even when a perfectly good console is
// attached, and the whole suite would silently skip everywhere.
func requireConsole(t *testing.T) {
	t.Helper()
	h, err := conOpenConout()
	if err != nil {
		t.Skipf("no console available (%v); this leg needs a real conhost", err)
	}
	defer syscall.CloseHandle(h)
	var mode uint32
	if r, _, e := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		t.Skipf("no console available (GetConsoleMode: %v); this leg needs a real conhost", e)
	}
}

func conOpenConout() (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(name, conGenericRead|conGenericWrite,
		conShareRead|conShareWrite, nil, syscall.OPEN_EXISTING, 0, 0)
}

// restoreOutputCP switches the console to UTF-8 so the raw corpus bytes are
// decoded as written, and returns a func that puts the previous code page
// back. The code page is console-wide, so leaving it changed would affect
// whatever the developer runs next in the same window.
func restoreOutputCP(t *testing.T) func() {
	t.Helper()
	prev, _, _ := procGetConsoleOutputCP.Call()
	if r, _, e := procSetConsoleOutputCP.Call(conUTF8); r == 0 {
		t.Fatalf("SetConsoleOutputCP(65001): %v", e)
	}
	return func() {
		if prev != 0 {
			procSetConsoleOutputCP.Call(prev)
		}
	}
}

// newConsoleScreen makes a private console screen buffer that is exactly
// cols x rows with no scrollback, VT processing on, and autowrap on — the state
// a real terminal starts in.
//
// The buffer is never made the active screen buffer, so nothing here reaches
// the terminal the test was launched from. The window is shrunk before the
// buffer is resized because a screen buffer may never be smaller than its
// window, and the achieved size is verified: the corpora are 150x40 and
// 173x36, and comparing rows against a differently-sized screen would be
// meaningless rather than merely wrong.
func newConsoleScreen(t *testing.T, cols, rows int) syscall.Handle {
	t.Helper()
	r, _, e := procCreateConsoleScreenBuffer.Call(
		uintptr(conGenericRead|conGenericWrite),
		uintptr(conShareRead|conShareWrite),
		0, uintptr(conTextmodeBuf), 0,
	)
	h := syscall.Handle(r)
	if h == syscall.InvalidHandle {
		t.Fatalf("CreateConsoleScreenBuffer: %v", e)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })

	conSetWindow(t, h, conSmallRect{0, 0, 0, 0})
	if r, _, e := procSetConsoleScreenBufferSize.Call(uintptr(h), conPackCoord(conCoord{int16(cols), int16(rows)})); r == 0 {
		t.Fatalf("SetConsoleScreenBufferSize(%d,%d): %v", cols, rows, e)
	}
	conSetWindow(t, h, conSmallRect{0, 0, int16(cols - 1), int16(rows - 1)})

	info := conScreenInfo(t, h)
	gotW := int(info.Window.Right-info.Window.Left) + 1
	gotH := int(info.Window.Bottom-info.Window.Top) + 1
	if int(info.Size.X) != cols || int(info.Size.Y) != rows || gotW != cols || gotH != rows {
		t.Fatalf("console refused the size: buffer=%dx%d window=%dx%d, want %dx%d",
			info.Size.X, info.Size.Y, gotW, gotH, cols, rows)
	}
	mode := uintptr(conEnableProcessedOutput | conEnableWrapAtEOLOutput | conEnableVirtualTerminalProcessing)
	if r, _, e := procSetConsoleMode.Call(uintptr(h), mode); r == 0 {
		t.Fatalf("SetConsoleMode(%#x): %v", mode, e)
	}
	return h
}

func conSetWindow(t *testing.T, h syscall.Handle, rect conSmallRect) {
	t.Helper()
	if r, _, e := procSetConsoleWindowInfo.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&rect))); r == 0 {
		t.Fatalf("SetConsoleWindowInfo(%d,%d,%d,%d): %v", rect.Left, rect.Top, rect.Right, rect.Bottom, e)
	}
}

// conWrite pushes raw bytes at the console. Chunks never split a UTF-8
// sequence: a ring tail is cut at an arbitrary offset, and conhost's UTF-8
// decoder does not carry a partial sequence across calls. Splitting an escape
// sequence is fine — the VT parser is stateful across writes.
func conWrite(t *testing.T, h syscall.Handle, b []byte) {
	t.Helper()
	const chunk = 4096
	for len(b) > 0 {
		n := len(b)
		if n > chunk {
			n = chunk
			for n > 0 && b[n]&0xC0 == 0x80 {
				n--
			}
			if n == 0 {
				n = chunk
			}
		}
		w, err := syscall.Write(h, b[:n])
		if err != nil {
			t.Fatalf("console write: %v", err)
		}
		if w <= 0 {
			t.Fatalf("short console write")
		}
		b = b[w:]
	}
}

func conScreenInfo(t *testing.T, h syscall.Handle) conScreenBufferInfo {
	t.Helper()
	var info conScreenBufferInfo
	if r, _, e := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info))); r == 0 {
		t.Fatalf("GetConsoleScreenBufferInfo: %v", e)
	}
	return info
}

func conCursorVisible(t *testing.T, h syscall.Handle) bool {
	t.Helper()
	var ci conCursorInfoStruct
	if r, _, e := procGetConsoleCursorInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&ci))); r == 0 {
		t.Fatalf("GetConsoleCursorInfo: %v", e)
	}
	return ci.Visible != 0
}

func conReadScreen(t *testing.T, h syscall.Handle, cols, rows int) []string {
	t.Helper()
	out := make([]string, rows)
	for y := 0; y < rows; y++ {
		out[y] = conRow(t, h, y, cols)
	}
	return out
}

// conRow reads one row out of the console's own screen buffer and renders the
// text it shows.
//
// Row at a time rather than the whole rectangle: ReadConsoleOutputW takes an
// internal shared-heap path that fails on large regions, and 40 calls cost
// nothing.
//
// A double-width glyph occupies two cells. conhost marks them LEADING and
// TRAILING and, for a BMP glyph, repeats the same UTF-16 unit in both, so the
// trailing cell is dropped. A non-BMP glyph is a surrogate pair split across
// the two cells instead, so a trailing cell holding a LOW SURROGATE is kept —
// dropping it would corrupt the character.
//
// Trailing ASCII SPACE only is trimmed, matching trimRows. Not unicode
// whitespace: Claude Code emits U+00A0 after its prompt glyph, and a trimmer
// that eats it deletes real screen content while reporting a match.
func conRow(t *testing.T, h syscall.Handle, y, cols int) string {
	t.Helper()
	buf := make([]conCharInfo, cols)
	region := conSmallRect{Left: 0, Top: int16(y), Right: int16(cols - 1), Bottom: int16(y)}
	r, _, e := procReadConsoleOutputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])),
		conPackCoord(conCoord{int16(cols), 1}),
		conPackCoord(conCoord{0, 0}),
		uintptr(unsafe.Pointer(&region)),
	)
	if r == 0 {
		t.Fatalf("ReadConsoleOutputW(row %d): %v", y, e)
	}

	units := make([]uint16, 0, cols)
	for _, c := range buf {
		ch := c.Char
		if ch == 0 {
			ch = ' '
		}
		isLowSurrogate := ch >= 0xDC00 && ch <= 0xDFFF
		if c.Attr&conTrailingByte != 0 && !isLowSurrogate {
			continue
		}
		units = append(units, ch)
	}
	return strings.TrimRight(string(utf16.Decode(units)), " ")
}
