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
	timed := retainCell(&cli.BoardTopicRow{Retention: &cli.TopicRetention{TTL: time.Hour,
		ExpiresAt: time.Date(2026, 10, 31, 2, 15, 0, 0, time.UTC), DiskBytes: 7}})
	if timed != "1h0m0s until=2026-10-31T02:15:00Z by=operator disk=7" {
		t.Errorf("timed retention cell = %q", timed)
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

// The editor takes a period only. Stopping retention is its own key (T), not
// a word typed into the period box.
func TestBoardRetainInput(t *testing.T) {
	for _, c := range []struct {
		in      string
		ttl     time.Duration
		forever bool
		bad     bool
	}{
		{in: "72h", ttl: 72 * time.Hour},
		{in: "forever", forever: true},
		{in: "clear", bad: true},
		{in: "soon", bad: true},
		{in: "0s", bad: true},
	} {
		ttl, forever, err := cli.ParseRetainTTL(c.in)
		if (err != nil) != c.bad || ttl != c.ttl || forever != c.forever {
			t.Errorf("%q = %v %v %v", c.in, ttl, forever, err)
		}
	}
}

// T stops retaining — offered, in the footer and as a key, only on a topic
// that is retained.
func TestApp_BoardUnretainKey(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyTopics([]cli.BoardTopicRow{
		{Name: "kept", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1, Retention: &cli.TopicRetention{Forever: true}},
		{Name: "plain", MsgCount: 1, LastSeq: 2, LastPublishedAtMs: 1},
	}, nil)
	if !strings.Contains(a.boardModal.View(), "T: unretain") {
		t.Error("footer on a retained row does not offer T: unretain")
	}
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}}); cmd == nil {
		t.Fatal("T on a retained row produced no request")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyDown})
	if a.boardModal.SelectedTopicName() != "plain" {
		t.Fatalf("cursor on %q, want plain", a.boardModal.SelectedTopicName())
	}
	if strings.Contains(a.boardModal.View(), "T: unretain") {
		t.Error("footer on a plain row offers T: unretain")
	}
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}}); cmd != nil {
		t.Fatal("T on a topic that is not retained produced a request")
	}
	if !strings.Contains(a.boardModal.View(), "not retained") {
		t.Error("T on a plain row did not say why nothing happened")
	}
	// From the message view of a retained topic.
	a.boardModal.ApplyMessages("kept", nil, nil, true)
	if !strings.Contains(a.boardModal.View(), "T: unretain") {
		t.Error("message view of a retained topic does not offer T: unretain")
	}
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}}); cmd == nil {
		t.Fatal("T in a retained topic's message view produced no request")
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

// o opens a topic by name — one that does not exist yet included — and from
// its message view `m` sends to it and `t` retains it. That is the only way
// to start a topic from the TUI.
func TestApp_BoardOpenTopicByName(t *testing.T) {
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	a.boardModal.ApplyTopics(nil, nil)

	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !a.boardModal.OpeningTopic() {
		t.Fatal("`o` on the topic list did not open the name editor")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tree.x.new")})
	_, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not read the named topic")
	}
	if a.boardModal.OpeningTopic() {
		t.Error("still editing after Enter")
	}
	// What DoBoardRead delivers for a topic nothing was published to.
	a.boardModal.ApplyMessages("tree.x.new", nil, nil, false)
	if a.boardModal.Mode() != boardMessages || a.boardModal.CurTopic() != "tree.x.new" {
		t.Fatalf("mode %v topic %q, want the new topic's message view", a.boardModal.Mode(), a.boardModal.CurTopic())
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if !a.boardModal.Retaining() {
		t.Fatal("`t` in the message view did not open the retain editor for the open topic")
	}
}

// A server before 84ba1a35 sent a retained, never-published topic's zero
// publish time wrapped to ~1.8e19; the row must read as "nothing published",
// not year 1.
func TestBoardTopicRow_WrappedPublishTimeIsAbsent(t *testing.T) {
	row := boardTopicToRow(&cli.BoardTopicRow{Name: "beta.sns", LastPublishedAtMs: 18446681938112751616}, nil)
	if row[4] != "-" || row[5] != "-" {
		t.Fatalf("LastSeq/LastAt = %q / %q, want - / -", row[4], row[5])
	}
}

// The Retain cell is cut on a narrow terminal (at 80 columns, right after
// "until=2026-"), so the highlighted topic's retention is spelled out in full
// on its own line, whatever the width.
func TestBoardModal_SelectedRetentionLineAtNarrowWidth(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(80, 30)
	m.ApplyTopics([]cli.BoardTopicRow{{Name: "timed.topic", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1,
		Retention: &cli.TopicRetention{TTL: time.Hour, ExpiresAt: time.Date(2026, 10, 31, 2, 15, 0, 0, time.UTC), DiskBytes: 7}}}, nil)
	if v := m.View(); !strings.Contains(v, "until=2026-10-31T02:15:00Z") {
		t.Fatalf("the full expiry is not on screen at 80 columns:\n%s", v)
	}
	m.ApplyTopics([]cli.BoardTopicRow{{Name: "plain", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1}}, nil)
	if v := m.View(); strings.Contains(v, "retain=") {
		t.Fatalf("a plain topic got a retention line:\n%s", v)
	}
}
