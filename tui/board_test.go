package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	m.ApplyMessages("testtopic", msgs, nil, true)
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
	m.ApplyMessages("jsontopic", []cli.BoardMessage{jsonMsg}, nil, true)

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
		{TaskHex: "aabbccddeeff0011", Hostname: "host-A", AgentProfile: "claude", Patterns: []cli.BoardSubscriberPattern{
			{Name: "chat.aabbccdd", Shown: 4200, Pending: 2},
			{Name: "rr.dec-019", Shown: 0, Pending: 0},
		}},
		// Registered but never attached: empty hostname must render as "-".
		{TaskHex: "1122334455667788", Hostname: "", AgentProfile: "codex", Patterns: []cli.BoardSubscriberPattern{
			{Name: "rr.dec-019", Shown: 0, Pending: 0},
		}},
	})
	if m.mode != boardSubscribers {
		t.Fatalf("mode = %v, want boardSubscribers", m.mode)
	}
	view := m.View()
	// shown/pending ride in the topics cell, and a zero mark is PRINTED, not
	// elided: "subscribed, nothing yet" and "subscribed, all read" are different
	// states and the field exists to separate them.
	for _, want := range []string{
		"subscribers of rr.dec-019 (2)", "aabbccdd", "host=host-A", "agent=codex", "host=-",
		"chat.aabbccdd(shown=4200 pending=2)", "rr.dec-019(shown=0 pending=0)",
	} {
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
	m.ApplyMessages("rr.dec-019", nil, nil, false)
	if m.mode != boardMessages {
		t.Fatalf("mode = %v, want boardMessages even when nothing is retained", m.mode)
	}
	if v := m.View(); !strings.Contains(v, "nothing published to this topic") {
		t.Errorf("view does not name the never-published state:\n%s", v)
	}

	// Published then emptied: the topic exists, its ring is empty.
	m.ApplyMessages("chat.aaaa", nil, nil, true)
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

// The per-message delivery mark is the answer to "which of these has the peer
// actually been handed". It must appear on EVERY row, including the ones
// everybody has: eliding it at 1/1 would make an undelivered 0/1 read as the
// only row that reports delivery at all, rather than as the exceptional one.
func TestBoardModalMessagesShowDeliveryMark(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 30)

	// One subscriber, shown up to seq 20: seq 10 and 20 have been handed over,
	// seq 30 has not.
	subs := []cli.BoardSubscriberRow{{
		TaskHex:  "aabbccddeeff0011",
		Hostname: "host-A",
		Patterns: []cli.BoardSubscriberPattern{{Name: "t.deliver", Shown: 20, Pending: 1}},
	}}
	msgs := []cli.BoardMessage{
		{Seq: 10, FromTaskHex: "11112222", ReceivedAtMs: 1_000},
		{Seq: 20, FromTaskHex: "33334444", ReceivedAtMs: 2_000},
		{Seq: 30, FromTaskHex: "55556666", ReceivedAtMs: 3_000},
	}
	m.ApplyMessages("t.deliver", msgs, subs, true)

	view := m.View()
	for _, want := range []string{
		"seq=10", "seq=20", "seq=30",
		"shown_to=1/1", // the two at or below the mark
		"shown_to=0/1", // the one above it
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n%s", want, view)
		}
	}
	if got := strings.Count(view, "shown_to=1/1"); got != 2 {
		t.Errorf("shown_to=1/1 appears %d times, want 2 (seq 10 and 20)", got)
	}
	if got := strings.Count(view, "shown_to=0/1"); got != 1 {
		t.Errorf("shown_to=0/1 appears %d times, want 1 (seq 30)", got)
	}
}

// A topic nobody subscribes to reports 0/0, which is a different fact from
// 0/1 (someone subscribes and has not been handed it). Both print.
func TestBoardModalMessagesMarkWithNoSubscribers(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 30)
	m.ApplyMessages("t.orphan", []cli.BoardMessage{{Seq: 7, FromTaskHex: "abcdabcd"}}, nil, true)
	if view := m.View(); !strings.Contains(view, "shown_to=0/0") {
		t.Errorf("view missing shown_to=0/0\n%s", view)
	}
}

