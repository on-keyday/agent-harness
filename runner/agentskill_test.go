package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentSkills_WritesHarnessCliSkill(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir); err != nil {
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
	if !strings.Contains(s, "harness.hello") {
		t.Error("SKILL.md should document the harness.hello handshake topic")
	}
	if !strings.Contains(s, "payload_b64") || !strings.Contains(s, "json.Valid") {
		t.Error("SKILL.md should explain the JSON-vs-base64 inbox behaviour")
	}
}

func TestWriteAgentSkills_CreatesClaudeMdWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentSkills(dir); err != nil {
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
	if err := WriteAgentSkills(dir); err != nil {
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
	if err := WriteAgentSkills(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(skillPath)
	if string(data) == "stale" {
		t.Error("WriteAgentSkills should overwrite stale SKILL.md so runner upgrades ship new guidance")
	}
}
