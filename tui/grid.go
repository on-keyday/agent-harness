package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	gridPerPage  = 6  // panes shown per page (3x2)
	gridMaxPanes = 24 // total cap across all pages (bounds concurrent view streams)
	// gridStagger spaces successive panes' first attach (by pane index) so
	// opening the grid doesn't fire all attaches simultaneously — see the storm
	// note in GridModel.Open / PaneStreamer.startDelay.
	gridStagger = 60 * time.Millisecond
)

// gridDiag is set once at startup from HARNESS_GRID_DIAG: when on, each pane
// renders its DiagLine as the first body row so a black pane reveals its own
// state (bytes received, emulator size, content row) in a screenshot.
var gridDiag = os.Getenv("HARNESS_GRID_DIAG") != ""

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
// h/l move focus along that list and flip the page at a boundary, so left/right
// and paging are one motion; [ ] still jump a whole page at a time. k/j move
// focus within the current page. Shift+H/J/K/L reorder the focused pane
// (crossing page boundaries), so specific sessions can be grouped onto one page.
type GridModel struct {
	open   bool
	width  int
	height int
	panes  []*PaneStreamer
	focus  int // global index into panes
	page   int
	input  bool // input mode: keystrokes go to the focused pane (cowrite)
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
	for i, t := range live {
		p := NewPaneStreamer(FormatTaskID(t.Id), 24, 80)
		// Stagger first attaches: firing all N at once storms the shared client and
		// starves some panes' attach responses (they hang at rx=0 = permanent
		// black). Space them so the connection isn't saturated by simultaneous
		// replay bursts; the reattach loop + per-attach timeout still recover any
		// that slip through.
		p.startDelay = time.Duration(i) * gridStagger
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
	m.input = false
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
	if p := m.focusedPane(); p != nil {
		return p.TaskID()
	}
	return ""
}

func (m GridModel) focusedPane() *PaneStreamer {
	if m.focus < 0 || m.focus >= len(m.panes) {
		return nil
	}
	return m.panes[m.focus]
}

// keyToBytes encodes a bubbletea key event into the raw PTY bytes to forward to
// a session in input mode. Printable runes and the common control/navigation
// keys are covered; an unmapped exotic key returns nil (silently dropped —
// acceptable for in-grid typing). Alt prefixes ESC.
func keyToBytes(m tea.KeyMsg) []byte {
	var b []byte
	switch m.Type {
	case tea.KeyRunes:
		b = []byte(string(m.Runes))
	case tea.KeySpace:
		b = []byte(" ")
	case tea.KeyUp:
		b = []byte("\x1b[A")
	case tea.KeyDown:
		b = []byte("\x1b[B")
	case tea.KeyRight:
		b = []byte("\x1b[C")
	case tea.KeyLeft:
		b = []byte("\x1b[D")
	case tea.KeyHome:
		b = []byte("\x1b[H")
	case tea.KeyEnd:
		b = []byte("\x1b[F")
	case tea.KeyPgUp:
		b = []byte("\x1b[5~")
	case tea.KeyPgDown:
		b = []byte("\x1b[6~")
	case tea.KeyDelete:
		b = []byte("\x1b[3~")
	default:
		// The C0/named-control KeyTypes (Enter=CR, Tab=HT, Esc=ESC, Backspace=DEL,
		// Ctrl+A..Z, …) hold the raw control byte as their value. Start at 1: a
		// bare Ctrl / Ctrl+Space reports Type 0 (NUL / Ctrl+@) on some terminals,
		// and forwarding that spurious ^@ into the session is just noise.
		if m.Type > 0 && m.Type <= 127 {
			b = []byte{byte(m.Type)}
		} else {
			return nil
		}
	}
	if m.Alt {
		b = append([]byte{0x1b}, b...)
	}
	return b
}

func (m GridModel) Update(msg tea.Msg) (GridModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Input mode: every keystroke is forwarded to the focused pane's session
		// (cowrite), EXCEPT Ctrl+] which exits back to navigation. Esc is passed
		// through so it still reaches apps in the pane (vim, claude, …).
		if m.input {
			if msg.String() == "ctrl+]" {
				m.input = false
				return m, nil
			}
			if b := keyToBytes(msg); len(b) > 0 {
				if p := m.focusedPane(); p != nil {
					p.SendInput(b)
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "esc", "q":
			m.Close()
			return m, nil
		case "i":
			// Enter input mode on the focused pane (type into it in place).
			if m.FocusedTaskID() != "" {
				m.input = true
			}
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
		// h/l (+ arrows): move focus left/right across the whole pane list,
		// flipping the page at a boundary so left/right and paging are one
		// motion ([ ] still jumps a whole page). k/j stay within the page.
		case "left", "h":
			m.moveFocusLinear(-1)
		case "right", "l":
			m.moveFocusLinear(1)
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

// moveFocus shifts focus by delta but clamps to the current page. Used by
// vertical (k/j) movement: the delta is the current page's column count, which
// need not match the destination page's tiling, so a cross-page vertical jump
// would be ill-defined.
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

// moveFocusLinear shifts focus by delta across the whole ordered pane list,
// flipping the page to keep focus visible. Used by horizontal (h/l) movement so
// that pressing right on a page's last pane lands on the next page's first pane
// — left/right and page switching become one motion. [ ] still jumps a page.
func (m *GridModel) moveFocusLinear(delta int) {
	if len(m.panes) == 0 {
		return
	}
	j := m.focus + delta
	if j < 0 || j >= len(m.panes) {
		return
	}
	m.focus = j
	m.page = m.focus / gridPerPage
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
// In input mode it flips to an INPUT bar so it is obvious keystrokes go to the
// pane, not the grid.
func (m GridModel) statusLine() string {
	bg, fg := lipgloss.Color("236"), lipgloss.Color("252")
	txt := fmt.Sprintf(" grid  page %d/%d · %d sessions   [ ]:page  ⇧HJKL:move  hjkl:focus  i:input  ⏎:attach  v:view  x:close  q:quit ",
		m.page+1, m.pageCount(), len(m.panes))
	if m.input {
		bg, fg = lipgloss.Color("22"), lipgloss.Color("231") // green bar
		id := m.FocusedTaskID()
		if len(id) > 8 {
			id = id[:8]
		}
		txt = fmt.Sprintf(" ● INPUT → %s   (keys go to this session · Ctrl+]:exit input) ", id)
	}
	// MaxHeight(1) keeps the bar a single row even when the hint text is wider
	// than the terminal (Width would otherwise wrap it and steal a pane row,
	// overflowing the grid).
	return lipgloss.NewStyle().
		Width(m.width).MaxWidth(m.width).MaxHeight(1).
		Background(bg).Foreground(fg).
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
	if gridDiag {
		// Overlay the pane's own state on its first row (truncated to width) so a
		// black pane shows WHY. Replaces the top body row rather than adding one,
		// keeping the pane within its budgeted height.
		diag := p.DiagLine()
		if len(diag) > w {
			diag = diag[:w]
		}
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = diag + body[nl:]
		} else {
			body = diag
		}
	}
	style := PanelStyle
	if idx == m.focus {
		style = PanelStyleFocused
		if m.input {
			// Green border on the focused pane while typing into it, matching the
			// INPUT status bar.
			style = PanelStyle.BorderForeground(lipgloss.Color("42"))
			head = "● " + head
			if len(head) > w {
				head = head[:w]
			}
		}
	}
	// MaxHeight/MaxWidth are a belt-and-suspenders clamp: even if some content
	// still rendered taller/wider than the cell, the pane can never exceed its
	// border-inclusive budget (w+2 × h+3), so the grid total stays within the
	// terminal and lipgloss.Place cannot clip the header row.
	return style.Width(w).Height(h + 1).MaxWidth(w + 2).MaxHeight(h + 3).Render(head + "\n" + body)
}
