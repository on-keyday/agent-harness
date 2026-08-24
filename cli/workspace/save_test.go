package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetPreservesEveryOtherLine(t *testing.T) {
	src := `# a comment nobody should lose

[workspace default]
repo = /old

# an introduction to the phone workspace
[workspace phone]
repo    = /keep/me
ws-path = /ws
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	f.Set(&Workspace{
		Name: "default",
		Repo: "/new",
		Tasks: []Task{{
			ID: "3f2a9c00000000000000000000000001", Resume: ResumeContinue,
			Runner: RunnerAssigned, Forwards: []string{"-L 3000:127.0.0.1:3000"},
		}},
	})
	out := string(f.Render())

	for _, keep := range []string{
		"# a comment nobody should lose",
		"# an introduction to the phone workspace",
		"repo    = /keep/me",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	if strings.Contains(out, "/old") {
		t.Errorf("the replaced workspace's old value survived:\n%s", out)
	}

	reparsed, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("the rendered file does not parse: %v", err)
	}
	ws, ok := reparsed.Workspace("default")
	if !ok || ws.Repo != "/new" || len(ws.Tasks) != 1 || ws.Tasks[0].Forwards[0] != "-L 3000:127.0.0.1:3000" {
		t.Errorf("round-trip lost the written workspace: %+v", ws)
	}
	if phone, ok := reparsed.Workspace("phone"); !ok || phone.Repo != "/keep/me" {
		t.Errorf("round-trip lost the other workspace: %+v", phone)
	}
}

// Replacing a SHORTER block with a longer one (and the reverse) must keep the
// following workspaces' spans correct, or a second Set writes into the wrong
// lines.
func TestSetTwiceInARow(t *testing.T) {
	src := "[workspace a]\nrepo = /a\n\n[workspace b]\nrepo = /b\n"
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	f.Set(&Workspace{Name: "a", Repo: "/a2", Tasks: []Task{
		{ID: "3f2a9c00000000000000000000000001", Forwards: []string{"-L 1:127.0.0.1:1"}},
		{ID: "7b1e000000000000000000000000000f", Forwards: []string{"-R 2:127.0.0.1:2"}},
	}})
	f.Set(&Workspace{Name: "b", Repo: "/b2"})

	reparsed, err := Parse(strings.NewReader(string(f.Render())))
	if err != nil {
		t.Fatalf("does not parse after two Sets: %v\n%s", err, f.Render())
	}
	a, _ := reparsed.Workspace("a")
	if a == nil || a.Repo != "/a2" || len(a.Tasks) != 2 {
		t.Errorf("workspace a = %+v", a)
	}
	b, _ := reparsed.Workspace("b")
	if b == nil || b.Repo != "/b2" {
		t.Errorf("workspace b = %+v", b)
	}
	// Shrinking a back down must not strip b either.
	f.Set(&Workspace{Name: "a", Repo: "/a3"})
	reparsed, err = Parse(strings.NewReader(string(f.Render())))
	if err != nil {
		t.Fatalf("does not parse after shrinking: %v\n%s", err, f.Render())
	}
	if b, _ := reparsed.Workspace("b"); b == nil || b.Repo != "/b2" {
		t.Errorf("shrinking a damaged b: %+v", b)
	}
}

func TestSetAppendsWhenAbsent(t *testing.T) {
	f := New()
	f.Set(&Workspace{Name: "default", ServerCID: "ws:example.invalid:8539-*"})
	reparsed, err := Parse(strings.NewReader(string(f.Render())))
	if err != nil {
		t.Fatalf("Parse of a fresh file: %v", err)
	}
	if _, ok := reparsed.Workspace("default"); !ok {
		t.Error("Set on an empty file did not add the workspace")
	}
}

// An empty value must not be written as a bare `key =`, which would parse back
// as an explicit empty value rather than as an absent key.
func TestSetOmitsEmptyKeys(t *testing.T) {
	f := New()
	f.Set(&Workspace{Name: "w"})
	out := string(f.Render())
	for _, absent := range []string{"server-cid", "ws-path", "repo", "grid"} {
		if strings.Contains(out, absent) {
			t.Errorf("empty %s was written:\n%s", absent, out)
		}
	}
}

// GridSet with an empty value is a real selection (the unnarrowed grid) and
// must survive the round trip as one.
func TestSetWritesAnEmptyGridWhenSet(t *testing.T) {
	f := New()
	f.Set(&Workspace{Name: "w", GridSet: true})
	reparsed, err := Parse(strings.NewReader(string(f.Render())))
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := reparsed.Workspace("w")
	if ws == nil || !ws.GridSet || ws.Grid != "" {
		t.Errorf("grid presence lost: %+v", ws)
	}
}

func TestSaveCreatesParentDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".harness", "config")
	f := New()
	f.Set(&Workspace{Name: "default"})
	if err := f.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("Save did not create %s: %v", p, err)
	}
}

func TestBlockMatchesWhatSetWrites(t *testing.T) {
	ws := &Workspace{Name: "w", Repo: "/x", Tasks: []Task{
		{ID: "3f2a9c00000000000000000000000001", Resume: ResumeFresh, Runner: RunnerAny},
	}}
	f := New()
	f.Set(ws)
	if !strings.Contains(string(f.Render()), strings.TrimRight(string(Block(ws)), "\n")) {
		t.Errorf("Block and Set disagree:\n--- Block ---\n%s\n--- file ---\n%s", Block(ws), f.Render())
	}
}

// A save must not delete task blocks it never looked at, and must not reset the
// resume / runner an operator hand-edited. Both were real: saving one task wiped
// its siblings and rewrote their policy to the defaults.
func TestMergeKeepsUnobservedBlocksAndHandEditedPolicy(t *testing.T) {
	existing, err := Parse(strings.NewReader(`[workspace default]
repo = /abs/repo

[workspace default task 3f2a9c00000000000000000000000001]
resume  = fresh
runner  = any
forward = -L 3000:127.0.0.1:3000

[workspace default task 7b1e000000000000000000000000000f]
resume  = continue
runner  = assigned
`))
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := existing.Workspace("default")

	// What a save of ONLY the first task observes.
	observed := &Workspace{Name: "default", Repo: "/abs/repo", Tasks: []Task{{
		ID: "3f2a9c00000000000000000000000001", Resume: ResumeContinue, Runner: RunnerAssigned,
		Forwards: []string{"-L 3000:127.0.0.1:3000", "-R 8080:127.0.0.1:8080"},
	}}}
	got := Merge(ex, observed, map[string]bool{"3f2a9c00000000000000000000000001": true})

	if len(got.Tasks) != 2 {
		t.Fatalf("len(Tasks) = %d, want 2 — the unobserved sibling must survive", len(got.Tasks))
	}
	if got.Tasks[0].Resume != ResumeFresh || got.Tasks[0].Runner != RunnerAny {
		t.Errorf("hand-edited policy was reset: resume=%q runner=%q", got.Tasks[0].Resume, got.Tasks[0].Runner)
	}
	if len(got.Tasks[0].Forwards) != 2 {
		t.Errorf("observed forwards were not updated: %q", got.Tasks[0].Forwards)
	}
	if got.Tasks[1].ID != "7b1e000000000000000000000000000f" || len(got.Tasks[1].Forwards) != 0 {
		t.Errorf("the unobserved block changed: %+v", got.Tasks[1])
	}
}

// An OBSERVED task with no live forwards has its forward lines cleared — that is
// what "save what is running now" means for a forward the operator stopped.
func TestMergeClearsForwardsOfAnObservedTask(t *testing.T) {
	existing, err := Parse(strings.NewReader(
		"[workspace default]\n[workspace default task 3f2a9c00000000000000000000000001]\nforward = -L 3000:127.0.0.1:3000\n"))
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := existing.Workspace("default")
	got := Merge(ex, &Workspace{Name: "default"}, map[string]bool{"3f2a9c00000000000000000000000001": true})
	if len(got.Tasks) != 1 || len(got.Tasks[0].Forwards) != 0 {
		t.Errorf("an observed task's stopped forward was not cleared: %+v", got.Tasks)
	}
}

// A first save has no existing workspace to merge with.
func TestMergeWithNoExistingWorkspace(t *testing.T) {
	got := Merge(nil, &Workspace{Name: "w", ServerCID: "ws:x-1", Tasks: []Task{{ID: "3f2a9c00000000000000000000000001"}}},
		map[string]bool{"3f2a9c00000000000000000000000001": true})
	if got.ServerCID != "ws:x-1" || len(got.Tasks) != 1 {
		t.Errorf("first save lost its own content: %+v", got)
	}
}

func TestRemove(t *testing.T) {
	src := `# keep me

[workspace a]
repo = /a

[workspace b]
repo = /b

[workspace b task 3f2a9c00000000000000000000000001]
resume = fresh
`
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if f.Remove("nosuch") {
		t.Error("Remove reported success for a workspace that is not there")
	}
	if !f.Remove("b") {
		t.Fatal("Remove(b) reported failure")
	}
	out := string(f.Render())
	if strings.Contains(out, "workspace b") || strings.Contains(out, "3f2a9c") {
		t.Errorf("workspace b survived:\n%s", out)
	}
	if !strings.Contains(out, "# keep me") || !strings.Contains(out, "repo = /a") {
		t.Errorf("removing b damaged the rest:\n%s", out)
	}
	reparsed, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("the file does not parse after Remove: %v\n%s", err, out)
	}
	if names := reparsed.Names(); len(names) != 1 || names[0] != "a" {
		t.Errorf("Names() = %q, want [a]", names)
	}
	// Removing the first one must keep the spans of what follows correct.
	if !f.Remove("a") {
		t.Fatal("Remove(a) reported failure")
	}
	if _, err := Parse(strings.NewReader(string(f.Render()))); err != nil {
		t.Fatalf("does not parse after removing both: %v", err)
	}
}
