package cli

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// mkTask builds a TaskInfo with the two fields the tree cares about plus a
// creation time for sibling ordering. id/parent are single bytes so the tests
// read as a shape, not as hex noise.
func mkTask(id, parent byte, createdAt uint64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = id
	t.CreatorTaskId.Id[0] = parent // 0 = operator-created root
	t.CreatedAt = createdAt
	return t
}

// shape renders the tree as "depth:id" lines so a test asserts ORDER and DEPTH
// together — the two things a reader of the tree actually consumes.
func shape(rows []TaskTreeRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteByte(byte('0' + r.Depth))
		b.WriteByte(':')
		b.WriteByte(r.Task.Id.Id[0])
		if r.Orphan {
			b.WriteString("(orphan)")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestBuildTaskTree_NestsChildrenUnderTheirCreator(t *testing.T) {
	// 'a' is operator-created; 'b' and 'c' are its children; 'd' is b's child.
	got := shape(BuildTaskTree([]protocol.TaskInfo{
		mkTask('d', 'b', 40),
		mkTask('b', 'a', 20),
		mkTask('a', 0, 10),
		mkTask('c', 'a', 30),
	}))
	want := "0:a\n1:b\n2:d\n1:c\n"
	if got != want {
		t.Errorf("tree shape:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestBuildTaskTree_SiblingsOrderByCreationThenID(t *testing.T) {
	got := shape(BuildTaskTree([]protocol.TaskInfo{
		mkTask('z', 'a', 20),
		mkTask('a', 0, 10),
		mkTask('y', 'a', 20), // same instant as z: id breaks the tie
		mkTask('m', 'a', 15),
	}))
	want := "0:a\n1:m\n1:y\n1:z\n"
	if got != want {
		t.Errorf("sibling order:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestBuildTaskTree_OrphanSurfacesAtRoot(t *testing.T) {
	// 'b' names a parent that is not in the visible set (pruned, or out of
	// scope). Hiding it would make a task disappear from a listing that is
	// supposed to show everything the caller can see.
	rows := BuildTaskTree([]protocol.TaskInfo{
		mkTask('b', 'x', 20),
		mkTask('a', 0, 10),
	})
	if got, want := shape(rows), "0:a\n0:b(orphan)\n"; got != want {
		t.Errorf("orphan handling:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestBuildTaskTree_IsLastDrivesTheConnectors(t *testing.T) {
	// IsLast[d] answers "is this row the last child at depth d", which is what
	// decides └─ vs ├─ and whether an ancestor column keeps drawing │.
	rows := BuildTaskTree([]protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('d', 'b', 30),
		mkTask('c', 'a', 40),
	})
	byID := map[byte][]bool{}
	for _, r := range rows {
		byID[r.Task.Id.Id[0]] = r.IsLast
	}
	// b is a's first of two children; d is b's only child; c is a's last.
	for _, tc := range []struct {
		id   byte
		want []bool
	}{
		{'a', []bool{}},
		{'b', []bool{false}},
		{'d', []bool{false, true}},
		{'c', []bool{true}},
	} {
		got := byID[tc.id]
		if len(got) != len(tc.want) {
			t.Errorf("%c: IsLast = %v, want %v", tc.id, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%c: IsLast = %v, want %v", tc.id, got, tc.want)
				break
			}
		}
	}
}

func TestBuildTaskTree_EveryTaskAppearsExactlyOnce(t *testing.T) {
	// The property a listing must not break: a tree view is a re-ORDERING, not
	// a filter. Includes a defensive cycle (a<-b, b<-a), which the server
	// refuses with SetParentStatus.would_cycle but a broken WAL replay could
	// still hand us — it must not hang or drop rows.
	in := []protocol.TaskInfo{
		mkTask('a', 'b', 10),
		mkTask('b', 'a', 20),
		mkTask('c', 0, 30),
	}
	rows := BuildTaskTree(in)
	if len(rows) != len(in) {
		t.Fatalf("got %d rows for %d tasks: %s", len(rows), len(in), shape(rows))
	}
	seen := map[byte]int{}
	for _, r := range rows {
		seen[r.Task.Id.Id[0]]++
	}
	for _, id := range []byte{'a', 'b', 'c'} {
		if seen[id] != 1 {
			t.Errorf("task %c appears %d times, want exactly 1: %s", id, seen[id], shape(rows))
		}
	}
}

func TestBuildTaskTree_EmptyInput(t *testing.T) {
	if rows := BuildTaskTree(nil); len(rows) != 0 {
		t.Errorf("nil input produced %d rows", len(rows))
	}
}

// hexOf renders the id mkTask built, so a test can name an anchor the way
// every caller does: a full 32-char task-id hex.
func hexOf(id byte) string {
	var t protocol.TaskID
	t.Id[0] = id
	return hex.EncodeToString(t.Id[:])
}

// ids renders a task slice as its single-byte ids, matching shape()'s idiom.
func ids(ts []protocol.TaskInfo) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteByte(t.Id.Id[0])
	}
	return b.String()
}

func TestTaskSubtree_AnchorPlusEveryDescendant(t *testing.T) {
	// a ├─ b ─ d
	//   └─ c        and an unrelated root e.
	in := []protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
		mkTask('d', 'b', 30),
		mkTask('c', 'a', 40),
		mkTask('e', 0, 50),
	}
	if got, want := ids(TaskSubtree(in, hexOf('a'))), "abdc"; got != want {
		t.Errorf("subtree of a = %q, want %q", got, want)
	}
	// From b: itself and d only — a's other branch and the unrelated root are
	// not below it.
	if got, want := ids(TaskSubtree(in, hexOf('b'))), "bd"; got != want {
		t.Errorf("subtree of b = %q, want %q", got, want)
	}
}

func TestTaskSubtree_LeafIsJustItself(t *testing.T) {
	in := []protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
	}
	if got, want := ids(TaskSubtree(in, hexOf('b'))), "b"; got != want {
		t.Errorf("subtree of leaf b = %q, want %q", got, want)
	}
}

func TestTaskSubtree_UnknownAnchorIsEmpty(t *testing.T) {
	// Empty, not "everything": a caller that mistyped an id must see nothing
	// happen and be told, never get the unfiltered set back.
	in := []protocol.TaskInfo{mkTask('a', 0, 10)}
	if got := TaskSubtree(in, hexOf('z')); len(got) != 0 {
		t.Errorf("unknown anchor returned %q, want empty", ids(got))
	}
	if got := TaskSubtree(in, ""); len(got) != 0 {
		t.Errorf("empty anchor returned %q, want empty", ids(got))
	}
	if got := TaskSubtree(nil, hexOf('a')); len(got) != 0 {
		t.Errorf("nil input returned %q, want empty", ids(got))
	}
}

func TestTaskSubtree_OrphanAnchorKeepsItsDescendants(t *testing.T) {
	// b's creator is not in the visible set, so BuildTaskTree re-roots it as an
	// orphan. Its own children are still its children — anchoring on a task
	// whose parent is out of scope must not come back empty-handed.
	in := []protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'x', 20), // creator 'x' not present
		mkTask('c', 'b', 30),
	}
	if got, want := ids(TaskSubtree(in, hexOf('b'))), "bc"; got != want {
		t.Errorf("subtree of orphan b = %q, want %q", got, want)
	}
}

func TestTaskSubtree_CycleMemberTerminates(t *testing.T) {
	// a<-b, b<-a: the server refuses to build this (would_cycle) but a broken
	// WAL replay could. BuildTaskTree parks cycle members at the end as depth-0
	// rows, so a subtree query must return the anchor alone rather than hang or
	// swallow the row after it.
	in := []protocol.TaskInfo{
		mkTask('a', 'b', 10),
		mkTask('b', 'a', 20),
		mkTask('c', 0, 30),
	}
	if got, want := ids(TaskSubtree(in, hexOf('a'))), "a"; got != want {
		t.Errorf("subtree of cycle member a = %q, want %q", got, want)
	}
}

func TestTaskSubtree_AnchorHexIsCaseAndSpaceInsensitive(t *testing.T) {
	// Ids reach this from a WebUI command line and a pasted id as readily as
	// from a table row, so normalise the way ParseScope does.
	in := []protocol.TaskInfo{
		mkTask('a', 0, 10),
		mkTask('b', 'a', 20),
	}
	if got, want := ids(TaskSubtree(in, "  "+strings.ToUpper(hexOf('a'))+" ")), "ab"; got != want {
		t.Errorf("subtree of padded upper-case anchor = %q, want %q", got, want)
	}
}

func TestTreePrefix_DrawsAncestorGutters(t *testing.T) {
	// A row two levels deep whose grandparent still has siblings below it keeps
	// a │ in the outer column; one whose grandparent was the last child leaves
	// that column blank. Getting this wrong is what makes a text tree look
	// like it forked somewhere it did not.
	for _, tc := range []struct {
		name   string
		isLast []bool
		want   string
	}{
		{"root", nil, ""},
		{"first of several", []bool{false}, "├─ "},
		{"last child", []bool{true}, "└─ "},
		{"deep, ancestor continues", []bool{false, true}, "│  └─ "},
		{"deep, ancestor finished", []bool{true, false}, "   ├─ "},
	} {
		if got := TreePrefix(TaskTreeRow{IsLast: tc.isLast}); got != tc.want {
			t.Errorf("%s: TreePrefix(%v) = %q, want %q", tc.name, tc.isLast, got, tc.want)
		}
	}
}
