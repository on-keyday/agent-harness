package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/transcript"
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

	// TranscriptStore names where this agent keeps its transcripts, as
	// runner/transcript.New knows them ("claude", "codex", "pi", "agy",
	// "opencode"); "" for an agent with none (a shell). Pillar B lists and
	// reads them by it.
	TranscriptStore string

	// StreamAdapter is the binary that runs this agent as an EVENT-STREAM
	// task: the runner execs the adapter and hands it the resolved agent argv,
	// and the adapter speaks the neutral protocol on its stdio
	// (runner/streamagent). Empty means this profile cannot serve the kind,
	// and a request for it is refused rather than silently falling back to a
	// PTY — the two are not substitutes.
	//
	// Resolved per task rather than cached, so editing an adapter reaches the
	// next task with no rebuild and no restart, the same trade
	// --agentskills-dir makes.
	StreamAdapter string
}

// ServesStream reports whether this profile can run an event-stream task.
func (p AgentProfile) ServesStream() bool { return p.StreamAdapter != "" }

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

// UnresolvableStreamAdapters returns `<name>: "<path>": <err>` for every
// profile whose StreamAdapter is non-empty and does not currently resolve.
//
// WARN, not error, and the adapter is NOT rewritten to the resolved path —
// this only reports. StreamAdapter is deliberately read per task so an adapter
// can be replaced under a live runner, and the runner fails the individual task
// when it cannot be found (Run in streamtask.go). The startup line exists
// because a preset now supplies this path for the claude profile
// (scripts/agent_presets.py), and `bin/harness-stream-adapter` is a build
// artifact: a checkout that has not run `make build` would otherwise look
// healthy until the first event-stream task failed.
//
// Same shape and same reason as UnrecognisedTranscriptStores below: a config
// value consumed later in the task path, reported once where the operator is
// still watching.
func (ps ProfileSet) UnresolvableStreamAdapters() []string {
	var out []string
	for _, p := range ps.profiles {
		if p.StreamAdapter == "" {
			continue
		}
		if _, err := exec.LookPath(p.StreamAdapter); err != nil {
			out = append(out, fmt.Sprintf("%s: %q: %v", p.Name, p.StreamAdapter, err))
		}
	}
	return out
}

// ResolveBinPaths replaces each profile's Bin with its exec.LookPath +
// filepath.Abs resolution, in place, and returns a warning line per profile
// whose Bin could not be resolved (that Bin is kept verbatim, so the failure
// surfaces at spawn rather than stopping the runner from starting).
//
// This must run once at startup, before any Bin reaches the exec layer: every
// spawn hands Bin to go-pty, whose Windows lookExtensions joins a bare name
// onto cmd.Dir (the task worktree) and never consults PATH, so a bare
// --agent-bin failed every PTY session (while the oneshot path A3 retired,
// which PATH-resolved through os/exec, worked). Resolving to an absolute path
// here makes a bare name work on every platform.
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
	StreamAdapter         string   `json:"streamAdapter"`
	TranscriptStore       string   `json:"transcriptStore"`

	// LogFormat is accepted and ignored since A3: restart.py replays a
	// runner's argv, and an --agent-profiles written before then carries it
	// (spec A-D32). Declared rather than left to json.Unmarshal's tolerance of
	// unknown keys, so a stricter decoder later cannot refuse that argv.
	LogFormat string `json:"logFormat"`
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
			StreamAdapter:         r.StreamAdapter,
			TranscriptStore:       r.TranscriptStore,
		})
	}
	return out, nil
}

// UnrecognisedTranscriptStores returns `<name>: "<value>"` for every profile
// whose TranscriptStore is neither empty nor a kind runner/transcript knows.
// agent-runner logs these once at startup so a typo is visible rather than
// silently listing no transcripts.
func (ps ProfileSet) UnrecognisedTranscriptStores() []string {
	known := map[string]bool{}
	for _, k := range transcript.Kinds {
		known[k] = true
	}
	var out []string
	for _, p := range ps.profiles {
		if p.TranscriptStore != "" && !known[p.TranscriptStore] {
			out = append(out, fmt.Sprintf("%s: %q", p.Name, p.TranscriptStore))
		}
	}
	return out
}

// TranscriptAgentBin returns, for a store read through the agent's own CLI
// (transcript.CLIName), the Bin of the first profile naming that store whose
// file is that CLI (modulo a Windows extension) — after ResolveBinPaths, the
// exact binary that profile's agent runs. "" when none is: a store read from
// files, or a runner whose only such profile is a wrapper (sandbox-opencode
// runs opencode-in-podman.sh, which cannot list sessions), and the store then
// runs the CLI's name from PATH.
func (ps ProfileSet) TranscriptAgentBin(kind string) string {
	name := transcript.CLIName(kind)
	if name == "" {
		return ""
	}
	for _, p := range ps.profiles {
		if p.TranscriptStore == kind && protocol.EqualAgentProfileName(filepath.Base(p.Bin), name) {
			return p.Bin
		}
	}
	return ""
}

// TranscriptStores returns the distinct, recognised store kinds of the
// profiles, in profile order: several profiles (an agent and its sandboxed
// variant) share one store, which is asked once.
func (ps ProfileSet) TranscriptStores() []string {
	known := map[string]bool{}
	for _, k := range transcript.Kinds {
		known[k] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range ps.profiles {
		if k := p.TranscriptStore; known[k] && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
