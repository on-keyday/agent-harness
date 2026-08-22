//go:build windows

// Does preamble() actually land the cursor bit on a REAL Windows console?
//
// mode_tracker_test.go asserts the bytes preamble() emits. That is necessary
// and not sufficient: the defect this file exists for was an ORDERING one, and
// a byte-level test written against the buggy order passes just as happily as
// one written against the correct order. Only a terminal can say which order
// works, and only conhost has the behaviour that makes the order matter —
// DECTCEM issued while the alternate buffer is already active never reaches it.
//
// So this feeds preamble()'s output to a console screen buffer and reads the
// cursor state back with GetConsoleCursorInfo. The observer is put in the state
// a real reattaching Windows client is in: already on the alternate buffer.
//
// The console plumbing below is deliberately self-contained, mirroring
// vtgrid/repaint_console_windows_test.go rather than sharing with it — see the
// note above the plumbing section.

package server

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestPreambleLandsCursorVisibilityOnAConsole is the end-to-end assertion: an
// app's tracked cursor state, replayed through preamble() into a real console
// whose observer is already on the alternate buffer, must produce the cursor
// the app asked for.
//
// Both directions are covered because the conhost behaviour is symmetric: the
// alternate buffer inherits visibility at the moment of ESC[?1049h and is
// immutable from inside, so getting it wrong loses a caret exactly as easily as
// it strands a stray block.
func TestPreambleLandsCursorVisibilityOnAConsole(t *testing.T) {
	wcRequireConsole(t)
	defer wcUseUTF8(t)()

	cases := []struct {
		name string
		// app is what the application wrote, in order. The tracker sees this.
		app string
		// observer is the state the reattaching client's terminal is already
		// in when the preamble arrives.
		observer string
		wantVis  bool
	}{
		{
			// htop: enters the alternate screen, then hides its cursor. On
			// conhost the hide never reached the alternate buffer, so before
			// the fix a reattach left a stray block on the UI.
			name:     "app hid its cursor inside the alt screen",
			app:      "\x1b[?1049h\x1b[?25l",
			observer: "\x1b[?25h\x1b[?1049h",
			wantVis:  false,
		},
		{
			// opencode: was on a main screen with the cursor already hidden
			// when it entered the alternate screen, then showed its caret.
			// The mirror image, and the one that loses a caret.
			name:     "app showed its caret inside the alt screen",
			app:      "\x1b[?25l\x1b[?1049h\x1b[?25h",
			observer: "\x1b[?25l\x1b[?1049h",
			wantVis:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newModeTracker()
			tr.feed([]byte(c.app))
			pre := tr.preamble()
			if len(pre) == 0 {
				t.Fatal("preamble is empty; nothing to replay")
			}

			h := wcNewScreen(t, 40, 10)
			wcWrite(t, h, []byte(c.observer))
			wcWrite(t, h, pre)

			if got := wcCursorVisible(t, h); got != c.wantVis {
				t.Errorf("cursor visible = %v, want %v\npreamble = %q",
					got, c.wantVis, pre)
			}
		})
	}
}

// TestPreambleOrderingIsWhatMakesItLand is the CONTROL, and it is the reason
// the test above can be trusted.
//
// It replays the SAME modes in the pre-fix order — the screen switch first,
// DECTCEM after — and asserts that the cursor bit does NOT land. Without this,
// a console that simply honoured DECTCEM everywhere would pass the test above
// no matter what order preamble() chose, and the ordering it is meant to
// protect could be reverted without anything going red.
func TestPreambleOrderingIsWhatMakesItLand(t *testing.T) {
	wcRequireConsole(t)
	defer wcUseUTF8(t)()

	h := wcNewScreen(t, 40, 10)
	wcWrite(t, h, []byte("\x1b[?25h\x1b[?1049h")) // observer already on alt, cursor visible
	// The old order: enter (a no-op here), then hide.
	wcWrite(t, h, []byte("\x1b[?1049h\x1b[?25l"))
	if !wcCursorVisible(t, h) {
		t.Error("the pre-fix ordering hid the cursor on an already-active alternate " +
			"buffer, so this control no longer proves anything and " +
			"TestPreambleLandsCursorVisibilityOnAConsole would pass regardless of " +
			"the order preamble() emits — conhost behaviour has changed")
	}
}

// ---- console plumbing ---------------------------------------------------
//
// A near-copy of the plumbing in vtgrid/repaint_console_windows_test.go. It is
// duplicated rather than shared because the two live in different packages and
// the helpers are unexported test code; a shared home would be an internal
// package, which is a layout decision worth making deliberately rather than as
// a side effect of adding a test.
//
// Duplicating THIS is safe in a way that duplicating the sequence under test
// would not be: it is a Win32 binding with no behaviour of its own, and if a
// copy drifts the tests fail loudly rather than quietly gating a fossil.
//
// Bound by hand because golang.org/x/sys/windows exports neither
// SetConsoleWindowInfo nor the console cursor calls in the shape needed here.

