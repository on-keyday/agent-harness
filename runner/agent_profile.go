package runner

import (
	"encoding/json"
	"fmt"
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
	ResumeInteractiveArgv []string
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
		if seen[p.Name] {
			return ProfileSet{}, fmt.Errorf("duplicate agent profile name %q", p.Name)
		}
		seen[p.Name] = true

		if err := ValidateOneshotArgvTemplate(p.OneshotArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: oneshotArgv: %w", p.Name, err)
		}
		if err := ValidateOneshotArgvTemplate(p.ResumeOneshotArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: resumeOneshotArgv: %w", p.Name, err)
		}
		if err := ValidateResumeInteractiveArgvTemplate(p.ResumeInteractiveArgv); err != nil {
			return ProfileSet{}, fmt.Errorf("agent profile %q: resumeInteractiveArgv: %w", p.Name, err)
		}
	}
	return ProfileSet{profiles: all}, nil
}

// Resolve looks up a profile by name. An empty name resolves to the default
// profile (index 0). An unknown non-empty name is an error listing the
// available names.
func (ps ProfileSet) Resolve(name string) (AgentProfile, error) {
	if name == "" {
		if len(ps.profiles) == 0 {
			return AgentProfile{}, fmt.Errorf("no agent profiles configured")
		}
		return ps.profiles[0], nil
	}
	for _, p := range ps.profiles {
		if p.Name == name {
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

// agentProfileJSON is the --agent-profiles wire shape: a JSON array of
// objects, one per extra profile.
type agentProfileJSON struct {
	Name                  string   `json:"name"`
	Bin                   string   `json:"bin"`
	AgentArgs             []string `json:"agentArgs"`
	OneshotArgv           []string `json:"oneshotArgv"`
	ResumeOneshotArgv     []string `json:"resumeOneshotArgv"`
	ResumeInteractiveArgv []string `json:"resumeInteractiveArgv"`
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
			ResumeInteractiveArgv: r.ResumeInteractiveArgv,
		})
	}
	return out, nil
}
