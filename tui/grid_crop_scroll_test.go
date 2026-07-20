package tui
import ("strings";"testing";"github.com/charmbracelet/x/vt")
// A full screen that SCROLLED (recent content at the bottom) but whose cursor
// was moved UP (common in full-screen TUIs / claude) must still show the recent
// BOTTOM content, not the old top. Cursor-only anchoring regresses this.
func TestPaneStreamer_ScrolledCursorMovedUp(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(80, 40), cols: 80, rows: 40}
	defer p.emu.Close()
	var lines []string
	for i := 1; i <= 100; i++ { lines = append(lines, "LINE_"+itoa(i)) }
	p.emu.Write([]byte(strings.Join(lines, "\r\n")))
	p.emu.Write([]byte("\x1b[5;1H")) // move cursor to row 5 (near top) after scroll
	out := p.Render(80, 10)          // pane shows 10 rows; should be the RECENT ones
	if !strings.Contains(out, "LINE_100") {
		t.Fatalf("must show recent bottom content (LINE_100); cursor-anchor hides it.\ngot:\n%s", out)
	}
}
func itoa(n int) string {
	if n == 0 { return "0" }
	var b []byte
	for n > 0 { b = append([]byte{byte('0'+n%10)}, b...); n/=10 }
	return string(b)
}
