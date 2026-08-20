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
// Zero value = a plain new task on any runner with the runner's default agent
// and NO capabilities. Every field is safe at zero:
//
//   - Selector zero value is RunnerSelectorKind_Any (the intended default).
//   - Caps zero value is Capability_None, which is also what an omitted --caps
//     parses to (ParseCaps("")), so unset and "explicitly none" are the same
//     request and a caller that forgets the field fails CLOSED. It was a
//     pointer while the default was Capability_All = 4095 — a non-zero default
//     needs "unset" and "zero" told apart, or "--caps none" reads as
//     inherit-all. Default-deny removes that distinction entirely; the
//     presence bit the resume path still needs is ResumeCapsOverride below,
//     which is about re-granting, not about parsing.
type SessionOpts struct {
	Selector           protocol.RunnerSelector
	ExtraArgs          []string
	ResumeTaskID       string // hex; "" = new task
	Caps               protocol.Capability
	ResumeCapsOverride bool   // resume only: re-grant Caps instead of keeping the task's persisted mask
	ResumeConversation bool   // resume the agent's own conversation (--continue-equivalent)
	AgentProfile       string // "" = runner default; e.g. "codex"
	// Scope bounds WHICH tasks Caps may be pointed at. Unlike Caps this needs
	// no pointer: the zero value is ScopeBase_Subtree, which IS the intended
	// default, and "explicitly none" is a distinct non-zero value. There is no
	// unset-vs-none ambiguity to encode — on a FRESH spawn. A resume does
	// have one ("re-grant this scope" vs "keep the task's"), so it carries
	// its own presence bit:
	Scope protocol.TaskScope
	// InitialRows / InitialCols size the session's PTY at open time. Both must
	// be non-zero to take effect; 0 (the zero value) sends nothing and leaves
	// the historical behaviour, where a PTY has NO size until an attached
	// client sends its own TerminalWindowSize frame.
	//
	// This is the only chance a detached session gets: resizing is a control
	// frame on an attached stream, and a spawner holding just `spawn` can never
	// attach, and a resize needs the CONTROL mode (exec_control), so a session it opens with
	// -d would stay 0x0 for its whole life. Measured 2026-08-18: codex's and
	// agy's full-screen TUIs paint nothing at that size.
	//
	// On an ATTACHED open the value is short-lived by design — the client's own
	// resize loop overwrites it with the real terminal size moments later.
	InitialRows uint16
	InitialCols uint16
	// ScopePresent, on a resume, writes Scope onto the task (attenuated);
	// false keeps the task's persisted scope. Independent of
	// ResumeCapsOverride — the two halves of authority re-grant separately.
	// Ignored on create.
	ScopePresent bool
}

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
	oi.RequestedCaps = opts.Caps
	oi.SetResumeCapsOverride(opts.ResumeCapsOverride)
	oi.SetResumeConversation(opts.ResumeConversation)
	oi.SetAgentProfile([]byte(opts.AgentProfile))
	oi.Scope = opts.Scope
	oi.SetScopePresent(opts.ScopePresent)
	return oi
}
