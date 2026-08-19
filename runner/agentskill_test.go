package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentSkills_WritesHarnessCliSkill(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".claude", "skills", "harness-cli", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SKILL.md missing: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "name: harness-cli") {
		t.Errorf("SKILL.md missing frontmatter name: %q", s[:min(len(s), 200)])
	}
	// The id-directed convention is the whole addressing story now — the
	// harness.hello discovery topic was removed 2026-08-18, so assert the
	// naming rule that replaced it rather than the retired topic.
	if !strings.Contains(s, "chat.<first-8") {
		t.Error("SKILL.md should document the chat.<first-8-...> inbound topic convention")
	}
	if !strings.Contains(s, "payload_b64") || !strings.Contains(s, "json.Valid") {
		t.Error("SKILL.md should explain the JSON-vs-base64 inbox behaviour")
	}
}

func TestWriteAgentSkills_CreatesClaudeMdWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md not written: %v", err)
	}
	if !strings.Contains(string(data), "harness-cli") {
		t.Errorf("minimal CLAUDE.md should mention harness-cli, got %q", string(data))
	}
}

func TestWriteAgentSkills_PreservesExistingClaudeMd(t *testing.T) {
	dir := t.TempDir()
	original := []byte("# project CLAUDE.md\nproject-specific rules here\n")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("existing CLAUDE.md was modified:\nwant: %q\ngot:  %q", original, got)
	}
	// Skill should still have been written even though CLAUDE.md was untouched.
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "harness-cli", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md should still be written when CLAUDE.md is preserved: %v", err)
	}
}

func TestWriteAgentSkills_OverwritesStaleSkill(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".claude", "skills", "harness-cli", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(skillPath)
	if string(data) == "stale" {
		t.Error("WriteAgentSkills should overwrite stale SKILL.md so runner upgrades ship new guidance")
	}
}

func TestClaudeMdMinimalContent(t *testing.T) {
	if !strings.Contains(claudeMdMinimal, "harness-cli skill harness-cli") {
		t.Error("pointer should route any agent to `harness-cli skill harness-cli`")
	}
	if !strings.Contains(claudeMdMinimal, ".agents/skills/harness-cli/SKILL.md") {
		t.Error("pointer should mention the .agents/skills location too")
	}
	if !strings.Contains(claudeMdMinimal, "do not commit") {
		t.Error("pointer should tell agents not to commit harness-injected files")
	}
}

func TestWriteAgentSkills_WritesAgentsSkillsLocation(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(dir, ".claude", "skills", "harness-cli", "SKILL.md"),
		filepath.Join(dir, ".agents", "skills", "harness-cli", "SKILL.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected skill at %s: %v", p, err)
		}
	}
}

func TestWriteAgentSkills_WritesAgentsAndGeminiPointers(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
		if !strings.Contains(string(data), "harness-cli") {
			t.Errorf("%s should mention harness-cli", name)
		}
	}
}

func TestWriteAgentSkills_PreservesExistingAgentsMd(t *testing.T) {
	dir := t.TempDir()
	original := []byte("# project AGENTS.md\nproject rules\n")
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSkills(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(got) != string(original) {
		t.Errorf("existing AGENTS.md was modified:\nwant %q\ngot  %q", original, got)
	}
}

// newDiskSkillSource builds a directory shaped like runner/agentskills as
// os.DirFS sees it: real skills, plus the non-skill files that live alongside
// them in the checkout (the package's own .go sources) and a stray directory
// with no SKILL.md.
func newDiskSkillSource(t *testing.T, skills map[string]string) string {
	t.Helper()
	src := t.TempDir()
	for name, body := range skills {
		if err := os.MkdirAll(filepath.Join(src, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "embed.go"), []byte("package agentskills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "references", "notes.md"), []byte("not a skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestWriteAgentSkills_DiskSourceReplacesEmbedded(t *testing.T) {
	src := newDiskSkillSource(t, map[string]string{"custom-skill": "---\nname: custom-skill\n---\nfrom disk\n"})
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{".claude", ".agents"} {
		data, err := os.ReadFile(filepath.Join(dir, root, "skills", "custom-skill", "SKILL.md"))
		if err != nil {
			t.Fatalf("%s/skills/custom-skill/SKILL.md missing: %v", root, err)
		}
		if !strings.Contains(string(data), "from disk") {
			t.Errorf("%s: want the on-disk body, got %q", root, data)
		}
		// The embedded skills must NOT come along: a non-nil source replaces
		// the embed rather than layering over it, so an operator pointing at a
		// pruned directory gets exactly what is in it.
		if _, err := os.Stat(filepath.Join(dir, root, "skills", "harness-cli")); err == nil {
			t.Errorf("%s: embedded harness-cli leaked into a disk-sourced injection", root)
		}
	}
}

// TestWriteAgentSkills_DiskSourceSkipsNonSkillEntries is the concrete reason
// materializeSkills filters by agentskills.ListFS instead of walking the source
// wholesale. os.DirFS("runner/agentskills") — the natural thing to point
// --agentskills-dir at, since that directory IS the go:embed source of truth —
// also exposes embed.go and agentskills_test.go. A wholesale walk would write
// them into every task worktree's .claude/skills, where an agent reads that
// tree as its guidance.
func TestWriteAgentSkills_DiskSourceSkipsNonSkillEntries(t *testing.T) {
	src := newDiskSkillSource(t, map[string]string{"real-skill": "---\nname: real-skill\n---\nbody\n"})
	dir := t.TempDir()
	if err := WriteAgentSkills(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(dir, ".claude", "skills")
	for _, stray := range []string{"embed.go", "references"} {
		if _, err := os.Stat(filepath.Join(skillsRoot, stray)); err == nil {
			t.Errorf("%s was copied into .claude/skills; only directories holding a SKILL.md are skills", stray)
		}
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "real-skill", "SKILL.md")); err != nil {
		t.Errorf("the real skill should still be written: %v", err)
	}
}

// TestWriteAgentSkills_DiskSourceRereadPerCall is the whole point of the disk
// source: the runner process outlives `make build`, so its embedded copy is
// frozen at launch. Reading the source on each call is what lets an edited
// SKILL.md reach the next task without restarting the runner.
func TestWriteAgentSkills_DiskSourceRereadPerCall(t *testing.T) {
	src := newDiskSkillSource(t, map[string]string{"live-skill": "v1\n"})
	srcFS := os.DirFS(src)

	first := t.TempDir()
	if err := WriteAgentSkills(first, srcFS); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "live-skill", "SKILL.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := t.TempDir()
	if err := WriteAgentSkills(second, srcFS); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(second, ".claude", "skills", "live-skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "v2" {
		t.Errorf("second injection should carry the edited file, got %q", got)
	}
}
