package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// TestBoardModalApplyTopics verifies that ApplyTopics populates rowTopics and
// rebuilds the underlying table rows. Mirrors TestConnsModalApplySnapshot.
func TestBoardModalApplyTopics(t *testing.T) {
	m := NewBoardModal()

	rows := []cli.BoardTopicRow{
		{Name: "foo", MsgCount: 3, LastSeq: 3, LastPublishedAtMs: 1_000},
		{Name: "bar", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 2_000},
	}
	m.ApplyTopics(rows, nil)

	if got := len(m.rowTopics); got != 2 {
		t.Fatalf("rowTopics: want 2, got %d", got)
	}
	// Sorted by name, not left in server order: BoardTopics iterates a map and
	// declares its order unspecified, so the list would otherwise reshuffle on
	// every refresh.
	if m.rowTopics[0].Name != "bar" {
		t.Errorf("rowTopics[0].Name: want bar, got %s", m.rowTopics[0].Name)
	}
	if m.rowTopics[1].Name != "foo" {
		t.Errorf("rowTopics[1].Name: want foo, got %s", m.rowTopics[1].Name)
	}
}

// TestBoardModalApplyTopicsUnion covers the case the listing exists for: a
// topic nobody has published to has no board topic at all, so it can only
// appear via the subscription set.
func TestBoardModalApplyTopicsUnion(t *testing.T) {
	m := NewBoardModal()
	rows := []cli.BoardTopicRow{
		{Name: "chat.aaaa", MsgCount: 2, LastSeq: 2, LastPublishedAtMs: 1_000},
		{Name: "orphan.x", MsgCount: 1, LastSeq: 3, LastPublishedAtMs: 2_000},
	}
	subs := map[string]int{"chat.aaaa": 1, "rr.dec-019": 2}

	m.ApplyTopics(rows, subs)

	var names []string
	for _, r := range m.rowTopics {
		names = append(names, r.Name)
	}
	want := []string{"chat.aaaa", "orphan.x", "rr.dec-019"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
	// The subscribed-only row exists but has nothing retained.
	for _, r := range m.rowTopics {
		if r.Name == "rr.dec-019" && (r.MsgCount != 0 || r.LastPublishedAtMs != 0) {
			t.Errorf("subscribed-only row should carry no message stats: %+v", r)
		}
	}
	// The Subs column shows 0 for a topic nobody subscribes to, and blank (not
	// 0) when counts are unavailable — an absent count must not read as "nobody".
	withCounts := boardTopicToRow(&m.rowTopics[1], subs) // orphan.x
	if withCounts[boardColSubs] != "0" {
		t.Errorf("orphan.x Subs = %q, want 0", withCounts[boardColSubs])
	}
	noCounts := boardTopicToRow(&m.rowTopics[1], nil)
	if noCounts[boardColSubs] != "" {
		t.Errorf("Subs with nil counts = %q, want blank", noCounts[boardColSubs])
	}
	// Retr is printed even at zero, unlike Subs: the count is always available,
	// so a blank would read as "unavailable" the way it does one column over.
	if noCounts[boardColRetr] != "0" {
		t.Errorf("Retr with no retractions = %q, want 0", noCounts[boardColRetr])
	}
	withRetracted := m.rowTopics[1]
	withRetracted.RetractedCount = 3
	if got := boardTopicToRow(&withRetracted, subs); got[boardColRetr] != "3" {
		t.Errorf("Retr = %q, want 3", got[boardColRetr])
	}
	// Withdrawn messages must never be folded into Msgs: that column answers
	// "how much would a subscriber receive", so setting RetractedCount leaves
	// it exactly where it was.
	live := boardTopicToRow(&m.rowTopics[1], subs)[boardColMsgs]
	if got := boardTopicToRow(&withRetracted, subs); got[boardColMsgs] != live {
		t.Errorf("Msgs changed when RetractedCount was set: %q -> %q", live, got[boardColMsgs])
	}
}

// TestBoardModalDrillAndPop exercises the two-mode state machine:
//   - ApplyMessages transitions to boardMessages mode.
//   - PopToTopics returns to boardTopics mode.
//   - Close marks the modal as not open.
func TestBoardModalDrillAndPop(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	if !m.IsOpen() {
		t.Fatal("IsOpen should be true after Open()")
	}
	if m.mode != boardTopics {
		t.Fatalf("initial mode: want boardTopics, got %v", m.mode)
	}

	msgs := []cli.BoardMessage{
		{Seq: 1, FromTaskHex: "aabbccdd", FromHostname: "host1", ReceivedAtMs: 1_000, Payload: []byte(`"hello"`)},
	}
	m.ApplyMessages("testtopic", msgs, true)
	if m.mode != boardMessages {
		t.Fatalf("after ApplyMessages: want boardMessages, got %v", m.mode)
	}
	if m.curTopic != "testtopic" {
		t.Errorf("curTopic: want testtopic, got %s", m.curTopic)
	}

	// Esc from message mode pops to topic mode (simulated via PopToTopics).
	m.PopToTopics()
	if m.mode != boardTopics {
		t.Fatalf("after PopToTopics: want boardTopics, got %v", m.mode)
	}

	// Esc from topic mode closes the modal (simulated via Close).
	m.Close()
	if m.IsOpen() {
		t.Fatal("IsOpen should be false after Close()")
	}
}

// TestBoardModalContentFormatsJSON verifies that a message carrying a valid JSON
// payload is pretty-printed in the content viewport. A plain-text payload
// should appear verbatim.
func TestBoardModalContentFormatsJSON(t *testing.T) {
	m := NewBoardModal()

	jsonMsg := cli.BoardMessage{
		Seq:          7,
		FromTaskHex:  "deadbeef001122334455667788990011",
		FromHostname: "node1",
		ReceivedAtMs: 3_000,
		Payload:      []byte(`{"key":"value","n":42}`),
	}
	m.ApplyMessages("jsontopic", []cli.BoardMessage{jsonMsg}, true)

	got := m.content.View()
	// The pretty-printed JSON must contain indented fields.
	if !strings.Contains(got, `"key": "value"`) {
		t.Errorf("content.View() missing indented JSON key-value; got:\n%s", got)
	}
	if !strings.Contains(got, `"n": 42`) {
		t.Errorf("content.View() missing indented JSON n:42; got:\n%s", got)
	}
}

// TestBoardModalSubscribersMode verifies that ApplySubscribers switches into
// subscribers mode and renders the rows, that an empty result says so rather
// than rendering blank, and that Esc's PopToTopics reaches back from it.
func TestBoardModalSubscribersMode(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(100, 30)

	m.ApplySubscribers("rr.dec-019", []cli.BoardSubscriberRow{
		{TaskHex: "aabbccddeeff0011", Hostname: "host-A", AgentProfile: "claude", Patterns: []string{"chat.aabbccdd", "rr.dec-019"}},
		// Registered but never attached: empty hostname must render as "-".
		{TaskHex: "1122334455667788", Hostname: "", AgentProfile: "codex", Patterns: []string{"rr.dec-019"}},
	})
	if m.mode != boardSubscribers {
		t.Fatalf("mode = %v, want boardSubscribers", m.mode)
	}
	view := m.View()
	for _, want := range []string{"subscribers of rr.dec-019 (2)", "aabbccdd", "host=host-A", "agent=codex", "host=-", "rr.dec-019"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n%s", want, view)
		}
	}

	// An empty subscriber set is a finding, not a blank pane.
	m.ApplySubscribers("quiet.topic", nil)
	if v := m.View(); !strings.Contains(v, "nobody subscribes") {
		t.Errorf("empty view does not state the finding:\n%s", v)
	}

	m.PopToTopics()
	if m.mode != boardTopics {
		t.Fatalf("after PopToTopics: mode = %v, want boardTopics", m.mode)
	}
}

// TestBoardModalEmptyStates covers the two ways a topic shows no messages.
// Both used to be reachable only as a race; the union listing makes the
// never-published one an ordinary destination, and the not-found path used to
// leave the modal on the topic list writing into a viewport that view never
// renders.
func TestBoardModalEmptyStates(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(100, 30)

	// Never published: BoardRead reports found=false.
	m.ApplyMessages("rr.dec-019", nil, false)
	if m.mode != boardMessages {
		t.Fatalf("mode = %v, want boardMessages even when nothing is retained", m.mode)
	}
	if v := m.View(); !strings.Contains(v, "nothing published to this topic") {
		t.Errorf("view does not name the never-published state:\n%s", v)
	}

	// Published then emptied: the topic exists, its ring is empty.
	m.ApplyMessages("chat.aaaa", nil, true)
	if m.mode != boardMessages {
		t.Fatalf("mode = %v, want boardMessages", m.mode)
	}
	v := m.View()
	if !strings.Contains(v, "on the board, but holds no messages") {
		t.Errorf("view does not name the emptied-ring state:\n%s", v)
	}
	if strings.Contains(v, "nothing published to this topic") {
		t.Errorf("emptied ring must not be reported as never published:\n%s", v)
	}
}
