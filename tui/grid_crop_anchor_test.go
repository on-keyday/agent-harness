package tui
import ("strings";"testing";"github.com/charmbracelet/x/vt")
func TestPaneStreamer_CropAnchorsToContent(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(80, 40), cols: 80, rows: 40}
	defer p.emu.Close()
	p.emu.Write([]byte("line A\r\nline B\r\nprompt$ "))
	out := p.Render(80, 10)
	if !strings.Contains(out, "line A") || !strings.Contains(out, "prompt$") {
		t.Fatalf("crop of a tall screen with content only at top must SHOW that content, not the empty bottom.\ngot %q", out)
	}
}
