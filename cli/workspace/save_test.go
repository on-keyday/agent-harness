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
