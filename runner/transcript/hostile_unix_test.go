//go:build unix

package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sandboxed agent can write its own store (agy mounts ~/.gemini, claude its
// project directory). An id it plants or a directory it swaps for a symlink
// must not make a listing reach outside the store: neither names nor sizes of
// files there, nor a walk of the host (final review of pillar B, Important 1).

func TestAgyHostileIdsAreNotSessions(t *testing.T) {
	f := newAgyFixture(t)
	writeAgy(t, filepath.Join(f.home, "host-secret.txt"), "SECRET")
	db := filepath.Join(f.root, "conversation_summaries.db")
	ws := `'["` + fileURI(f.dir) + `"]'`
	sqlExec(t, db, `INSERT INTO conversation_summaries (conversation_id, last_modified_time, workspace_uris, last_user_input_time) VALUES `+
		`('..', '2026-11-13 00:00:00+00:00', `+ws+`, '2026-11-13 00:00:00+00:00'),`+
		`('../../..', '2026-11-13 00:00:00+00:00', `+ws+`, '2026-11-13 00:00:00+00:00'),`+
		`('a/b', '2026-11-13 00:00:00+00:00', `+ws+`, '2026-11-13 00:00:00+00:00');`)
	st := f.store(t)
	ss, err := st.Sessions(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(ss), ","); got != "A" {
		t.Fatalf("sessions %q, want A alone", got)
	}
	for _, id := range []string{"..", "../../..", "a/b"} {
		if _, err := st.Files(f.dir, id); !errors.Is(err, ErrNoSession) {
			t.Fatalf("files of %q: %v, want ErrNoSession", id, err)
		}
	}
}

func TestAgySymlinkedBrainIsNotWalked(t *testing.T) {
	f := newAgyFixture(t)
	outside := t.TempDir()
	writeAgy(t, filepath.Join(outside, "A", "host-only.txt"), "HOST")
	brain := filepath.Join(f.root, "brain")
	if err := os.RemoveAll(brain); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, brain); err != nil {
		t.Fatal(err)
	}
	fs, err := f.store(t).Files(f.dir, "A")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range fs {
		if strings.HasPrefix(x.Name, "brain/") {
			t.Fatalf("listed %q through a symlinked brain/", x.Name)
		}
	}
}

func TestClaudeHostileIdAndSymlinkedCompanions(t *testing.T) {
	root := t.TempDir()
	dir := "/w/repo/.harness-worktrees/aaaa"
	projects := filepath.Join(root, "projects")
	proj := filepath.Join(projects, ClaudeProjectDirName(dir))
	line := `{"type":"user","cwd":"` + dir + `","timestamp":"2026-11-14T01:02:03.000Z"}` + "\n"
	write(t, filepath.Join(projects, "-other-project", "victim.jsonl"), line)
	write(t, filepath.Join(proj, "...jsonl"), line) // id ".."
	write(t, filepath.Join(proj, "s1.jsonl"), line)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "host-only.jsonl"), "HOST")
	if err := os.MkdirAll(filepath.Join(proj, "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(proj, "s1", "subagents")); err != nil {
		t.Fatal(err)
	}
	st, err := New("claude", env(map[string]string{"CLAUDE_CONFIG_DIR": root}))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := st.Sessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(ss), ","); got != "s1" {
		t.Fatalf("sessions %q, want s1 alone", got)
	}
	fs, err := st.Files(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(fs), ","); got != "s1.jsonl" {
		t.Fatalf("files %q, want s1.jsonl alone (nothing through the symlink)", got)
	}
}
