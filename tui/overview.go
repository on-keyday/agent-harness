package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// overviewEvery is how often the open overlay re-reads what the App does not
// already hold (conns, forwards, the board) -- the WebUI's snapshot period.
const overviewEvery = 5 * time.Second

// OverviewFetchMsg carries the overview inputs the overlay fetches itself.
// gen is the opening it was asked for; a result from an earlier opening is
// dropped (see overviewTickMsg).
type OverviewFetchMsg struct {
	gen         int
	Conns       []protocol.ConnInfo
	ConnsErr    error
	Forwards    []protocol.PortForwardInfo
	ForwardsErr error
	Topics      []cli.BoardTopicRow
	Newest      map[string]*cli.BoardMessage
	TopicsErr   error
}

// overviewTickMsg asks for the next fetch of generation gen. Only an open
// overlay of the same generation acts on it, so closing (or reopening) the
// overlay ends the previous chain: this is the trsf poll's rule
// (trsfTickMsg).
type overviewTickMsg struct{ gen int }

func overviewTick(gen int) tea.Cmd {
	return tea.Tick(overviewEvery, func(time.Time) tea.Msg { return overviewTickMsg{gen: gen} })
}

// DoOverviewFetch reads conns, forwards and the board over the App's
// long-lived client.
func DoOverviewFetch(c *cli.Client, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m := OverviewFetchMsg{gen: gen}
		m.Conns, m.ConnsErr = c.ConnListWith(ctx)
		m.Forwards, m.ForwardsErr = c.PortForwardListWith(ctx, cli.ForwardListQuery{})
		m.Topics, m.Newest, m.TopicsErr = c.OverviewBoardWith(ctx)
		return m
	}
}

// overviewRow is one line of the overlay. A header row carries its section in
// kind ("header:<section>"); a data row carries what Enter opens.
type overviewRow struct {
	kind string // task | host | forward | topic | header:<section> | err
	key  string // task id / hostname / forward id / topic name
	text string
}

// OverviewModel is the full-screen overview overlay: cli.BuildOverview's
// sections, one selectable line each.
type OverviewModel struct {
	open   bool
	gen    int
	width  int
	height int
	rows   []overviewRow
	cursor int
}

func NewOverviewModel() OverviewModel { return OverviewModel{} }

// Open opens the overlay under a fresh generation and returns it.
func (m *OverviewModel) Open() int {
	m.open = true
	m.gen++
	m.cursor = 0
	return m.gen
}

func (m *OverviewModel) Close()       { m.open = false }
func (m *OverviewModel) IsOpen() bool { return m.open }
func (m *OverviewModel) Gen() int     { return m.gen }

func (m *OverviewModel) SetSize(w, h int) { m.width, m.height = w, h }

// Apply replaces the rows with o's, keeping the cursor on the same row when
// that row still exists.
func (m *OverviewModel) Apply(o cli.Overview, pending bool) {
	var prev overviewRow
	if m.cursor < len(m.rows) {
		prev = m.rows[m.cursor]
	}
	m.rows = overviewRows(o, pending)
	m.cursor = 0
	for i, r := range m.rows {
		if r.kind == prev.kind && r.key == prev.key && r.kind != "err" {
			m.cursor = i
			break
		}
	}
	if m.rows[m.cursor].kind == "err" {
		m.move(1)
	}
}

// Selected is the row under the cursor: its kind and key.
func (m *OverviewModel) Selected() (kind, key string) {
	if m.cursor >= len(m.rows) {
		return "", ""
	}
	r := m.rows[m.cursor]
	return r.kind, r.key
}

// move steps the cursor by d, skipping error lines, which open nothing.
func (m *OverviewModel) move(d int) {
	for i := m.cursor + d; i >= 0 && i < len(m.rows); i += d {
		if m.rows[i].kind != "err" {
			m.cursor = i
			return
		}
	}
}

func (m OverviewModel) Update(msg tea.KeyMsg) OverviewModel {
	switch msg.String() {
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	}
	return m
}

