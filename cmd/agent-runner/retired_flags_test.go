package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// restart.py replays a runner's argv, so a runner built after A3 must still
// start from the argv an older scripts/agent_presets.py produced (spec A-D32).
// --agent-log-format and the logFormat preset key are accepted and ignored: no
// value of either, including one the runner used to call unrecognised, may
// stop the runner or produce a startup warning. The oneshot argv flags stay
// live (session new --oneshot runs them).
func TestRunnerAcceptsRetiredFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{
		"--roots", "/tmp",
		"--agent-bin", "claude",
		"--agent-oneshot-argv", "--output-format stream-json --verbose {args} -p {prompt}",
		"--agent-resume-oneshot-argv", "--output-format stream-json --verbose {args} --continue -p {prompt}",
		"--agent-interactive-argv", "{args} -- {prompt}",
		"--agent-resume-interactive-argv", "{args} --continue -- {prompt}",
		// Not a name agentlog ever decoded: this used to draw the
		// "unrecognised --agent-log-format" startup warning.
		"--agent-log-format", "no-such-format",
		"--agent-transcript-store", "claude",
		"--agent-profiles", `[` +
			`{"name":"codex","bin":"codex","oneshotArgv":["exec","--json","{args}","{prompt}"],` +
			`"resumeOneshotArgv":["exec","--json","resume","--last","{args}","{prompt}"],` +
			`"logFormat":"codex-jsonl","transcriptStore":"codex"},` +
			`{"name":"weird","bin":"weird","logFormat":"also-not-a-format"}]`,
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var warn bytes.Buffer
	profiles, err := cfg.buildProfiles(&warn)
	if err != nil {
		t.Fatalf("buildProfiles: %v", err)
	}
	if got := strings.Join(profiles.Names(), ","); got != "claude,codex,weird" {
		t.Fatalf("profiles = %s, want claude,codex,weird", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("startup warnings = %q, want none (the log format is ignored)", warn.String())
	}
	if usage := fs.Lookup("agent-log-format").Usage; !strings.Contains(usage, "ignored since A3") {
		t.Fatalf("--agent-log-format usage = %q, want it to say the flag is ignored", usage)
	}
}
