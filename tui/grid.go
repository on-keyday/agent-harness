package tui

import (
	"context"
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	gridPerPage  = 6  // panes shown per page (3x2)
	gridMaxPanes = 24 // total cap across all pages (bounds concurrent view streams)
)

type gridTickMsg struct{}

func gridTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return gridTickMsg{} })
}

// GridModel is a full-screen bubbletea overlay tiling live PaneStreamers, one
// per interactive session (the connsModal/boardModal template — a normal
// overlay whose Update/View are dispatched from App, NOT a tea.Exec suspend;
// tea.Exec would freeze the Update loop and only one live pane could ever be
// shown at a time).
//
// Panes form one ordered list; a page shows a gridPerPage-sized window of it.
// [ ] switch pages; Shift+H/J/K/L reorder the focused pane (crossing page
// boundaries), so specific sessions can be grouped onto the same page.
type GridModel struct {
	open   bool
	width  int
	height int
	panes  []*PaneStreamer
	focus  int // global index into panes
	page   int
	client *cli.Client
}

func NewGridModel() GridModel { return GridModel{} }

func (m GridModel) IsOpen() bool { return m.open }

func (m *GridModel) SetSize(w, h int) { m.width, m.height = w, h }

// gridCols picks a column count so panes stay roughly square-ish: 1 for ≤1,
// 2 for ≤4, 3 for ≤9. Applied per page (a page holds ≤gridPerPage panes).
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

func (m GridModel) pageCount() int {
	if len(m.panes) == 0 {
		return 1
	}
	return (len(m.panes) + gridPerPage - 1) / gridPerPage
}

func (m GridModel) pageStart() int { return m.page * gridPerPage }

func (m GridModel) pageEnd() int {
	end := m.pageStart() + gridPerPage
	if end > len(m.panes) {
		end = len(m.panes)
	}
	return end
}

// pageCols is the column count of the current page's tiling.
func (m GridModel) pageCols() int { return gridCols(m.pageEnd() - m.pageStart()) }

// Open builds panes for the live (Running/Detached) interactive tasks in
// tasks, capped at gridMaxPanes and ordered most-recently-active first, and
// starts each pane streaming read-only over the shared client c. It never
// dials a fresh connection and never sends a PTY size — the grid has no size
// authority (Global Constraint); each PaneStreamer sizes its own emulator to
// whatever the server replays.
func (m *GridModel) Open(ctx context.Context, c *cli.Client, tasks []protocol.TaskInfo) {
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
	m.page = 0
	m.client = c
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
	m.page = 0
}

// attachFocused closes the grid and returns a cmd that attaches the focused
// session in the given mode (Control = takeover, View = read-only). No-op if
// no pane is focused or there is no client.
func (m GridModel) attachFocused(mode protocol.AttachMode) (GridModel, tea.Cmd) {
	id := m.FocusedTaskID()
	if id == "" || m.client == nil {
		return m, nil
	}
	cmd := DoAttachSession(m.client, id, mode)
	m.Close()
	return m, cmd
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
			// Enter = control (takeover) attach: go from monitoring straight to
			// interacting, mirroring the task list's r/R. Use v for read-only.
			return m.attachFocused(protocol.AttachMode_Control)
		case "v":
			// v = read-only view attach (mirrors the task list's v key).
			return m.attachFocused(protocol.AttachMode_View)
		case "x":
			m.dismissFocused()
			return m, nil
		case "[":
			m.setPage(m.page - 1)
			return m, nil
		case "]":
			m.setPage(m.page + 1)
			return m, nil
		// h/j/k/l (+ arrows): move focus within the current page.
		case "left", "h":
			m.moveFocus(-1)
		case "right", "l":
			m.moveFocus(1)
		case "up", "k":
			m.moveFocus(-m.pageCols())
		case "down", "j":
			m.moveFocus(m.pageCols())
		// Shift+H/J/K/L: move (reorder) the focused pane, crossing page bounds.
		case "H":
			m.movePane(-1)
		case "L":
			m.movePane(1)
		case "K":
			m.movePane(-m.pageCols())
		case "J":
			m.movePane(m.pageCols())
		}
	case gridTickMsg:
		if !m.open {
			return m, nil
		}
		return m, gridTick()
	}
	return m, nil
}

