package agentskills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// A skill's files beyond SKILL.md are reachable through Files / File / Extract,
// which is the only way to them where no worktree received the skill.

func TestFilesListsTheWholeSkillDirectory(t *testing.T) {
	files, err := Files("running-parallel-workers")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SKILL.md", "references/questions.md", "scripts/q.py"} {
		found := false
		for _, f := range files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Files() = %v, missing %s", files, want)
		}
	}
}

func TestFilesFSFiltersLikeListFS(t *testing.T) {
	fsys := fstest.MapFS{
		"s/SKILL.md":      {Data: []byte("x")},
		"s/refs/a.md":     {Data: []byte("a")},
		"notaskill/x.txt": {Data: []byte("x")},
	}
	got, err := FilesFS(fsys, "s")
	if err != nil || strings.Join(got, ",") != "SKILL.md,refs/a.md" {
		t.Fatalf("FilesFS(s) = %v, %v", got, err)
	}
	if _, err := FilesFS(fsys, "notaskill"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("FilesFS(notaskill) err = %v, want ErrNotExist: a directory without SKILL.md is not a skill", err)
	}
}

func TestFileReadsOneAndRefusesEscapes(t *testing.T) {
	b, err := File("running-parallel-workers", "references/questions.md")
	if err != nil || !strings.HasPrefix(string(b), "# Owner questions") {
		t.Fatalf("File(references/questions.md) = %.40q, %v", b, err)
	}
	if b2, err := File("running-parallel-workers", `references\questions.md`); err != nil || string(b2) != string(b) {
		t.Errorf("a backslash path should name the same file, got err %v", err)
	}
	for _, bad := range []string{"../harness-cli/SKILL.md", "/etc/passwd", ".", "", "references/../../x"} {
		if _, err := File("running-parallel-workers", bad); err == nil {
			t.Errorf("File(%q) succeeded, want a refusal", bad)
		}
	}
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if _, err := File(bad, "SKILL.md"); err == nil {
			t.Errorf("File(name %q) succeeded, want a refusal", bad)
		}
	}
	if _, err := File("running-parallel-workers", "references/nope.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file err = %v, want ErrNotExist", err)
	}
}

func TestExtractWritesUnderNameAndRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	written, err := Extract("running-parallel-workers", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := Files("running-parallel-workers")
	if len(written) != len(files) {
		t.Fatalf("wrote %d files, the skill has %d", len(written), len(files))
	}
	got, err := os.ReadFile(filepath.Join(dir, "running-parallel-workers", "scripts", "q.py"))
	want, _ := File("running-parallel-workers", "scripts/q.py")
	if err != nil || string(got) != string(want) {
		t.Fatalf("extracted scripts/q.py differs from the embedded one (err %v)", err)
	}

	// A second extract without force writes nothing and names what exists.
	marker := filepath.Join(dir, "running-parallel-workers", "SKILL.md")
	if err := os.WriteFile(marker, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, err := Extract("running-parallel-workers", dir, false); err == nil || len(w) != 0 ||
		!strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("second Extract = %v, %v; want a refusal naming SKILL.md and nothing written", w, err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "local edit" {
		t.Fatal("a refused Extract overwrote an existing file")
	}
	if _, err := Extract("running-parallel-workers", dir, true); err != nil {
		t.Fatalf("Extract with force: %v", err)
	}
	if b, _ := os.ReadFile(marker); string(b) == "local edit" {
		t.Fatal("Extract with force did not replace the existing file")
	}
}
