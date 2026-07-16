package cli

import "github.com/on-keyday/agent-harness/runner/protocol"

// SessionOpts is the one option bag for creating or resuming a session, shared
// by the submit (oneshot) and open-interactive paths. It replaces the
// Submit/Interactive "…WithSelectorArgsAndCaps" ladders, whose 10-11 positional
// arguments — three of them adjacent strings (repo, resumeTaskID, agentProfile)
// and two adjacent bools — could be transposed with no compile error. Named
// fields make transposition impossible, and a new field is added here once
// instead of threaded through ~20 call sites (2026-07-16: agentProfile was added
// exactly that way, and it did not even earn a name in the method ladder).
//
// Zero value = a plain new task on any runner with the runner's default agent,
// inheriting all of the spawner's capabilities. Every field is safe at zero:
//
//   - Selector zero value is RunnerSelectorKind_Any (the intended default).
//   - Caps is a POINTER on purpose. Capability_All is 4095, not the zero value,
//     and Capability(0) is a real user-facing value ("--caps none" = maximal
//     confinement). A plain Capability field could not tell "unset" from
//     "explicitly none": leaving it zero would silently confine every task, and
//     normalizing 0→All would turn "--caps none" into a privilege escalation.
//     So: nil = inherit-all (the historical default); &0 = explicitly none.
type SessionOpts struct {
	Selector           protocol.RunnerSelector
	ExtraArgs          []string
	ResumeTaskID       string // hex; "" = new task
	Caps               *protocol.Capability
	ResumeCapsOverride bool   // resume only: re-grant Caps instead of keeping the task's persisted mask
	ResumeConversation bool   // resume the agent's own conversation (--continue-equivalent)
	AgentProfile       string // "" = runner default; e.g. "codex"
}

// resolvedCaps returns the capability mask to send on the wire: the explicit
// value when set, else Capability_All (inherit everything the spawner holds).
func (o SessionOpts) resolvedCaps() protocol.Capability {
	if o.Caps != nil {
		return *o.Caps
	}
	return protocol.Capability_All
}

// CapsPtr is a helper for callers that have a Capability value and want to set
// SessionOpts.Caps explicitly (including the confining Capability(0)).
func CapsPtr(c protocol.Capability) *protocol.Capability { return &c }

// buildOpenInteractiveRequest constructs the wire OpenInteractiveRequest from
// SessionOpts, minus resumeTaskID (hex-parse error handled by the caller) and
// the X11 fields (set by the caller when x11 != nil). Shared by the native and
// wasm open-interactive paths so they cannot drift; also lets unit tests assert
// on the built request's fields (e.g. AgentProfile) without a live connection.
func buildOpenInteractiveRequest(repoPath string, opts SessionOpts) protocol.OpenInteractiveRequest {
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repoPath))
	oi.Selector = opts.Selector
	oi.ExtraArgs = protocol.ClaudeArgsFromStrings(opts.ExtraArgs)
	oi.RequestedCaps = opts.resolvedCaps()
	oi.SetResumeCapsOverride(opts.ResumeCapsOverride)
	oi.SetResumeConversation(opts.ResumeConversation)
	oi.SetAgentProfile([]byte(opts.AgentProfile))
	return oi
}
