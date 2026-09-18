package cli

import (
	"reflect"
	"strings"
	"testing"
)

// The tests below mirror cli/tasktree_test.go's style and are the contract for
// BuildThreads: roots in seq order, siblings by seq, orphans surfaced at root
// with their own children kept, a cycle terminating, and every input message
// emitted exactly once.

// A chain whose parent is absent renders at root and keeps its children.
func TestBuildThreadsOrphanKeepsChildren(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 20, InReplyTo: 10}, // 10 is NOT in the input
		{Seq: 21, InReplyTo: 20},
	}
	rows := BuildThreads(msgs, map[uint64]string{20: "chat.a", 21: "chat.b"})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if !rows[0].Orphan || rows[0].Depth != 0 {
		t.Errorf("row 0 = {orphan:%v depth:%d}, want {true 0}", rows[0].Orphan, rows[0].Depth)
	}
	// Depth 0, not 1: the orphan has exactly one reply, so that reply is a
	// continuation rather than a fork. What this test is about is that the
	// child is KEPT and follows its parent — an orphan is re-rooted, never
	// pruned. See TestBuildThreadsDepthMarksForksNotReplies for the rule.
	if rows[1].Depth != 0 {
		t.Errorf("row 1 depth = %d, want 0: a lone reply is a continuation", rows[1].Depth)
	}
	if rows[1].Msg.Seq != 21 || rows[1].Msg.InReplyTo != 20 {
		t.Errorf("an orphan lost its own child: row 1 = %+v", rows[1].Msg)
	}
}

// A chain spans topics; the row carries where each message landed.
func TestBuildThreadsSpansTopics(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 1},
		{Seq: 2, InReplyTo: 1},
		{Seq: 3, InReplyTo: 2},
	}
	rows := BuildThreads(msgs, map[uint64]string{1: "chat.aaa", 2: "chat.bbb", 3: "chat.aaa"})
	got := []string{rows[0].Topic, rows[1].Topic, rows[2].Topic}
	want := []string{"chat.aaa", "chat.bbb", "chat.aaa"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("topics = %v, want %v", got, want)
	}
	// A linear chain is flat: each message has exactly one reply, so nothing
	// forks and nothing indents. The chain is still ordered and each row still
	// names its parent in InReplyTo.
	if rows[2].Depth != 0 {
		t.Errorf("depth = %d, want 0: a linear chain does not indent", rows[2].Depth)
	}
}

// Siblings order by Seq, not by arrival time, and the order is total.
func TestBuildThreadsSiblingOrderIsSeq(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 1},
		{Seq: 7, InReplyTo: 1, ReceivedAtMs: 500},
		{Seq: 3, InReplyTo: 1, ReceivedAtMs: 500}, // same ms, lower seq
	}
	rows := BuildThreads(msgs, nil)
	if rows[1].Msg.Seq != 3 || rows[2].Msg.Seq != 7 {
		t.Errorf("sibling order = %d,%d, want 3,7", rows[1].Msg.Seq, rows[2].Msg.Seq)
	}
}

// A malformed input set with a cycle must terminate and emit each message once.
func TestBuildThreadsCycleTerminates(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 1, InReplyTo: 2},
		{Seq: 2, InReplyTo: 1},
	}
	rows := BuildThreads(msgs, nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (each message exactly once)", len(rows))
	}
}

// Every input message comes back exactly once, roots in seq order.
func TestBuildThreadsTwoRootsInterleaved(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 10}, {Seq: 11, InReplyTo: 10}, {Seq: 5}, {Seq: 6, InReplyTo: 5},
	}
	rows := BuildThreads(msgs, nil)
	got := []uint64{rows[0].Msg.Seq, rows[1].Msg.Seq, rows[2].Msg.Seq, rows[3].Msg.Seq}
	want := []uint64{5, 6, 10, 11}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestBuildThreadsEmpty(t *testing.T) {
	if rows := BuildThreads(nil, nil); rows != nil {
		t.Errorf("rows = %v, want nil", rows)
	}
}

