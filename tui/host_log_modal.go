package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/cli"
)

// HostLogResultMsg delivers the answer to `server log` or `runner log`.
type HostLogResultMsg struct {
	Target   string // "server" or "runner <prefix>"
	RunnerID string // empty for server log
	Opts     cli.HostLogOpts
	Snap     cli.HostLogSnapshot
	Err      error
}

// DoHostLogServer fetches the server's process log using the long-lived client.
func DoHostLogServer(c *cli.Client, opts cli.HostLogOpts) tea.Cmd {
	return func() tea.Msg {
		snap, err := cli.HostLogServerWith(context.Background(), c, opts)
		return HostLogResultMsg{Target: "server", Opts: opts, Snap: snap, Err: err}
	}
}

// DoHostLogRunner fetches a connected runner's process log using the long-lived client.
func DoHostLogRunner(c *cli.Client, runnerID string, opts cli.HostLogOpts) tea.Cmd {
	return func() tea.Msg {
		snap, err := cli.HostLogRunnerWith(context.Background(), c, runnerID, opts)
		return HostLogResultMsg{Target: "runner " + runnerID, RunnerID: runnerID, Opts: opts, Snap: snap, Err: err}
	}
}

// HostLogModal is a scrollable modal displaying server or runner process logs
// (pillar C, docs/superpowers/specs/2026-11-28-pillar-c-host-logs-design.md).
type HostLogModal struct {
	open     bool
	width    int
	height   int
	title    string
	header   string
	runnerID string
	opts     cli.HostLogOpts
	lines    []string
	errText  string
	loading  bool
	scroll   int
}

func NewHostLogModal() HostLogModal { return HostLogModal{} }

func (m *HostLogModal) IsOpen() bool { return m.open }

func (m *HostLogModal) SetSize(w, h int) {
	m.width, m.height = w, h
}

func (m *HostLogModal) OpenLoading(target, runnerID string, opts cli.HostLogOpts) {
	m.open = true
	m.title = target + " log"
	m.runnerID = runnerID
	m.opts = opts
	m.header = "loading..."
	m.lines = nil
	m.errText = ""
	m.loading = true
	m.scroll = 0
}

func (m *HostLogModal) Close() { m.open = false }

func (m *HostLogModal) RunnerID() string     { return m.runnerID }
func (m *HostLogModal) Opts() cli.HostLogOpts { return m.opts }

func (m *HostLogModal) SetResult(msg HostLogResultMsg) {
	m.loading = false
	m.title = msg.Target + " log"
	m.runnerID = msg.RunnerID
	m.opts = msg.Opts
	if msg.Err != nil {
		m.errText = msg.Err.Error()
		m.header = ""
		m.lines = nil
		m.scroll = 0
		return
	}
	m.errText = ""
	m.header = cli.FormatHostLogHeader(msg.Target, msg.Snap)
	raw := strings.ReplaceAll(string(msg.Snap.Data), "\r\n", "\n")
	if raw == "" {
		m.lines = []string{"(empty)"}
	} else {
		m.lines = strings.Split(strings.TrimRight(raw, "\n"), "\n")
	}
	// Start at bottom on tail reads so newest lines are immediately visible.
	if !msg.Opts.HasOffset {
		m.ScrollBottom()
	} else {
		m.scroll = 0
	}
}

func (m *HostLogModal) bodyHeight() int {
	h := m.height - 10
	if h < 6 {
		h = 6
	}
	return h
}

func (m *HostLogModal) maxScroll() int {
	max := len(m.lines) - m.bodyHeight()
	if max < 0 {
		return 0
	}
	return max
}

func (m *HostLogModal) ScrollBy(delta int) {
	m.scroll += delta
	if m.scroll < 0 {
		m.scroll = 0
	}
	if max := m.maxScroll(); m.scroll > max {
		m.scroll = max
	}
}

func (m *HostLogModal) ScrollTop()    { m.scroll = 0 }
func (m *HostLogModal) ScrollBottom() { m.scroll = m.maxScroll() }

func (m *HostLogModal) View() string {
	w := m.width - 8
	if w < 44 {
		w = 44
	}
	if w > 120 {
		w = 120
	}
	bh := m.bodyHeight()
	var body string
	switch {
	case m.loading:
		body = MutedStyle.Render("Fetching process log...")
	case m.errText != "":
		body = ErrorStyle.Render(m.errText)
	default:
		end := m.scroll + bh
		if end > len(m.lines) {
			end = len(m.lines)
		}
		visible := make([]string, 0, end-m.scroll)
		innerW := w - 4
		if innerW < 20 {
			innerW = 20
		}
		for _, ln := range m.lines[m.scroll:end] {
			if len(ln) > innerW {
				ln = ln[:innerW-1] + "…"
			}
			visible = append(visible, ln)
		}
		body = strings.Join(visible, "\n")
	}

	title := HeaderStyle.Render(m.title)
	meta := ""
	if m.header != "" {
		meta = MutedStyle.Render(m.header) + "\n"
	}
	pos := ""
	if len(m.lines) > 0 {
		pos = fmt.Sprintf("  [%d/%d]", m.scroll+1, len(m.lines))
	}
	footer := MutedStyle.Render("j/k scroll  g/G top/bottom  r refresh  s server log  Esc close" + pos)
	content := title + "\n" + meta + "\n" + body + "\n\n" + footer
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Width(w).
		Render(content)
}

func (a *App) inHostLogModal(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		a.hostLogModal.Close()
		return nil
	case "j", "down":
		a.hostLogModal.ScrollBy(1)
		return nil
	case "k", "up":
		a.hostLogModal.ScrollBy(-1)
		return nil
	case "pgdown", "ctrl+d":
		a.hostLogModal.ScrollBy(a.hostLogModal.bodyHeight() / 2)
		return nil
	case "pgup", "ctrl+u":
		a.hostLogModal.ScrollBy(-a.hostLogModal.bodyHeight() / 2)
		return nil
	case "g":
		a.hostLogModal.ScrollTop()
		return nil
	case "G":
		a.hostLogModal.ScrollBottom()
		return nil
	case "r":
		if a.client == nil {
			return nil
		}
		rid := a.hostLogModal.RunnerID()
		opts := a.hostLogModal.Opts()
		if rid == "" {
			a.hostLogModal.OpenLoading("server", "", opts)
			return DoHostLogServer(a.client, opts)
		}
		a.hostLogModal.OpenLoading("runner "+rid[:min(8, len(rid))], rid, opts)
		return DoHostLogRunner(a.client, rid, opts)
	case "s":
		if a.client == nil {
			return nil
		}
		opts := cli.HostLogOpts{}
		a.hostLogModal.OpenLoading("server", "", opts)
		return DoHostLogServer(a.client, opts)
	}
	return nil
}
