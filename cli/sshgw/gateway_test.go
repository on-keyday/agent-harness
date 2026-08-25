//go:build !js

package sshgw

import (
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Only the control seat is scarce. A second control session would take the seat
// server-side from whatever holds it — possibly the operator's own TUI — so the
// gateway refuses it instead of superseding. Cowriters and viewers are what
// those modes exist for and are never counted.
func TestGateway_ControlSeatIsExclusivePerTask(t *testing.T) {
	g := newGateway(nil)
	const a, b = "aaaa", "bbbb"

	if !g.claim(a, protocol.AttachMode_Control) {
		t.Fatal("first control claim on a free task must succeed")
	}
	if g.claim(a, protocol.AttachMode_Control) {
		t.Error("second control claim on the same task must be refused")
	}
	if !g.claim(b, protocol.AttachMode_Control) {
		t.Error("control on a different task must be unaffected")
	}
	if !g.claim(a, protocol.AttachMode_Cowrite) || !g.claim(a, protocol.AttachMode_Cowrite) {
		t.Error("cowriters must never be refused")
	}
	if !g.claim(a, protocol.AttachMode_View) {
		t.Error("viewers must never be refused")
	}

	g.release(a, protocol.AttachMode_Control)
	if !g.claim(a, protocol.AttachMode_Control) {
		t.Error("the seat must be reusable once released")
	}
}

// Releasing a mode that never claims must not free somebody else's seat.
func TestGateway_ReleasingACowriterKeepsTheSeat(t *testing.T) {
	g := newGateway(nil)
	const id = "aaaa"
	if !g.claim(id, protocol.AttachMode_Control) {
		t.Fatal("claim")
	}
	g.release(id, protocol.AttachMode_Cowrite)
	if g.claim(id, protocol.AttachMode_Control) {
		t.Error("a cowriter's release freed the control seat")
	}
}

func TestGateway_ClaimIsRaceFree(t *testing.T) {
	g := newGateway(nil)
	const id = "aaaa"
	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.claim(id, protocol.AttachMode_Control) {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent claims succeeded, want exactly 1", wins)
	}
}

func TestParsePtyReq(t *testing.T) {
	// "xterm-256color", 120 cols, 40 rows, 960x480 px, empty modes.
	term := "xterm-256color"
	payload := make([]byte, 0, 4+len(term)+20)
	payload = appendU32(payload, uint32(len(term)))
	payload = append(payload, term...)
	payload = appendU32(payload, 120)
	payload = appendU32(payload, 40)
	payload = appendU32(payload, 960)
	payload = appendU32(payload, 480)
	payload = appendU32(payload, 0)

	d, err := parsePtyReq(payload)
	if err != nil {
		t.Fatalf("parsePtyReq: %v", err)
	}
	// The whole reason for named fields: SSH sends COLUMNS first, and
	// SetTerminalWindowSize takes ROWS first. A swap renders a plausible but
	// wrong screen, which is the kind of bug that survives a demo.
	if d.Cols != 120 || d.Rows != 40 || d.WidthPx != 960 || d.HeightPx != 480 {
		t.Errorf("got %+v, want {Cols:120 Rows:40 WidthPx:960 HeightPx:480}", d)
	}
}

func TestParsePtyReq_Short(t *testing.T) {
	if _, err := parsePtyReq([]byte{0, 0, 0, 9}); err == nil {
		t.Error("want an error when the TERM length exceeds the payload")
	}
	if _, err := parsePtyReq(nil); err == nil {
		t.Error("want an error for an empty payload")
	}
	// A TERM string present but the dimensions missing.
	short := appendU32(nil, 2)
	short = append(short, "vt"...)
	if _, err := parsePtyReq(short); err == nil {
		t.Error("want an error when the dimensions are absent")
	}
}

func TestParseWindowChange_ZeroIsDecodedNotRejected(t *testing.T) {
	// A zero size decodes fine; it is the CALLER that declines to send it on,
	// the same both-or-nothing rule applyInitialWindowSize follows.
	payload := appendU32(nil, 0)
	payload = appendU32(payload, 0)
	payload = appendU32(payload, 0)
	payload = appendU32(payload, 0)
	d, err := parseWindowChange(payload)
	if err != nil {
		t.Fatalf("parseWindowChange: %v", err)
	}
	if d.Rows != 0 || d.Cols != 0 {
		t.Errorf("got %+v, want zeroes", d)
	}
}

func TestParseWindowChange_Short(t *testing.T) {
	if _, err := parseWindowChange([]byte{0, 0, 0, 1}); err == nil {
		t.Error("want an error for a truncated window-change payload")
	}
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