// TestBoardModalMessagesNameWhoRetracted: with two verbs able to withdraw a
// message, a bare RETRACTED would credit the author for what an operator did.
// The list row and the content header must both say which check it passed.
func TestBoardModalMessagesNameWhoRetracted(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 30)
	msgs := []cli.BoardMessage{
		{
			Seq: 10, FromTaskHex: "11112222", ReceivedAtMs: 1_000,
			Retracted: true, RetractedAtMs: 2_000,
			RetractedBy: protocol.RetractedBy_Author, RetractedByTaskHex: "11112222",
		},
		{
			Seq: 20, FromTaskHex: "33334444", ReceivedAtMs: 1_000,
			Retracted: true, RetractedAtMs: 2_000,
			RetractedBy: protocol.RetractedBy_PurgeCap, RetractedByTaskHex: "99998888",
		},
		{
			Seq: 30, FromTaskHex: "55556666", ReceivedAtMs: 1_000,
			Retracted: true, RetractedAtMs: 2_000,
			RetractedBy: protocol.RetractedBy_PurgeCap,
		},
	}
	m.ApplyMessages("t.withdrawn", msgs, nil, true)

	view := m.View()
	for _, want := range []string{
		"RETRACTED by=author",
		"RETRACTED by=purge_cap:99998888",
		// No task id: an operator client holds its capabilities directly. The
		// row says so rather than printing an empty by=.
		"RETRACTED by=purge_cap:operator",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("message list missing %q\n%s", want, view)
		}
	}
	// The header of the selected message (seq 10) carries it too — that pane is
	// where the payload is read, so it must not lose the attribution.
	if !strings.Contains(view, "at=") || !strings.Contains(view, "by=author") {
		t.Errorf("content header missing the retract attribution\n%s", view)
	}
	// And the footer advertises the key that produces this state.
	if !strings.Contains(view, modalKeys.BoardRetractMsg+": retract msg") {
		t.Errorf("message-mode footer does not advertise the retract key\n%s", view)
	}
}

// TestBoardModal_ChainsUsesTheSharedRenderer pins that the TUI draws the CLI's
// output rather than a second rendering of the same rows. If this ever needs a
// TUI-specific expectation, that is the signal the two surfaces have drifted.
func TestBoardModal_ChainsUsesTheSharedRenderer(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	// Built through the real pipeline rather than by hand: production always
	// feeds ApplyChains the output of SelectThreads, which stamps the
	// conversation key, and a hand-made row set is a shape that never reaches
	// it. The first version of this test made rows directly and could not see
	// the section headers at all.
	topicOf := map[uint64]string{1: "chat.aaaa", 2: "chat.bbbb"}
	rows, err := cli.SelectThreads(cli.BuildThreads([]cli.BoardMessage{
		{Seq: 1, FromTaskHex: "aaaa1111", Payload: []byte("root")},
		{Seq: 2, InReplyTo: 1, FromTaskHex: "bbbb2222", Payload: []byte("reply")},
	}, topicOf), topicOf, cli.ThreadFilter{})
	if err != nil {
		t.Fatal(err)
	}
	m.ApplyChains(rows)

	// The list is where ApplyChains lands now; the renderer runs on the way
	// into ONE conversation, which is also the selection the destructive keys
	// act on.
	if m.Mode() != boardChainList {
		t.Fatalf("mode after ApplyChains = %v, want boardChainList", m.Mode())
	}
	m.OpenSelectedConversation()
	if m.Mode() != boardChains {
		t.Fatalf("mode = %v, want boardChains", m.Mode())
	}
	view := m.View()
	for _, want := range []string{"#1", "#2", "re=1", "topic=chat.aaaa", "topic=chat.bbbb"} {
		if !strings.Contains(view, want) {
			t.Errorf("chain detail missing %q:\n%s", want, view)
		}
	}
	// The conversation header. "The TUI inherits it because it draws the CLI's
	// spelling" is a claim, and this is what holds it: a surface that started
	// naming conversations its own way would drift silently.
	if !strings.Contains(view, "message(s)") {
		t.Errorf("chain detail lost the conversation header:\n%s", view)
	}

	// The window statement and the row count belong to the LIST -- it is the
	// view that answers "is this everything". Asserted by the statement's TAIL:
	// it is wrapped to the panel width, so the whole sentence never appears on
	// one line, and the tail is the part that was being truncated away before
	// it was wrapped.
	m.PopToChainList()
	list := m.View()
	if !strings.Contains(list, "ORPHAN marks a reply") {
		t.Errorf("chain list lost the end of the window statement:\n%s", list)
	}
	for _, want := range []string{"2 rows", "message(s)"} {
		if !strings.Contains(list, want) {
			t.Errorf("chain list missing %q:\n%s", want, list)
		}
	}
}