var (
	wcKernel32 = syscall.NewLazyDLL("kernel32.dll")

	wcCreateScreenBuffer = wcKernel32.NewProc("CreateConsoleScreenBuffer")
	wcSetBufferSize      = wcKernel32.NewProc("SetConsoleScreenBufferSize")
	wcSetWindowInfo      = wcKernel32.NewProc("SetConsoleWindowInfo")
	wcGetCursorInfo      = wcKernel32.NewProc("GetConsoleCursorInfo")
	wcSetMode            = wcKernel32.NewProc("SetConsoleMode")
	wcGetMode            = wcKernel32.NewProc("GetConsoleMode")
	wcGetOutputCP        = wcKernel32.NewProc("GetConsoleOutputCP")
	wcSetOutputCP        = wcKernel32.NewProc("SetConsoleOutputCP")
)

const (
	wcGenericRead  = 0x80000000
	wcGenericWrite = 0x40000000
	wcShareRead    = 0x00000001
	wcShareWrite   = 0x00000002
	wcTextmodeBuf  = 0x00000001

	wcEnableProcessedOutput           = 0x0001
	wcEnableWrapAtEOLOutput           = 0x0002
	wcEnableVirtualTerminalProcessing = 0x0004

	wcUTF8CodePage = 65001
)

type wcCoord struct{ X, Y int16 }

type wcSmallRect struct{ Left, Top, Right, Bottom int16 }

type wcCursorInfo struct {
	Size    uint32
	Visible int32 // BOOL
}

func wcPack(c wcCoord) uintptr {
	return uintptr(uint32(uint16(c.X)) | uint32(uint16(c.Y))<<16)
}

// wcRequireConsole skips when the process has no console, so `go test ./server/`
// still works headless on Windows.
//
// It probes CONOUT$, NOT the standard output handle: `go test` pipes the test
// binary's stdout, so GetConsoleMode on it fails even with a good console
// attached and the whole leg would silently skip everywhere while reporting
// PASS.
func wcRequireConsole(t *testing.T) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString("CONOUT$")
	if err != nil {
		t.Skipf("no console available (%v)", err)
	}
	h, err := syscall.CreateFile(name, wcGenericRead|wcGenericWrite,
		wcShareRead|wcShareWrite, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Skipf("no console available (%v); this test needs a real conhost", err)
	}
	defer syscall.CloseHandle(h)
	var mode uint32
	if r, _, e := wcGetMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		t.Skipf("no console available (GetConsoleMode: %v); this test needs a real conhost", e)
	}
}

// wcUseUTF8 switches the console to UTF-8 and returns a func restoring the
// previous code page, which is console-wide and would otherwise outlive the run.
func wcUseUTF8(t *testing.T) func() {
	t.Helper()
	prev, _, _ := wcGetOutputCP.Call()
	if r, _, e := wcSetOutputCP.Call(wcUTF8CodePage); r == 0 {
		t.Fatalf("SetConsoleOutputCP(65001): %v", e)
	}
	return func() {
		if prev != 0 {
			wcSetOutputCP.Call(prev)
		}
	}
}

// wcNewScreen makes a PRIVATE console screen buffer, never made active, so the
// test does not disturb the terminal it was launched from.
func wcNewScreen(t *testing.T, cols, rows int) syscall.Handle {
	t.Helper()
	r, _, e := wcCreateScreenBuffer.Call(
		uintptr(wcGenericRead|wcGenericWrite),
		uintptr(wcShareRead|wcShareWrite),
		0, uintptr(wcTextmodeBuf), 0,
	)
	h := syscall.Handle(r)
	if h == syscall.InvalidHandle {
		t.Fatalf("CreateConsoleScreenBuffer: %v", e)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })

	// Shrink the window before resizing the buffer: a buffer may never be
	// smaller than its window.
	small := wcSmallRect{0, 0, 0, 0}
	if r, _, e := wcSetWindowInfo.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&small))); r == 0 {
		t.Fatalf("SetConsoleWindowInfo(shrink): %v", e)
	}
	if r, _, e := wcSetBufferSize.Call(uintptr(h), wcPack(wcCoord{int16(cols), int16(rows)})); r == 0 {
		t.Fatalf("SetConsoleScreenBufferSize(%d,%d): %v", cols, rows, e)
	}
	win := wcSmallRect{0, 0, int16(cols - 1), int16(rows - 1)}
	if r, _, e := wcSetWindowInfo.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&win))); r == 0 {
		t.Fatalf("SetConsoleWindowInfo(%dx%d): %v", cols, rows, e)
	}
	mode := uintptr(wcEnableProcessedOutput | wcEnableWrapAtEOLOutput | wcEnableVirtualTerminalProcessing)
	if r, _, e := wcSetMode.Call(uintptr(h), mode); r == 0 {
		t.Fatalf("SetConsoleMode(%#x): %v", mode, e)
	}
	return h
}

func wcWrite(t *testing.T, h syscall.Handle, b []byte) {
	t.Helper()
	for len(b) > 0 {
		n, err := syscall.Write(h, b)
		if err != nil {
			t.Fatalf("console write: %v", err)
		}
		if n <= 0 {
			t.Fatal("short console write")
		}
		b = b[n:]
	}
}

func wcCursorVisible(t *testing.T, h syscall.Handle) bool {
	t.Helper()
	var ci wcCursorInfo
	if r, _, e := wcGetCursorInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&ci))); r == 0 {
		t.Fatalf("GetConsoleCursorInfo: %v", e)
	}
	return ci.Visible != 0
}