// overviewRows lays o out one line per row, every line through
// cli.OverviewText. pending says conns, forwards and the board have not been
// read yet: their counts are then unknown, not zero, and print as "…".
func overviewRows(o cli.Overview, pending bool) []overviewRow {
	var rows []overviewRow
	add := func(r overviewRow) {
		r.text = cli.OverviewText(r.text)
		rows = append(rows, r)
	}
	head := func(section, text string) {
		add(overviewRow{kind: "header:" + section, text: text})
	}
	errRow := func(e string) {
		if e != "" {
			add(overviewRow{kind: "err", text: "  error: " + e})
		}
	}
	task := func(t cli.OverviewTask) {
		add(overviewRow{kind: "task", key: t.ID,
			text: fmt.Sprintf("  %-10s %-14s %s  %s", t.Badge, truncateLeft(t.Repo, 14), t.ID[:min(8, len(t.ID))], t.Now)})
	}

	head("needs-you", fmt.Sprintf("needs you: %d", o.NeedsYouCount))
	for _, t := range o.NeedsYou {
		task(t)
	}
	g := o.GroupCounts
	head("tasks", fmt.Sprintf("tasks: %d ready, %d working, %d no report, %d queued, %d finished",
		g["ready"], g["working"], g["no report"], g["queued"], g["finished"]))
	for _, t := range o.Live {
		task(t)
	}
	clients := fmt.Sprint(o.OperatorClients)
	if pending {
		clients = "…"
	}
	head("hosts", fmt.Sprintf("hosts: %d (%d slots, %s operator clients)", o.HostCount, o.SlotCount, clients))
	errRow(o.HostsErr)
	for _, h := range o.Hosts {
		add(overviewRow{kind: "host", key: h.Hostname,
			text: fmt.Sprintf("  %-16s %-8s idle %d busy %d offline %d  tasks %d/%d", truncateLeft(h.Hostname, 16), h.Goos, h.Idle, h.Busy, h.Offline, h.ActiveTasks, h.MaxTasks)})
	}
	if pending {
		head("forwards", "forwards: …")
	} else {
		head("forwards", fmt.Sprintf("forwards: %d", o.ForwardCount))
	}
	errRow(o.ForwardsErr)
	for _, f := range o.Forwards {
		add(overviewRow{kind: "forward", key: fmt.Sprint(f.ID),
			text: fmt.Sprintf("  %-3s %s  %s %s", f.Dir, f.Spec, truncateLeft(f.Repo, 14), f.TaskID[:min(8, len(f.TaskID))])})
	}
	if pending {
		head("board", "board: …")
	} else {
		head("board", fmt.Sprintf("board: %d topics, %d messages", o.TopicCount, o.MsgTotal))
	}
	errRow(o.BoardErr)
	for _, t := range o.Topics {
		add(overviewRow{kind: "topic", key: t.Name,
			text: fmt.Sprintf("  %-24s %d msgs  %-8s %s", truncateLeft(t.Name, 24), t.MsgCount, t.Age, t.Head)})
	}
	return rows
}

func (m OverviewModel) View() string {
	w := m.width
	if w <= 0 {
		w = 80
	}
	var b strings.Builder
	b.WriteString(HeaderStyle.Render(fitCells("overview · ↑↓ select · enter open · esc close", w)))
	for i, r := range m.rows {
		line := fitCells(r.text, w)
		switch {
		case i == m.cursor:
			line = CursorStyle.Render(line)
		case strings.HasPrefix(r.kind, "header:"):
			line = HeaderStyle.Render(line)
		case r.kind == "err":
			line = ErrorStyle.Render(line)
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// fitCells cuts s to at most w terminal cells.
func fitCells(s string, w int) string {
	if runewidth.StringWidth(s) <= w {
		return s
	}
	return runewidth.Truncate(s, w, "…")
}

// rebuildOverview recomputes the overlay from the App's snapshot plus the last
// fetch. Called on a snapshot and on each fetch while the overlay is open.
func (a *App) rebuildOverview() {
	tasks := make([]protocol.TaskInfo, 0, len(a.tasksByID))
	for _, t := range a.tasksByID {
		tasks = append(tasks, t)
	}
	f := a.overviewIn
	a.overview.Apply(cli.BuildOverview(cli.OverviewInput{
		Runners: a.runnersSnapshot, Tasks: tasks,
		Conns: f.Conns, ConnsErr: f.ConnsErr,
		Forwards: f.Forwards, ForwardsErr: f.ForwardsErr,
		Topics: f.Topics, TopicsErr: f.TopicsErr, Newest: f.Newest,
	}, time.Now()), !a.overviewHave)
}

// openOverview is the V key and the `overview` verb.
func (a *App) openOverview() tea.Cmd {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("overview: not connected"))
		return nil
	}
	gen := a.overview.Open()
	a.overview.SetSize(a.width, a.height)
	// The last opening's read is kept: stale until this opening's first
	// fetch lands, but measured. Before any read the counts print as "…".
	a.rebuildOverview()
	return DoOverviewFetch(a.client, gen)
}

func (a *App) onOverview(tea.KeyMsg) (tea.Cmd, bool) { return a.openOverview(), true }

// inOverview: Esc closes; ↑↓ select; Enter closes the overlay and opens what
// the selected row is about, where the rest of the TUI shows it.
func (a *App) inOverview(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		a.overview.Close()
		return nil
	case tea.KeyEnter:
		kind, key := a.overview.Selected()
		a.overview.Close()
		return a.openOverviewTarget(kind, key)
	}
	a.overview = a.overview.Update(msg)
	return nil
}

func (a *App) openOverviewTarget(kind, key string) tea.Cmd {
	switch kind {
	case "task":
		a.tasks.SelectID(key)
		a.setFocus(focusTasks)
	case "header:needs-you", "header:tasks":
		a.setFocus(focusTasks)
	case "host", "header:hosts":
		a.setFocus(focusRunners)
	case "forward", "header:forwards":
		cmd, _ := a.onForwards(tea.KeyMsg{})
		return cmd
	case "topic":
		cmd, _ := a.onBoard(tea.KeyMsg{})
		return tea.Batch(cmd, DoBoardRead(a.client, key))
	case "header:board":
		cmd, _ := a.onBoard(tea.KeyMsg{})
		return cmd
	}
	return nil
}
