package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestHostLogModalScrollAndKeys(t *testing.T) {
	m := NewHostLogModal()
	m.SetSize(80, 18) // bodyHeight = 8
	m.OpenLoading("server", "", cli.HostLogOpts{})
	if !m.IsOpen() {
		t.Fatal("expected modal open after OpenLoading")
	}
	if !strings.Contains(m.View(), "Fetching process log") {
		t.Fatalf("loading view missing prompt: %q", m.View())
	}

	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, "line "+string(rune('A'+i-1)))
	}
	raw := strings.Join(lines, "\n") + "\n"
	m.SetResult(HostLogResultMsg{
		Target: "server",
		Opts:   cli.HostLogOpts{},
		Snap: cli.HostLogSnapshot{
			Source:      protocol.HostLogSource_Ring,
			TotalSize:   uint64(len(raw)),
			StartOffset: 0,
			Data:        []byte(raw),
		},
	})
	// Tail read starts scrolled to bottom.
	if m.scroll == 0 {
		t.Fatal("expected tail read to start scrolled to bottom")
	}
	if !strings.Contains(m.View(), "line T") {
		t.Fatalf("tail view should show newest line: %q", m.View())
	}

	m.ScrollTop()
	if m.scroll != 0 || !strings.Contains(m.View(), "line A") {
		t.Fatalf("ScrollTop failed: scroll=%d view=%q", m.scroll, m.View())
	}
	m.ScrollBy(3)
	if m.scroll != 3 {
		t.Fatalf("ScrollBy(3) = %d, want 3", m.scroll)
	}
	m.ScrollBottom()
	if m.scroll != m.maxScroll() {
		t.Fatalf("ScrollBottom = %d, want %d", m.scroll, m.maxScroll())
	}

	// Range read starts at top.
	m.SetResult(HostLogResultMsg{
		Target: "server",
		Opts:   cli.HostLogOpts{HasOffset: true, Offset: 0},
		Snap: cli.HostLogSnapshot{
			Source:      protocol.HostLogSource_File,
			TotalSize:   uint64(len(raw)),
			StartOffset: 0,
			Data:        []byte(raw),
		},
	})
	if m.scroll != 0 {
		t.Fatalf("range read scroll = %d, want 0", m.scroll)
	}

	// Error display.
	m.SetResult(HostLogResultMsg{Target: "server", Err: errors.New("permission denied")})
	if !strings.Contains(m.View(), "permission denied") {
		t.Fatalf("error view missing message: %q", m.View())
	}

	// App overlay key dispatch closes on Esc.
	a := &App{hostLogModal: m}
	a.inHostLogModal(tea.KeyMsg{Type: tea.KeyEsc})
	if a.hostLogModal.IsOpen() {
		t.Fatal("Esc should close hostLogModal")
	}
}
