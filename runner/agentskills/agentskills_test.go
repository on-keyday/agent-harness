package agentskills

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDescription(t *testing.T) {
	// Every listed skill must expose a non-empty frontmatter description.
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	for _, n := range names {
		d, err := Description(n)
		if err != nil {
			t.Errorf("Description(%q): %v", n, err)
			continue
		}
		if d == "" {
			t.Errorf("skill %q has an empty description", n)
		}
	}
	// A skill without frontmatter yields "" (not an error path we can hit via
	// real skills, so exercise the parser directly).
	if got := frontmatterField([]byte("# no frontmatter\n"), "description"); got != "" {
		t.Errorf("frontmatterField(no frontmatter) = %q, want empty", got)
	}
	if got := frontmatterField([]byte("---\ndescription: hi there\n---\n"), "description"); got != "hi there" {
		t.Errorf("frontmatterField = %q, want %q", got, "hi there")
	}
}

func TestList(t *testing.T) {
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	// Every listed name must resolve to a non-empty skill, and the list must
	// stay sorted. Names are enumerated from the embed FS, so this also guards
	// against a directory sneaking in without a SKILL.md.
	if len(names) == 0 {
		t.Fatal("List() returned no skills")
	}
	for i, n := range names {
		if i > 0 && names[i-1] > n {
			t.Errorf("List() not sorted: %q before %q", names[i-1], n)
		}
		b, err := Skill(n)
		if err != nil || len(b) == 0 {
			t.Errorf("listed skill %q does not resolve: err=%v len=%d", n, err, len(b))
		}
	}
	// The core skills must be present.
	want := map[string]bool{"harness-cli": false, "independent-review": false, "landing-to-main": false, "session-debugging": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("List() missing expected skill %q", n)
		}
	}
}

func TestSkillHarnessCLI(t *testing.T) {
	b, err := Skill("harness-cli")
	if err != nil {
		t.Fatalf("Skill(harness-cli): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("harness-cli skill is empty")
	}
}

func TestSkillIndependentReview(t *testing.T) {
	b, err := Skill("independent-review")
	if err != nil {
		t.Fatalf("Skill(independent-review): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("independent-review skill is empty")
	}
}

func TestSkillLandingToMain(t *testing.T) {
	b, err := Skill("landing-to-main")
	if err != nil {
		t.Fatalf("Skill(landing-to-main): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("landing-to-main skill is empty")
	}
}

func TestSkillSessionDebugging(t *testing.T) {
	b, err := Skill("session-debugging")
	if err != nil {
		t.Fatalf("Skill(session-debugging): %v", err)
	}
	if len(b) == 0 {
		t.Fatal("session-debugging skill is empty")
	}
}

func TestSkillUnknown(t *testing.T) {
	if _, err := Skill("nope"); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

// TestListFSFiltersNonSkillEntries pins the filter the runner's on-disk source
// depends on. The embed FS is curated by the //go:embed directive, so List()
// never had to reject anything; an os.DirFS handed to ListFS is not curated —
// os.DirFS("runner/agentskills") also carries this package's .go files, and a
// checkout's .claude/skills carries repo-only skills. Only a top-level
// directory holding a SKILL.md counts.
func TestListFSFiltersNonSkillEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"beta-skill", "alpha-skill"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with no SKILL.md, and a plain file at top level.
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embed.go"), []byte("package agentskills\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := ListFS(os.DirFS(dir))
	if err != nil {
		t.Fatalf("ListFS: %v", err)
	}
	want := []string{"alpha-skill", "beta-skill"} // sorted
	if len(names) != len(want) {
		t.Fatalf("ListFS = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("ListFS = %v, want %v", names, want)
		}
	}
}

// TestListFSEmptyDir: a directory with nothing skill-shaped in it lists no
// skills rather than erroring. agent-runner turns that into a startup failure
// (see resolveAgentSkillsDir) — the distinction matters because injection
// itself is a Warn-only step, so a typo'd path must be caught before any task
// runs, not per task.
func TestListFSEmptyDir(t *testing.T) {
	names, err := ListFS(os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatalf("ListFS on an empty dir: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("ListFS on an empty dir = %v, want none", names)
	}
}

// TestListFSOverEmbedMatchesList keeps the two entry points from drifting:
// List() must stay a thin wrapper, or the embedded and on-disk paths could
// start disagreeing about what a skill is.
func TestListFSOverEmbedMatchesList(t *testing.T) {
	viaList, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	viaFS, err := ListFS(FS)
	if err != nil {
		t.Fatalf("ListFS(FS): %v", err)
	}
	if len(viaList) != len(viaFS) {
		t.Fatalf("List() = %v, ListFS(FS) = %v", viaList, viaFS)
	}
	for i := range viaList {
		if viaList[i] != viaFS[i] {
			t.Fatalf("List() = %v, ListFS(FS) = %v", viaList, viaFS)
		}
	}
}

// mirrorDirs are the checked-in copies of the embedded skills, relative to the
// repo root: .claude/ is what a claude session in THIS repo loads, .agents/ is
// what other harnesses read. This package is the go:embed source of truth, so
// both are copies and neither may drift from it.
var mirrorDirs = []string{".claude/skills", ".agents/skills"}

// repoRoot is this package's directory (the test's working dir) walked back to
// the checkout root.
const repoRoot = "../.."

// TestMirrorsMatchEmbeddedSkills is the mechanical form of surface-parity
// checklist item 35: an embedded skill is edited HERE and mirrored to
// .claude/skills and .agents/skills in the same commit.
//
// It exists because that rule was manual and drifted three times in a row: at
// the time this test was written, .agents/ carried an older harness-cli,
// landing-to-main and supervising-workers than the embed FS did, so agents in
// other repositories were reading instructions this repo had already replaced —
// silently, because nothing compared the two. Same reasoning as keys_test's
// binding/help pair: a rule that only lives in a checklist is a rule that gets
// walked past.
//
// Byte-identical, not "close enough": a mirror that is merely similar is the
// state that produced the drift, and there is no editorial difference between
// the copies to preserve.
func TestMirrorsMatchEmbeddedSkills(t *testing.T) {
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	for _, n := range names {
		want, err := Skill(n)
		if err != nil {
			t.Fatalf("Skill(%q): %v", n, err)
		}
		for _, dir := range mirrorDirs {
			path := filepath.Join(repoRoot, dir, n, "SKILL.md")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %v — mirror the embedded skill there in the same commit", path, err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the embedded runner/agentskills/%s/SKILL.md "+
					"(%d vs %d bytes); copy the embedded file over it", path, n, len(got), len(want))
			}
		}
	}
}

// TestAgentsMirrorHasNoExtraSkills guards the other direction for .agents/,
// which is read by agents in OTHER repositories: a skill there that this
// package does not embed is either a stale copy of a removed skill or a
// repo-dev skill that leaked out of .claude/ (implementation-pitfalls,
// surface-parity-checklist and dummy-harness are about THIS repository and are
// deliberately .claude-only). Either way the agents reading it are being told
// about something that is not theirs.
func TestAgentsMirrorHasNoExtraSkills(t *testing.T) {
	names, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	embedded := make(map[string]bool, len(names))
	for _, n := range names {
		embedded[n] = true
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, ".agents/skills"))
	if err != nil {
		t.Fatalf("read .agents/skills: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || embedded[e.Name()] {
			continue
		}
		t.Errorf(".agents/skills/%s is not an embedded skill: remove it, or add it to "+
			"runner/agentskills if agents in other repos are meant to have it", e.Name())
	}
}
