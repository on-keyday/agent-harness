package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// treeTask builds a task with the fields renderListTree actually prints.
func treeTask(id, parent byte, createdAt uint64, prompt string) protocol.TaskInfo {
	t := mkTask(id, parent, createdAt)
	t.Status = protocol.TaskStatus_Running
	t.Kind = protocol.TaskKind_Oneshot
	t.SetRepoPath([]byte("/repo"))
	t.SetPrompt([]byte(prompt))
	return t
}

func renderTreeString(tasks []protocol.TaskInfo) string {
	var b bytes.Buffer
	renderListTree(&protocol.ListResultBody{Tasks: tasks}, &b)
	return b.String()
}

func TestRenderListTree_DrawsGuttersAndDropsRedundantBy(t *testing.T) {
	out := renderTreeString([]protocol.TaskInfo{
		treeTask('a', 0, 10, "root"),
		treeTask('b', 'a', 20, "child-1"),
		treeTask('c', 'a', 30, "child-2"),
	})
	if !strings.Contains(out, "TASKS (by creator)") {
		t.Errorf("missing tree header:\n%s", out)
	}
	for _, want := range []string{"├─ ", "└─ "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing connector %q:\n%s", want, out)
		}
	}
	// by= is what the gutter replaces; leaving it would repeat the parent on
	// every row of a view whose whole point is showing it once.
	if strings.Contains(out, "by=") {
		t.Errorf("tree rows still carry by=:\n%s", out)
	}
	// Dropping by= must not eat a neighbouring column.
	for _, want := range []string{"repo=/repo", "caps=", "prompt="} {
		if !strings.Contains(out, want) {
			t.Errorf("column %q lost when by= was stripped:\n%s", want, out)
		}
	}
}

func TestRenderListTree_ShowsOrphansRatherThanHidingThem(t *testing.T) {
	out := renderTreeString([]protocol.TaskInfo{
		treeTask('a', 0, 10, "root"),
		treeTask('b', 'x', 20, "parent-was-pruned"),
	})
	if !strings.Contains(out, "orphan") {
		t.Errorf("orphan not marked:\n%s", out)
	}
	if !strings.Contains(out, "parent-was-pruned") {
		t.Errorf("orphan row missing entirely — a tree view must not filter:\n%s", out)
	}
}

// TestRenderListTree_ShowsEveryTaskTheFlatViewDoes is the invariant that keeps
// --tree honest: switching views may reorder rows, never lose one.
func TestRenderListTree_ShowsEveryTaskTheFlatViewDoes(t *testing.T) {
	tasks := []protocol.TaskInfo{
		treeTask('a', 0, 10, "p-root"),
		treeTask('b', 'a', 20, "p-child"),
		treeTask('c', 'x', 30, "p-orphan"),
		treeTask('d', 0, 40, "p-second-root"),
	}
	var flat bytes.Buffer
	renderList(&protocol.ListResultBody{Tasks: tasks}, &flat)
	tree := renderTreeString(tasks)
	for _, p := range []string{"p-root", "p-child", "p-orphan", "p-second-root"} {
		if !strings.Contains(flat.String(), p) {
			t.Fatalf("test bug: %q missing from the flat view", p)
		}
		if !strings.Contains(tree, p) {
			t.Errorf("%q present in the flat view but missing from --tree", p)
		}
	}
}
