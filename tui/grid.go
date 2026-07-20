package tui

import (
	"context"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const gridMaxPanes = 9

type gridTickMsg struct{}

func gridTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return gridTickMsg{} })
}

// GridModel is a full-screen bubbletea overlay tiling live PaneStreamers, one
// per interactive session (the connsModal/boardModal template — a normal
// overlay whose Update/View are dispatched from App, NOT a tea.Exec suspend;
// tea.Exec would freeze the Update loop and only one live pane could ever be
// shown at a time).
type GridModel struct {
	open    bool
	width   int
	height  int
	panes   []*PaneStreamer
	focus   int
	cols    int // computed pane columns for the current size
	ctx     context.Context
	client  *cli.Client
	program *tea.Program
}

func NewGridModel() GridModel { return GridModel{} }

func (m GridModel) IsOpen() bool { return m.open }

func (m *GridModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.cols = gridCols(len(m.panes))
}

// gridCols picks a column count so panes stay roughly square-ish: 1 for ≤1,
// 2 for ≤4, 3 for ≤9.
func gridCols(n int) int {
	switch {
	case n <= 1:
		return 1
	case n <= 4:
		return 2
	default:
		return 3
	}
}

// Open builds panes for the live (Running/Detached) interactive tasks in
// tasks, capped at gridMaxPanes and ordered most-recently-active first, and
// starts each pane streaming read-only over the shared client c. It never
// dials a fresh connection and never sends a PTY size — the grid has no size
// authority (Global Constraint); each PaneStreamer sizes its own emulator to
// whatever the server replays.
func (m *GridModel) Open(ctx context.Context, c *cli.Client, program *tea.Program, tasks []protocol.TaskInfo) {
	live := make([]protocol.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		if t.Kind == protocol.TaskKind_Interactive &&
			(t.Status == protocol.TaskStatus_Running || t.Status == protocol.TaskStatus_Detached) {
			live = append(live, t)
		}
	}
	// Activity-desc: most recently active session first. TaskInfo carries no
	// single "last activity" timestamp; LastOutputAt (the same field the task
	// list uses for its idle/busy badge, see refreshTasksTable) is the closest
	// analog — higher (more recent) first, with never-yet-active (0) sessions
	// sinking to the end.
	sort.Slice(live, func(i, j int) bool {
		return live[i].LastOutputAt > live[j].LastOutputAt
	})
	if len(live) > gridMaxPanes {
		live = live[:gridMaxPanes]
	}
	m.panes = m.panes[:0]
	for _, t := range live {
		p := NewPaneStreamer(FormatTaskID(t.Id), 24, 80)
		p.Start(ctx, c)
		m.panes = append(m.panes, p)
	}
	m.open = true
	m.focus = 0
	m.ctx, m.client, m.program = ctx, c, program
	m.cols = gridCols(len(m.panes))
}

// Close stops every pane (idempotent — safe even if some panes already
// errored) and closes the overlay.
func (m *GridModel) Close() {
	for _, p := range m.panes {
		p.Stop()
	}
	m.panes = nil
	m.open = false
	m.focus = 0
}

func (m GridModel) FocusedTaskID() string {
	if m.focus < 0 || m.focus >= len(m.panes) {
		return ""
	}
	return m.panes[m.focus].TaskID()
}

func (m GridModel) Update(msg tea.Msg) (GridModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.Close()
			return m, nil
		case "enter":
			if id := m.FocusedTaskID(); id != "" && m.client != nil {
				cmd := DoAttachSession(m.client, id, protocol.AttachMode_View)
				m.Close()
				return m, cmd
			}
			return m, nil
		case "x":
			m.dismissFocused()
			return m, nil
		case "left", "h":
			m.moveFocus(-1)
		case "right", "l":
			m.moveFocus(1)
		case "up", "k":
			m.moveFocus(-m.cols)
		case "down", "j":
			m.moveFocus(m.cols)
		}
	case gridTickMsg:
		if !m.open {
			return m, nil
		}
		return m, gridTick()
	}
	return m, nil
}

func (m *GridModel) moveFocus(delta int) {
	if len(m.panes) == 0 {
		return
	}
	f := m.focus + delta
	if f < 0 {
		f = 0
	}
	if f >= len(m.panes) {
		f = len(m.panes) - 1
	}
	m.focus = f
}

func (m *GridModel) dismissFocused() {
	if m.focus < 0 || m.focus >= len(m.panes) {
		return
	}
	m.panes[m.focus].Stop()
	m.panes = append(m.panes[:m.focus], m.panes[m.focus+1:]...)
	if m.focus >= len(m.panes) {
		m.focus = len(m.panes) - 1
	}
	if m.focus < 0 {
		m.focus = 0
	}
	m.cols = gridCols(len(m.panes))
}

func (m GridModel) View() string {
	if len(m.panes) == 0 {
		return PanelStyleFocused.Padding(0, 1).Render("grid: no live interactive sessions (esc to close)")
	}
	cols := m.cols
	rows := (len(m.panes) + cols - 1) / cols
	// Cell interior size, minus borders (2) and header line (1).
	cellW := m.width/cols - 2
	cellH := m.height/rows - 3
	if cellW < 8 {
		cellW = 8
	}
	if cellH < 2 {
		cellH = 2
	}
	var rowsOut []string
	for r := 0; r < rows; r++ {
		var cells []string
		for c := 0; c < cols; c++ {
			idx := r*cols + c
			if idx >= len(m.panes) {
				break
			}
			cells = append(cells, m.renderPane(idx, cellW, cellH))
		}
		rowsOut = append(rowsOut, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rowsOut...)
}

func (m GridModel) renderPane(idx, w, h int) string {
	p := m.panes[idx]
	head := p.TaskID()
	if len(head) > 8 {
		head = head[:8]
	}
	if err := p.Err(); err != nil {
		head += " (ended)"
	}
	body := p.Render(w, h)
	style := PanelStyle
	if idx == m.focus {
		style = PanelStyleFocused
	}
	return style.Width(w).Height(h + 1).Render(head + "\n" + body)
}
