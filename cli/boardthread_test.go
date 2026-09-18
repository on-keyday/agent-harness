package cli

import (
	"reflect"
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
	if rows[1].Depth != 1 {
		t.Errorf("row 1 depth = %d, want 1: an orphan keeps its own children", rows[1].Depth)
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
	if rows[2].Depth != 2 {
		t.Errorf("depth = %d, want 2", rows[2].Depth)
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