// IsLast drives the gutter: a root with three children must flag only the
// last of them, so the renderer draws ├─ ├─ └─ and not three └─.
func TestBuildThreadsIsLastFlagsOnlyTheLastChild(t *testing.T) {
	msgs := []BoardMessage{
		{Seq: 1},
		{Seq: 2, InReplyTo: 1},
		{Seq: 3, InReplyTo: 1},
		{Seq: 4, InReplyTo: 1},
	}
	rows := BuildThreads(msgs, nil)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	for i, want := range []int{0, 1, 1, 1} { // depths: root + 3 children
		if rows[i].Depth != want {
			t.Errorf("row %d depth = %d, want %d", i, rows[i].Depth, want)
		}
	}
	for i, want := range [][]bool{nil, {false}, {false}, {true}} {
		got := rows[i].IsLast
		if want == nil {
			if len(got) != 0 {
				t.Errorf("row %d (root) IsLast = %v, want empty", i, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("row %d (seq %d) IsLast = %v, want %v", i, rows[i].Msg.Seq, got, want)
		}
	}
}

// TestBuildThreadsSizeIsPopulatedIncludingEmpty pins the field that replaced a
// zero-means-unset sentinel. An empty publish is a real case here (`agent send`
// reporting bytes: 0), so size 0 must be a value the renderer prints, not a
// marker meaning "ask the payload instead".
func TestBuildThreadsSizeIsPopulated(t *testing.T) {
	rows := BuildThreads([]BoardMessage{
		{Seq: 1, Payload: []byte("seven!!")},
		{Seq: 2, InReplyTo: 1, Payload: []byte{}},
		{Seq: 3, InReplyTo: 99, Payload: []byte("orphan")},
	}, nil)
	want := map[uint64]int{1: 7, 2: 0, 3: 6}
	for _, r := range rows {
		if got := r.Size; got != want[r.Msg.Seq] {
			t.Errorf("seq %d: Size = %d, want %d", r.Msg.Seq, got, want[r.Msg.Seq])
		}
	}
}

// TestBuildThreadsDepthMarksForksNotReplies pins what the gutter means. An
// earlier version indented per reply, which measured 17 levels deep on a real
// 31-message exchange that contained no forks at all.
func TestBuildThreadsDepthMarksForksNotReplies(t *testing.T) {
	// 1 -> 2 -> 3 is linear; 3 is answered TWICE, by 4 and 5.
	rows := BuildThreads([]BoardMessage{
		{Seq: 1},
		{Seq: 2, InReplyTo: 1},
		{Seq: 3, InReplyTo: 2},
		{Seq: 4, InReplyTo: 3},
		{Seq: 5, InReplyTo: 3},
	}, nil)
	want := map[uint64]int{1: 0, 2: 0, 3: 0, 4: 1, 5: 1}
	for _, r := range rows {
		if got := r.Depth; got != want[r.Msg.Seq] {
			t.Errorf("seq %d: depth = %d, want %d", r.Msg.Seq, got, want[r.Msg.Seq])
		}
	}
	// The two forked replies get a gutter; the linear run gets none.
	byseq := map[uint64]ThreadRow{}
	for _, r := range rows {
		byseq[r.Msg.Seq] = r
	}
	if g := TreePrefix(byseq[2].IsLast); g != "" {
		t.Errorf("a continuation drew a gutter: %q", g)
	}
	if g := TreePrefix(byseq[4].IsLast); g != "├─ " {
		t.Errorf("first forked reply gutter = %q, want %q", g, "├─ ")
	}
	if g := TreePrefix(byseq[5].IsLast); g != "└─ " {
		t.Errorf("last forked reply gutter = %q, want %q", g, "└─ ")
	}
}

// msg is a terse BoardMessage for the grouping tests: seq, parent, sender, ms.
func msg(seq, parent uint64, from string, ms uint64) BoardMessage {
	return BoardMessage{Seq: seq, InReplyTo: parent, FromTaskHex: from, ReceivedAtMs: ms}
}

func group(t *testing.T, msgs []BoardMessage, topicOf map[uint64]string) []ThreadRow {
	t.Helper()
	rows, err := SelectThreads(BuildThreads(msgs, topicOf), topicOf, ThreadFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestConversationGrouping_TheMeasuredShape is the fixture the design spec was
// written from: a two-party exchange five minutes long, on a live board, came
// out as EIGHT messages in FOUR chains — two of them single unanswered status
// messages, one a message sent with --topic rather than --in-reply-to. All of
// it is one conversation.
func TestConversationGrouping_TheMeasuredShape(t *testing.T) {
	a, b := "70fbad4a", "c96af19d"
	topicOf := map[uint64]string{
		1: "chat." + b, 2: "chat." + a, 3: "chat." + a,
		4: "chat." + b, 5: "chat." + a, 6: "chat." + a, 7: "chat." + a, 8: "chat." + b,
	}
	rows := group(t, []BoardMessage{
		msg(1, 0, a, 100), // claude → pi
		msg(2, 1, b, 110), // pi replies
		msg(3, 0, b, 120), // pi, unanswered: its own chain
		msg(4, 0, a, 130), // claude, sent with --topic: its own chain
		msg(5, 4, b, 140), // pi, fork branch one
		msg(6, 4, b, 150), // pi, fork branch two
		msg(7, 0, b, 160), // pi, unanswered: its own chain
		msg(8, 6, a, 170), // claude replies to branch two
	}, topicOf)

	if len(rows) != 8 {
		t.Fatalf("rows = %d, want 8", len(rows))
	}
	keys := map[string]int{}
	for _, r := range rows {
		keys[r.Conversation]++
	}
	if len(keys) != 1 {
		t.Fatalf("conversations = %d, want 1: %v", len(keys), keys)
	}
	if _, ok := keys["70fbad4a+c96af19d"]; !ok {
		t.Errorf("conversation key = %v, want 70fbad4a+c96af19d", keys)
	}
}

// TestConversationGrouping_SeparatesPeers: a supervisor talking to two workers
// is two conversations, not one, even though it is in both.
func TestConversationGrouping_SeparatesPeers(t *testing.T) {
	sup, w1, w2 := "aaaaaaaa", "bbbbbbbb", "cccccccc"
	topicOf := map[uint64]string{
		1: "chat." + w1, 2: "chat." + sup,
		3: "chat." + w2, 4: "chat." + sup,
	}
	rows := group(t, []BoardMessage{
		msg(1, 0, sup, 100), msg(2, 1, w1, 110),
		msg(3, 0, sup, 120), msg(4, 3, w2, 130),
	}, topicOf)

	got := map[string][]uint64{}
	for _, r := range rows {
		got[r.Conversation] = append(got[r.Conversation], r.Msg.Seq)
	}
	want := map[string][]uint64{
		"aaaaaaaa+bbbbbbbb": {1, 2},
		"aaaaaaaa+cccccccc": {3, 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("grouping = %v, want %v", got, want)
	}
}

// TestConversationGrouping_NamedTopicIsTheRoom: a chain that touched a topic
// which is not a chat.<id> keys on that topic — the sender declared a subject
// with --reply-to, and that is the unit.
func TestConversationGrouping_NamedTopicIsTheRoom(t *testing.T) {
	a, b := "70fbad4a", "c96af19d"
	topicOf := map[uint64]string{1: "chat." + b, 2: "rr.dec-019"}
	rows := group(t, []BoardMessage{msg(1, 0, a, 100), msg(2, 1, b, 110)}, topicOf)
	for _, r := range rows {
		if r.Conversation != "rr.dec-019" {
			t.Errorf("seq %d: conversation = %q, want rr.dec-019", r.Msg.Seq, r.Conversation)
		}
	}
}

// TestConversationGrouping_ReceiveOnlyPartyCounts: a party that never sent is
// still a participant — it owns the topic the message landed on. Dropping it
// would key a one-way exchange as if it had one participant.
func TestConversationGrouping_ReceiveOnlyPartyCounts(t *testing.T) {
	a, b := "70fbad4a", "c96af19d"
	topicOf := map[uint64]string{1: "chat." + b}
	rows := group(t, []BoardMessage{msg(1, 0, a, 100)}, topicOf)
	if rows[0].Conversation != "70fbad4a+c96af19d" {
		t.Errorf("conversation = %q, want both parties even though %s never sent",
			rows[0].Conversation, b)
	}
	if h := ConversationHeader(rows); !strings.Contains(h, b) {
		t.Errorf("header %q omits the party that only received", h)
	}
}

// TestConversationGrouping_OrderIsMostRecentLast: sections follow their newest
// message, not their first — a conversation that started early and is still
// going belongs at the bottom, where a transcript's freshest lines are.
func TestConversationGrouping_OrderIsMostRecentLast(t *testing.T) {
	a, b, c := "aaaaaaaa", "bbbbbbbb", "cccccccc"
	topicOf := map[uint64]string{1: "chat." + b, 2: "chat." + c, 3: "chat." + b}
	rows := group(t, []BoardMessage{
		msg(1, 0, a, 100), // a↔b starts first
		msg(2, 0, a, 200), // a↔c is one message, in the middle
		msg(3, 0, a, 300), // a↔b speaks again, most recently
	}, topicOf)
	var order []string
	for _, r := range rows {
		if len(order) == 0 || order[len(order)-1] != r.Conversation {
			order = append(order, r.Conversation)
		}
	}
	want := []string{"aaaaaaaa+cccccccc", "aaaaaaaa+bbbbbbbb"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("section order = %v, want %v (oldest activity first)", order, want)
	}
}
