package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/on-keyday/agent-harness/runner/agentlog"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AgentProfile is one named agent launch profile: a binary plus the argv
// templates used to build oneshot / resume-oneshot / resume-interactive
// invocations. A runner may advertise several profiles (e.g. "claude",
// "codex") so a client can pick which agent binary handles a given task.
type AgentProfile struct {
	Name string
	Bin  string

	AgentArgs             []string
	OneshotArgv           []string
	ResumeOneshotArgv     []string
	InteractiveArgv       []string
	ResumeInteractiveArgv []string

	// LogFormat names the agentlog decoder for this agent's stdout:
	// "" (passthrough), "claude-stream-json", or "codex-jsonl". An
	// unrecognised value resolves to passthrough rather than failing, so a
	// typo degrades to raw lines instead of stopping the runner.
	LogFormat string
}

// ProfileSet is the immutable set of agent profiles a runner was configured
// with: exactly one default profile (index 0, built from the single-agent
// --agent-bin/--agent-args/... flags) plus zero or more extra profiles
// parsed from --agent-profiles.
type ProfileSet struct {
	profiles []AgentProfile
}

// NewProfileSet builds a ProfileSet from a default profile and extra
// profiles. The default profile is always index 0 / the empty-name
// resolution target. Every profile name (default and extra) must be unique,
// and every profile's argv templates must pass the existing
// ValidateOneshotArgvTemplate / ValidateResumeInteractiveArgvTemplate
// checks.
func NewProfileSet(defaultP AgentProfile, extra []AgentProfile) (ProfileSet, error) {
	all := make([]AgentProfile, 0, 1+len(extra))
	all = append(all, defaultP)
	all = append(all, extra...)

	seen := make(map[string]bool, len(all))
	for _, p := range all {
		norm := protocol.NormalizeAgentProfileName(p.Name)
		if seen[norm] {
			return ProfileSet{}, fmt.Errorf("duplicate agent profile name %q (names are compared modulo Windows executable extension)", p.Name)
		}
		seen[norm] = true

		if err := ValidateOneshotArgvTemplate(p.OneshotArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: oneshotArgv: %w", p.Name, err)
		}
		if err := ValidateOneshotArgvTemplate(p.ResumeOneshotArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: resumeOneshotArgv: %w", p.Name, err)
		}
		if err := ValidateInteractiveArgvTemplate(p.InteractiveArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: interactiveArgv: %w", p.Name, err)
		}
		if err := ValidateResumeInteractiveArgvTemplate(p.ResumeInteractiveArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: resumeInteractiveArgv: %w", p.Name, err)
		}
	}
	return ProfileSet{profiles: all}, nil
}

// Resolve looks up a profile by name. An empty name resolves to the default
// profile (index 0). An unknown non-empty name is an error listing the
// available names. Matching is EqualAgentProfileName (modulo Windows
// executable extension), mirroring the server's RunnerEntry.HasProfile, so a
// dispatch carrying a task's recorded "claude.exe" resolves against a
// profile now named "claude".
func (ps ProfileSet) Resolve(name string) (AgentProfile, error) {
	if name == "" {
		if len(ps.profiles) == 0 {
			return AgentProfile{}, fmt.Errorf("no agent profiles configured")
		}
		return ps.profiles[0], nil
	}
	for _, p := range ps.profiles {
		if protocol.EqualAgentProfileName(p.Name, name) {
			return p, nil
		}
	}
	return AgentProfile{}, fmt.Errorf("unknown agent profile %q (have %v)", name, ps.Names())
}

// Names returns the configured profile names, default first.
func (ps ProfileSet) Names() []string {
	names := make([]string, len(ps.profiles))
	for i, p := range ps.profiles {
		names[i] = p.Name
	}
	return names
}

// UnrecognisedLogFormats returns `<name>: "<value>"` for every profile whose
// LogFormat is neither empty nor a name agentlog.NewDecoder recognises. It
// checks against agentlog.KnownFormats() rather than its own copy of the
// name list, so this check cannot drift from what NewDecoder actually
// accepts. agent-runner logs these once at startup so a typo is visible
// rather than silently degrading to raw output.
func (ps ProfileSet) UnrecognisedLogFormats() []string {
	known := make(map[string]bool, len(agentlog.KnownFormats()))
	for _, f := range agentlog.KnownFormats() {
		known[f] = true
	}
	var out []string
	for _, p := range ps.profiles {
		if p.LogFormat == "" || known[p.LogFormat] {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %q", p.Name, p.LogFormat))
	}
	return out
}

// ResolveBinPaths replaces each profile's Bin with its exec.LookPath +
// filepath.Abs resolution, in place, and returns a warning line per profile
// whose Bin could not be resolved (that Bin is kept verbatim, so the spawn
// fails later exactly as it would today — e.g. a binary installed after
// runner startup still works for the oneshot path).
//
// This must run once at startup, before any Bin reaches the exec layer: the
// oneshot path (os/exec.CommandContext) PATH-resolves a bare name itself,
// but the interactive path hands Bin to go-pty, whose Windows lookExtensions
// joins a bare name onto cmd.Dir (the task worktree) and never consults
// PATH — a bare --agent-bin spawns oneshots fine and fails every
// interactive/PTY session. Resolving to an absolute path here removes that
// asymmetry on every platform.
func (ps *ProfileSet) ResolveBinPaths() []string {
	var warns []string
	for i := range ps.profiles {
		p := &ps.profiles[i]
		resolved, err := exec.LookPath(p.Bin)
		if err != nil && !errors.Is(err, exec.ErrDot) {
			warns = append(warns, fmt.Sprintf("%s: %q: %v", p.Name, p.Bin, err))
			continue
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %q: %v", p.Name, p.Bin, err))
			continue
		}
		p.Bin = abs
	}
	return warns
}

// agentProfileJSON is the --agent-profiles wire shape: a JSON array of
// objects, one per extra profile.
type agentProfileJSON struct {
	Name                  string   `json:"name"`
	Bin                   string   `json:"bin"`
	AgentArgs             []string `json:"agentArgs"`
	OneshotArgv           []string `json:"oneshotArgv"`
	ResumeOneshotArgv     []string `json:"resumeOneshotArgv"`
	InteractiveArgv       []string `json:"interactiveArgv"`
	ResumeInteractiveArgv []string `json:"resumeInteractiveArgv"`
	LogFormat             string   `json:"logFormat"`
}

// ParseAgentProfilesJSON parses the JSON array accepted by --agent-profiles
// into extra AgentProfile values. An empty string parses to (nil, nil).
func ParseAgentProfilesJSON(s string) ([]AgentProfile, error) {
	if s == "" {
		return nil, nil
	}
	var raw []agentProfileJSON
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("invalid --agent-profiles JSON: %w", err)
	}
	out := make([]AgentProfile, 0, len(raw))
	for i, r := range raw {
		if r.Name == "" {
			return nil, fmt.Errorf("--agent-profiles[%d]: name is required", i)
		}
		out = append(out, AgentProfile{
			Name:                  r.Name,
			Bin:                   r.Bin,
			AgentArgs:             r.AgentArgs,
			OneshotArgv:           r.OneshotArgv,
			ResumeOneshotArgv:     r.ResumeOneshotArgv,
			InteractiveArgv:       r.InteractiveArgv,
			ResumeInteractiveArgv: r.ResumeInteractiveArgv,
			LogFormat:             r.LogFormat,
		})
	}
	return out, nil
}
