package main

import (
	"flag"
	"strings"
	"testing"
)

// TestListenFlagMutualExclusion verifies that providing both --server-cid
// (other than its default) and --listen returns an error from validate.
func TestListenFlagMutualExclusion(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*",
		"--listen", "0.0.0.0:8540",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.serverCIDExplicit = true // simulate user-set (not default)
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

// TestListenFlagRequiresOneOf verifies that providing neither --server-cid
// (when cleared) nor --listen/--udp-listen returns an error.
func TestListenFlagRequiresOneOf(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := cfg.validate()
	if err == nil {
		t.Fatalf("expected error when neither --server-cid nor --listen provided")
	}
}

// TestListenOnlyMode verifies --listen alone (no --server-cid) is valid.
func TestListenOnlyMode(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "",
		"--listen", "0.0.0.0:8540",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !cfg.isListenMode() {
		t.Errorf("expected isListenMode() true with --listen set")
	}
}

// TestServerCIDOnlyMode verifies the legacy --server-cid path stays valid.
func TestServerCIDOnlyMode(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--server-cid", "ws:127.0.0.1:8539-*",
		"--roots", "/tmp",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.isListenMode() {
		t.Errorf("expected isListenMode() false with only --server-cid set")
	}
}

func TestParseAgentArgsFlag(t *testing.T) {
	got, err := parseAgentArgsFlag("--agent-oneshot-argv", `exec {args} "{prompt}"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "{args}", "{prompt}"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestAgentArgvFlagsParseAndValidate(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--agent-oneshot-argv", "exec {args} {prompt}",
		"--agent-resume-interactive-argv", "resume --last {args}",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	oneshot, err := parseAgentArgsFlag("--agent-oneshot-argv", cfg.AgentOneshotArgv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(oneshot, " ") != "exec {args} {prompt}" {
		t.Fatalf("oneshot argv = %#v", oneshot)
	}
	resume, err := parseAgentArgsFlag("--agent-resume-interactive-argv", cfg.AgentResumeInteractiveArgv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(resume, " ") != "resume --last {args}" {
		t.Fatalf("resume argv = %#v", resume)
	}
}

func TestAgentRuntimeAliasFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--agent-bin", "codex",
		"--agent-args", `--profile "agent default"`,
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ClaudeBin != "codex" {
		t.Fatalf("ClaudeBin = %q, want codex", cfg.ClaudeBin)
	}
	if cfg.ClaudeArgs != `--profile "agent default"` {
		t.Fatalf("ClaudeArgs = %q", cfg.ClaudeArgs)
	}
}

func TestClaudeRuntimeAliasFlagsRemainSupported(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--claude-bin", "claude",
		"--claude-args", "--permission-mode auto",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ClaudeBin != "claude" {
		t.Fatalf("ClaudeBin = %q, want claude", cfg.ClaudeBin)
	}
	if cfg.ClaudeArgs != "--permission-mode auto" {
		t.Fatalf("ClaudeArgs = %q", cfg.ClaudeArgs)
	}
}