// TestBoardModal_ChainsEscapesBodies is the panel-integrity half: a body from an
// untrusted peer must not reach the viewport with a live control sequence, or
// it repaints over the border — the reason sanitizeOutput exists for the raw
// pane. RenderThreads cannot judge a viewport by its type, so ApplyChains says
// BodyEscaped explicitly; this is what would catch that line being dropped.
func TestBoardModal_ChainsEscapesBodies(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	m.ApplyChains([]cli.ThreadRow{
		{Msg: cli.BoardMessage{Seq: 1, Payload: []byte("clear:\x1b[2J bell:\x07")}, Topic: "chat.aaaa", Size: 18},
	})
	// The bodies are drawn in the detail, so that is where the escaping has to
	// hold.
	m.OpenSelectedConversation()
	view := m.View()
	for _, raw := range []string{"\x1b[2J", "\x07"} {
		if strings.Contains(view, raw) {
			t.Errorf("a raw control sequence %q reached the viewport:\n%q", raw, view)
		}
	}
	if !strings.Contains(view, `\x1b`) {
		t.Errorf("the escape is not visible either:\n%s", view)
	}
}

// chainFixture builds two conversations through the real pipeline, because
// SelectThreads is what stamps the conversation key and a hand-made ThreadRow
// is a shape production never produces.
func chainFixture(t *testing.T) []cli.ThreadRow {
	t.Helper()
	topicOf := map[uint64]string{
		1: "chat.aaaaaaaa", 2: "chat.bbbbbbbb",
		3: "chat.cccccccc", 4: "chat.dddddddd",
	}
	rows, err := cli.SelectThreads(cli.BuildThreads([]cli.BoardMessage{
		{Seq: 1, FromTaskHex: "bbbbbbbb", ReceivedAtMs: 100, Payload: []byte("A-side")},
		{Seq: 2, InReplyTo: 1, FromTaskHex: "aaaaaaaa", ReceivedAtMs: 200, Payload: []byte("A-reply")},
		{Seq: 3, FromTaskHex: "dddddddd", ReceivedAtMs: 300, Payload: []byte("B-side")},
		{Seq: 4, InReplyTo: 3, FromTaskHex: "cccccccc", ReceivedAtMs: 400, Payload: []byte("B-reply")},
	}, topicOf), topicOf, cli.ThreadFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestBoardModal_ThreadActionsScopeToTheHighlightedConversation is the
// surface-parity invariant, not presentation: no surface may offer a wider
// destructive form than the CLI allows. The CLI requires a selector; this list
// requires a selection, and what the key acts on must be the highlighted row
// and nothing else.
func TestBoardModal_ThreadActionsScopeToTheHighlightedConversation(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	m.ApplyChains(chainFixture(t))

	if len(m.chainConvs) != 2 {
		t.Fatalf("grouped into %d conversation(s), want 2", len(m.chainConvs))
	}
	first := m.SelectedConversationKey()
	if first == "" {
		t.Fatal("nothing selected after ApplyChains; the first row must be")
	}
	if first != m.chainConvs[0].Key {
		t.Errorf("selected %q, want the first row's %q", first, m.chainConvs[0].Key)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	second := m.SelectedConversationKey()
	if second != m.chainConvs[1].Key {
		t.Errorf("after ↓ selected %q, want the second row's %q", second, m.chainConvs[1].Key)
	}
	if second == first {
		t.Fatal("the cursor did not move; the fixture does not exercise the scoping")
	}

	// Past the end it stays put rather than wrapping onto the first one — a
	// destructive key must never act on a row the cursor appears to have left.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.SelectedConversationKey(); got != second {
		t.Errorf("cursor moved past the last row: %q", got)
	}
}

// TestBoardModal_NoConversationNoSelector: with nothing on the board the key
// has nothing to act on, and "" is what tells the caller to do nothing. A
// destructive verb reaching the server with no selector is the whole-board form
// the CLI declares unreachable.
func TestBoardModal_NoConversationNoSelector(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	m.ApplyChains(nil)

	if got := m.SelectedConversationKey(); got != "" {
		t.Errorf("SelectedConversationKey() = %q on an empty board, want \"\"", got)
	}
	// And the view says so rather than looking like a loading state.
	if !strings.Contains(m.View(), "nothing on the board") {
		t.Errorf("empty chain list does not say it is empty:\n%s", m.View())
	}
}

// TestBoardModal_ChainListAdvertisesItsKeys: the footer is the only place these
// two keys are discoverable, and they are the destructive pair.
func TestBoardModal_ChainListAdvertisesItsKeys(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.SetSize(120, 40)
	m.ApplyChains(chainFixture(t))
	view := m.View()
	for _, want := range []string{
		modalKeys.BoardRetractMsg + ": retract thread",
		modalKeys.BoardPurgeMsg + ": purge thread",
		"Enter: open",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("chain list footer does not advertise %q:\n%s", want, view)
		}
	}
}
