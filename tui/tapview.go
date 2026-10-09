package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// tapMaxLines bounds what the view keeps. A tap on a busy forward or exec
// produces lines faster than anyone reads them, and an unbounded slice would
// grow until the TUI is the memory problem.
const tapMaxLines = 5000

// tapSubject names what a tap view is reading: a forward or an exec, by id.
type tapSubject struct {
	Kind string // "forward" | "exec"
	ID   uint64
}

func (s tapSubject) String() string { return fmt.Sprintf("%s #%d", s.Kind, s.ID) }

// TapView is a full-screen overlay showing one forward's or one exec's live
// traffic.
//
// It is a view of its own rather than an append into the cmdline result region,
// and that is the lesson the exec listing paid for: the cmdline region is FIVE
// lines and scrolls to the bottom on every write, so anything longer hides its
// own header. A tap emits several lines per record.
//
// The header is drawn OUTSIDE the viewport, so it cannot scroll away no matter
// how much traffic arrives.
type TapView struct {
	open    bool
	subject tapSubject
	vp      viewport.Model
	lines   []string
	// follow keeps the viewport pinned to the newest line. An operator who
	// scrolls up is reading something, so scrolling releases it; `G` takes it
	// back. Without this, a busy forward makes reading scrollback impossible.
	follow bool
	width  int
	height int
}

func NewTapView(subject tapSubject) TapView {
	return TapView{subject: subject, vp: viewport.New(0, 0), follow: true}
}

func (m *TapView) IsOpen() bool        { return m.open }
func (m *TapView) Open()               { m.open = true }
func (m *TapView) Close()              { m.open = false }
func (m *TapView) Subject() tapSubject { return m.subject }
func (m *TapView) Following() bool     { return m.follow }
func (m *TapView) SetFollow(on bool)   { m.follow = on }
func (m *TapView) LineCount() int      { return len(m.lines) }

// SetSize derives the frame from the TERMINAL, never from the content: a frame
// that grows with what arrives would resize itself on every record.
func (m *TapView) SetSize(w, h int) {
	m.width, m.height = w, h
	// border + header + footer, matching the other full-screen overlays.
	m.vp.Width = w - 4
	m.vp.Height = h - 4
	if m.vp.Height < 1 {
		m.vp.Height = 1
	}
	m.refresh()
}

// Height reports the viewport height this view gets for a terminal of h rows.
// Exported so a test can assert it is a real viewport rather than a strip.
func (m *TapView) Height(h int) int {
	return h - 4
}

// Append adds rendered lines, trimming the oldest when the cap is reached.
func (m *TapView) Append(lines []string) {
	if len(lines) == 0 {
		return
	}
	m.lines = append(m.lines, lines...)
	if len(m.lines) > tapMaxLines {
		m.lines = m.lines[len(m.lines)-tapMaxLines:]
	}
	m.refresh()
}

func (m *TapView) refresh() {
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	if m.follow {
		m.vp.GotoBottom()
	}
}

// Update forwards navigation keys to the viewport. Any manual movement
// releases follow; `G` re-arms it.
func (m *TapView) Update(msg tea.Msg) tea.Cmd {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "G":
			m.follow = true
			m.vp.GotoBottom()
			return nil
		case "j", "k", "up", "down", "pgup", "pgdown", "g", "home", "end":
			m.follow = false
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *TapView) View() string {
	if !m.open {
		return ""
	}
	state := "following"
	if !m.follow {
		state = "scrolled"
	}
	// Header and footer are rendered OUTSIDE the viewport, matching
	// ForwardsModal.View, which is also what keeps the header from scrolling
	// away under traffic.
	header := HeaderStyle.Render(fmt.Sprintf("tap on %s — %d lines (%s)",
		m.subject, len(m.lines), state))
	footer := FooterStyle.Render("j/k: scroll · G: follow · Esc: close")
	box := PanelStyleFocused.Padding(0, 1)
	body := m.vp.View()
	if len(m.lines) == 0 {
		body = "waiting for traffic…"
	}
	return box.Render(header + "\n" + body + "\n" + footer)
}