// moveFocus shifts focus by delta but clamps to the current page, so focus
// movement never silently jumps pages (use [ ] for that).
func (m *GridModel) moveFocus(delta int) {
	if len(m.panes) == 0 {
		return
	}
	j := m.focus + delta
	if j < m.pageStart() || j >= m.pageEnd() {
		return
	}
	m.focus = j
}

// movePane swaps the focused pane with the one delta positions away in the
// ordered list, following the moved pane (and its page). Crossing a page
// boundary is intentional — it is how a session is pushed onto another page to
// sit beside specific others.
func (m *GridModel) movePane(delta int) {
	j := m.focus + delta
	if j < 0 || j >= len(m.panes) {
		return
	}
	m.panes[m.focus], m.panes[j] = m.panes[j], m.panes[m.focus]
	m.focus = j
	m.page = m.focus / gridPerPage
}

func (m *GridModel) setPage(p int) {
	if p < 0 || p >= m.pageCount() {
		return
	}
	m.page = p
	m.focus = p * gridPerPage // land focus on the page's first pane
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
	// Keep the page in range and containing focus.
	if m.page >= m.pageCount() {
		m.page = m.pageCount() - 1
	}
	if m.page < 0 {
		m.page = 0
	}
	if len(m.panes) > 0 && (m.focus < m.pageStart() || m.focus >= m.pageEnd()) {
		m.page = m.focus / gridPerPage
	}
}

// statusLine is the one-row header: page position, session count, and key hints.
func (m GridModel) statusLine() string {
	txt := fmt.Sprintf(" grid  page %d/%d · %d sessions   [ ]:page  ⇧HJKL:move  hjkl:focus  ⏎:attach  v:view  x:close  q:quit ",
		m.page+1, m.pageCount(), len(m.panes))
	// MaxHeight(1) keeps the bar a single row even when the hint text is wider
	// than the terminal (Width would otherwise wrap it and steal a pane row,
	// overflowing the grid).
	return lipgloss.NewStyle().
		Width(m.width).MaxWidth(m.width).MaxHeight(1).
		Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252")).
		Render(txt)
}

func (m GridModel) View() string {
	if len(m.panes) == 0 {
		return PanelStyleFocused.Padding(0, 1).Render("grid: no live interactive sessions (esc to close)")
	}
	status := m.statusLine()

	start, end := m.pageStart(), m.pageEnd()
	page := m.panes[start:end]
	cols := gridCols(len(page))
	rows := (len(page) + cols - 1) / cols

	// Reserve the top row for the status line; panes tile the rest.
	gridH := m.height - 1
	if gridH < 4 {
		gridH = 4
	}
	cellW := m.width/cols - 2
	cellH := gridH/rows - 3
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
			li := r*cols + c
			if li >= len(page) {
				break
			}
			cells = append(cells, m.renderPane(start+li, cellW, cellH))
		}
		rowsOut = append(rowsOut, lipgloss.JoinHorizontal(lipgloss.Top, cells...))
	}
	grid := lipgloss.JoinVertical(lipgloss.Left, rowsOut...)
	return lipgloss.JoinVertical(lipgloss.Left, status, grid)
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
	// Truncate the header to the cell width so a long id + " (ended)" can never
	// wrap onto a second line (which would push the pane past its budgeted
	// height and overflow the grid).
	if len(head) > w {
		head = head[:w]
	}
	body := p.Render(w, h)
	style := PanelStyle
	if idx == m.focus {
		style = PanelStyleFocused
	}
	// MaxHeight/MaxWidth are a belt-and-suspenders clamp: even if some content
	// still rendered taller/wider than the cell, the pane can never exceed its
	// border-inclusive budget (w+2 × h+3), so the grid total stays within the
	// terminal and lipgloss.Place cannot clip the header row.
	return style.Width(w).Height(h + 1).MaxWidth(w + 2).MaxHeight(h + 3).Render(head + "\n" + body)
}
