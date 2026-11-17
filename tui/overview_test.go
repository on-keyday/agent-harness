package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func fullOverview() cli.Overview {
	long := strings.Repeat("x", 200)
	o := cli.Overview{
		NeedsYouCount: 9, OperatorClients: 2, HostCount: 9, SlotCount: 30, ForwardCount: 9, TopicCount: 9, MsgTotal: 99,
		GroupCounts: map[string]int{"ready": 1, "working": 2, "no report": 3, "queued": 4, "finished": 5},
		HostsErr:    long, ForwardsErr: long, BoardErr: long,
	}
	for i := 0; i < cli.OverviewRowCap; i++ {
		row := cli.OverviewTask{ID: strings.Repeat("ab", 16), Repo: long, Badge: "wait:3m", Now: long}
		o.NeedsYou = append(o.NeedsYou, row)
		o.Live = append(o.Live, row)
		o.Hosts = append(o.Hosts, cli.OverviewHost{Hostname: long, Goos: "linux"})
		o.Forwards = append(o.Forwards, cli.OverviewForward{ID: 1, Dir: "-L", Spec: long, TaskID: strings.Repeat("cd", 16), Repo: long})
		o.Topics = append(o.Topics, cli.OverviewTopic{Name: long, MsgCount: 3, Head: long})
	}
	return o
}

// Every section full, every error set, every string too long: the overlay
// still fits the TUI's minimum terminal.
func TestOverviewViewFits80x24(t *testing.T) {
	m := NewOverviewModel()
	m.SetSize(80, 24)
	m.Open()
	m.Apply(fullOverview(), false)
	lines := strings.Split(m.View(), "\n")
	if len(lines) > 24 {
		t.Fatalf("%d lines:\n%s", len(lines), m.View())
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > 80 {
			t.Errorf("line %d is %d cells: %q", i, w, l)
		}
	}
}

// A poll result or tick of a generation the overlay has left is dropped, so
// closing it stops the chain.
func TestOverviewPollStopsOnClose(t *testing.T) {
	a := New(Config{})
	g := a.overview.Open()
	a.overview.Close()
	if _, cmd := a.Update(overviewTickMsg{gen: g}); cmd != nil {
		t.Error("a tick after close re-armed the poll")
	}
	if _, cmd := a.Update(OverviewFetchMsg{gen: g}); cmd != nil {
		t.Error("a result after close re-armed the poll")
	}
	g2 := a.overview.Open()
	if _, cmd := a.Update(overviewTickMsg{gen: g2 - 1}); cmd != nil {
		t.Error("a stale generation's tick re-armed the poll")
	}
}

func TestOverviewEnterTaskSelects(t *testing.T) {
	a := New(Config{})
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	mk := func(b byte) protocol.TaskInfo {
		var ti protocol.TaskInfo
		ti.Id.Id[0] = b
		ti.Kind = protocol.TaskKind_Interactive
		ti.Status = protocol.TaskStatus_Running
		ti.CreatedAt = uint64(b) * 1e9
		return ti
	}
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{mk(0x11), mk(0x22)}})
	want := FormatTaskID(mk(0x11).Id)
	a.overview.Open()
	a.rebuildOverview()
	found := false
	for i := 0; i < 30; i++ {
		if kind, key := a.overview.Selected(); kind == "task" && key == want {
			found = true
			break
		}
		a.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !found {
		t.Fatalf("no task row for %s in:\n%s", want, a.overview.View())
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if a.overview.IsOpen() {
		t.Fatal("enter left the overlay open")
	}
	if got := a.tasks.SelectedID(); got != want || a.focus != focusTasks {
		t.Errorf("selected %q focus %v, want %q on the tasks table", got, a.focus, want)
	}
}

// Esc closes, like every overlay.
func TestOverviewEscCloses(t *testing.T) {
	a := New(Config{})
	a.overview.Open()
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if a.overview.IsOpen() {
		t.Fatal("esc left the overlay open")
	}
}

// The TUI rows carry the same escaping as the CLI: no raw control byte from a
// topic name, hostname or error reaches the overlay's text.
func TestOverviewRowsEscapeForeignText(t *testing.T) {
	o := cli.BuildOverview(cli.OverviewInput{
		Topics:    []cli.BoardTopicRow{{Name: "evil\x1b[2J\ntopic", MsgCount: 1}},
		TopicsErr: errors.New("x\x1b[2J"),
	}, time.Now())
	for _, r := range overviewRows(o, false) {
		if strings.ContainsAny(r.text, "\x1b\n") {
			t.Errorf("raw control byte in row %q", r.text)
		}
	}
}

// Before the first fetch of conns, forwards and the board, those counts are
// not zero -- they are unknown, and the overlay says so.
func TestOverviewUnfetchedShowsNoZeros(t *testing.T) {
	a := New(Config{})
	a.overview.Open()
	a.rebuildOverview()
	v := a.overview.View()
	for _, bad := range []string{"forwards: 0", "board: 0 topics", "0 operator clients"} {
		if strings.Contains(v, bad) {
			t.Errorf("unmeasured %q shown as a measurement:\n%s", bad, v)
		}
	}
	a.Update(OverviewFetchMsg{gen: a.overview.Gen()})
	if v := a.overview.View(); !strings.Contains(v, "forwards: 0") {
		t.Errorf("after a fetch the zero is a measurement and must show:\n%s", v)
	}
}

// A tick that finds the TUI disconnected keeps the chain alive, so the
// overlay resumes refreshing after a reconnect.
func TestOverviewPollSurvivesDisconnect(t *testing.T) {
	a := New(Config{})
	g := a.overview.Open()
	if _, cmd := a.Update(overviewTickMsg{gen: g}); cmd == nil {
		t.Fatal("a tick while disconnected ended the poll chain")
	}
}
