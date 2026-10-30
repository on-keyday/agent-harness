package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// The Retain cell is filled only on a retained row: whether a topic is
// retained is the subject, so an empty cell is the answer, not an elision.
func TestBoardModalRetainColumn(t *testing.T) {
	m := NewBoardModal()
	m.SetSize(200, 40)
	m.ApplyTopics([]cli.BoardTopicRow{
		{Name: "kept", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1_000,
			Retention: &cli.TopicRetention{Forever: true, DiskBytes: 321}},
		{Name: "plain", MsgCount: 1, LastSeq: 2, LastPublishedAtMs: 1_000},
	}, nil)
	rows := m.topicsTable.Rows()
	col := -1
	for i, c := range m.topicsTable.Columns() {
		if c.Title == "Retain" {
			col = i
		}
	}
	if col < 0 {
		t.Fatal("no Retain column")
	}
	if got := rows[0][col]; got != "forever by=operator disk=321" {
		t.Errorf("retained row cell = %q", got)
	}
	if got := rows[1][col]; got != "" {
		t.Errorf("plain row cell = %q, want empty", got)
	}
}

// Through the App's real key routing: t on a topic opens the editor, typed
// keys land in it, and Enter produces the request.
func TestApp_BoardRetainKey(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyTopics([]cli.BoardTopicRow{{Name: "tree.x.d", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1}}, nil)

	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if !a.boardModal.Retaining() {
		t.Fatal("`t` on a topic did not open the retain editor")
	}
	if !strings.Contains(a.boardModal.View(), "retain tree.x.d") {
		t.Error("the editor does not name the topic it retains")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("72h")})
	_, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not produce the retain request")
	}
	if a.boardModal.Retaining() {
		t.Error("still editing after Enter")
	}
}

func TestBoardRetainInput(t *testing.T) {
	for _, c := range []struct {
		in      string
		ttl     time.Duration
		forever bool
		clear   bool
		bad     bool
	}{
		{in: "72h", ttl: 72 * time.Hour},
		{in: "forever", forever: true},
		{in: "clear", clear: true},
		{in: "soon", bad: true},
		{in: "0s", bad: true},
	} {
		ttl, forever, clear, err := parseRetainInput(c.in)
		if (err != nil) != c.bad || ttl != c.ttl || forever != c.forever || clear != c.clear {
			t.Errorf("%q = %v %v %v %v", c.in, ttl, forever, clear, err)
		}
	}
}

// A chat.* topic can never be retained, so `t` there says why instead of
// opening an editor whose every answer the server refuses.
func TestApp_BoardRetainKeyOnChatTopic(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyTopics([]cli.BoardTopicRow{{Name: "chat.0123abcd", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1}}, nil)
	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if a.boardModal.Retaining() {
		t.Fatal("`t` on a chat topic opened the retain editor")
	}
	if !strings.Contains(a.boardModal.View(), "cannot be retained") {
		t.Error("no reason shown for refusing to retain a chat topic")
	}
}
