package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileSetResolve(t *testing.T) {
	def := AgentProfile{Name: "claude", Bin: "claude"}
	ps, err := NewProfileSet(def, []AgentProfile{{Name: "codex", Bin: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := ps.Resolve(""); p.Name != "claude" {
		t.Fatalf("empty→default got %q", p.Name)
	}
	if p, _ := ps.Resolve("codex"); p.Bin != "codex" {
		t.Fatalf("got %q", p.Bin)
	}
	if _, err := ps.Resolve("gemini"); err == nil {
		t.Fatal("unknown must error")
	}
}

func TestProfileSetDupName(t *testing.T) {
	_, err := NewProfileSet(AgentProfile{Name: "claude"}, []AgentProfile{{Name: "claude"}})
	if err == nil {
		t.Fatal("dup name must error")
	}
}

func TestProfileSetDupNameAcrossExeSuffix(t *testing.T) {
	_, err := NewProfileSet(AgentProfile{Name: "claude"}, []AgentProfile{{Name: "claude.exe"}})
	if err == nil {
		t.Fatal("claude + claude.exe denote the same profile; dup must error")
	}
}

func TestProfileSetResolveAcrossExeSuffix(t *testing.T) {
	ps, err := NewProfileSet(AgentProfile{Name: "claude.exe", Bin: "C:/bin/claude.exe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ps.Resolve("claude")
	if err != nil {
		t.Fatalf("Resolve(claude) against profile claude.exe: %v", err)
	}
	if p.Bin != "C:/bin/claude.exe" {
		t.Fatalf("got Bin %q", p.Bin)
	}
}

func TestParseAgentProfilesJSON(t *testing.T) {
	ps, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","oneshotArgv":["exec","{args}","{prompt}"],"resumeOneshotArgv":["exec","resume","--last","{args}","{prompt}"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "codex" {
		t.Fatalf("got %+v", ps)
	}
}

func TestParseAgentProfilesJSONCarriesLogFormat(t *testing.T) {
	got, err := ParseAgentProfilesJSON(`[{"name":"codex","bin":"codex","logFormat":"codex-jsonl"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].LogFormat != "codex-jsonl" {
		t.Fatalf("got %+v, want one profile with LogFormat codex-jsonl", got)
	}
}

func TestNewProfileSetAcceptsUnknownLogFormat(t *testing.T) {
	// A misconfigured profile must not stop the runner from starting; the
	// decoder resolves an unrecognised name to passthrough.
	_, err := NewProfileSet(AgentProfile{Name: "claude", Bin: "claude", LogFormat: "nonsense"}, nil)
	if err != nil {
		t.Fatalf("NewProfileSet rejected an unknown log format: %v", err)
	}
}

func TestUnrecognisedLogFormats(t *testing.T) {
	ps, err := NewProfileSet(
		AgentProfile{Name: "claude", Bin: "claude", LogFormat: "claude-stream-json"},
		[]AgentProfile{
			{Name: "codex", Bin: "codex", LogFormat: "codex-jsonl"},
			{Name: "bash", Bin: "bash"},
			{Name: "weird", Bin: "weird", LogFormat: "nonsense"},
		},
	)
	if err != nil {
		t.Fatalf("NewProfileSet: %v", err)
	}
	got := ps.UnrecognisedLogFormats()
	if len(got) != 1 || got[0] != `weird: "nonsense"` {
		t.Fatalf("got %v, want exactly the weird profile", got)
	}
}

func TestResolveBinPaths(t *testing.T) {
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "fakeagent")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ps, err := NewProfileSet(
		AgentProfile{Name: "fakeagent", Bin: "fakeagent"},
		[]AgentProfile{
			{Name: "missing", Bin: "no-such-agent-bin-xyz"},
			{Name: "abs", Bin: fake},
		},
	)
	if err != nil {
		t.Fatalf("NewProfileSet: %v", err)
	}

	warns := ps.ResolveBinPaths()
	if len(warns) != 1 || !strings.Contains(warns[0], "missing") {
		t.Fatalf("warnings = %v, want exactly one for profile \"missing\"", warns)
	}

	p, _ := ps.Resolve("fakeagent")
	if p.Bin != fake {
		t.Errorf("bare name: Bin = %q, want %q", p.Bin, fake)
	}
	if !filepath.IsAbs(p.Bin) {
		t.Errorf("bare name: Bin %q not absolute", p.Bin)
	}
	if p, _ = ps.Resolve("missing"); p.Bin != "no-such-agent-bin-xyz" {
		t.Errorf("unresolvable Bin changed to %q, want kept as-is", p.Bin)
	}
	if p, _ = ps.Resolve("abs"); p.Bin != fake {
		t.Errorf("absolute Bin changed to %q, want unchanged %q", p.Bin, fake)
	}
}

func TestResolveBinPathsRelativeWithSeparator(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakeagent")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	ps, err := NewProfileSet(AgentProfile{Name: "rel", Bin: "./fakeagent"}, nil)
	if err != nil {
		t.Fatalf("NewProfileSet: %v", err)
	}
	if warns := ps.ResolveBinPaths(); len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	p, _ := ps.Resolve("rel")
	if !filepath.IsAbs(p.Bin) {
		t.Errorf("relative Bin = %q, want absolute (a worktree-relative Dir join must not reinterpret it)", p.Bin)
	}
	if got, err := filepath.EvalSymlinks(p.Bin); err != nil || got != mustEvalSymlinks(t, fake) {
		t.Errorf("relative Bin resolved to %q (%v), want %q", p.Bin, err, fake)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
