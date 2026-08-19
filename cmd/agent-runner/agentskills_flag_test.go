package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillDir builds a directory holding one skill plus a non-skill file, the
// shape os.DirFS sees when pointed at the repo's runner/agentskills.
func writeSkillDir(t *testing.T, skill string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skill, "SKILL.md"), []byte("---\nname: "+skill+"\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embed.go"), []byte("package agentskills\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveAgentSkillsDirUnsetKeepsEmbedded(t *testing.T) {
	fsys, names, err := resolveAgentSkillsDir("", "")
	if err != nil {
		t.Fatalf("resolveAgentSkillsDir: %v", err)
	}
	if fsys != nil || names != nil {
		t.Fatalf("unset should yield the embedded default (nil FS), got fs=%v names=%v", fsys, names)
	}
}

func TestResolveAgentSkillsDirFromFlag(t *testing.T) {
	dir := writeSkillDir(t, "harness-cli")
	fsys, names, err := resolveAgentSkillsDir(dir, "")
	if err != nil {
		t.Fatalf("resolveAgentSkillsDir: %v", err)
	}
	if fsys == nil {
		t.Fatal("expected a non-nil FS for a populated dir")
	}
	if len(names) != 1 || names[0] != "harness-cli" {
		t.Fatalf("names = %v, want [harness-cli] — non-skill entries must not be counted", names)
	}
}

func TestResolveAgentSkillsDirFromEnv(t *testing.T) {
	dir := writeSkillDir(t, "session-debugging")
	_, names, err := resolveAgentSkillsDir("", dir)
	if err != nil {
		t.Fatalf("resolveAgentSkillsDir: %v", err)
	}
	if len(names) != 1 || names[0] != "session-debugging" {
		t.Fatalf("names = %v, want [session-debugging]", names)
	}
}

// The flag wins over the env var, matching --psk / HARNESS_PSK in this file.
func TestResolveAgentSkillsDirFlagBeatsEnv(t *testing.T) {
	flagDir := writeSkillDir(t, "from-flag")
	envDir := writeSkillDir(t, "from-env")
	_, names, err := resolveAgentSkillsDir(flagDir, envDir)
	if err != nil {
		t.Fatalf("resolveAgentSkillsDir: %v", err)
	}
	if len(names) != 1 || names[0] != "from-flag" {
		t.Fatalf("names = %v, want [from-flag]", names)
	}
}

// A path that resolves to no skills must fail at STARTUP. Skill injection
// itself is Warn-only in the task path (session.go handleAssign), so a typo
// would otherwise be invisible except as one warning per task, with every task
// silently running without skills.
func TestResolveAgentSkillsDirEmptyIsFatal(t *testing.T) {
	_, _, err := resolveAgentSkillsDir(t.TempDir(), "")
	if err == nil {
		t.Fatal("a directory with no skills must be a startup error, not a silent fallback to the embedded copy")
	}
	if !strings.Contains(err.Error(), "no skills") {
		t.Fatalf("error should say what is wrong, got %v", err)
	}
}

func TestResolveAgentSkillsDirMissingPathIsFatal(t *testing.T) {
	_, _, err := resolveAgentSkillsDir(filepath.Join(t.TempDir(), "does-not-exist"), "")
	if err == nil {
		t.Fatal("a nonexistent --agentskills-dir must be a startup error")
	}
}

func TestAgentSkillsDirFlagBinds(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{"--agentskills-dir", "/some/where"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.AgentSkillsDir != "/some/where" {
		t.Fatalf("AgentSkillsDir = %q", cfg.AgentSkillsDir)
	}
	if cfg.AgentSkillsDir == newMainConfig().AgentSkillsDir {
		t.Fatal("default should be empty (embedded copy) and differ from the parsed value")
	}
}

func TestResolveAgentSkillsDirFileIsFatal(t *testing.T) {
	f := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(f, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveAgentSkillsDir(f, "")
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("pointing at a file should say so, got %v", err)
	}
}
