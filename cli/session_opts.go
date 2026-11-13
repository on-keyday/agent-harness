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
	// Overrides narrow individual capabilities below Scope. It rides HERE
	// rather than beside each caller's request build, because a field set in
	// one caller and missed in the others is the established failure mode on
	// these routes -- the wire carries it as a sibling of scope, and both
	// shared builders below copy it under the same presence bit.
	Overrides []protocol.ScopeOverride
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
	// EventStream opens the session as TaskKind_Stream: the runner spawns the
	// profile's stream adapter instead of the agent under a PTY, and the
	// frames carry neutral NDJSON. A profile with no adapter refuses the
	// request rather than falling back to a terminal.
	EventStream bool
	// ScopePresent, on a resume, writes Scope onto the task (attenuated);
	// false keeps the task's persisted scope. Independent of
	// ResumeCapsOverride — the two halves of authority re-grant separately.
	// Ignored on create.
	ScopePresent bool
	// ResumeExact, on a resume, takes every setting from the task's record:
	// the request must then carry no prompt, args, agent, caps or scope
	// re-grant, and needs only the resume_exact capability.
	ResumeExact bool
	// DelegateCaps and DelegateScope are the delegation bound written onto the
	// task: what IT may put into the tasks it creates. Zero is the default
	// bound, which delegates nothing — the same fail-closed reading as Caps.
	// On a resume each half is written only behind its presence bit, so a
	// session default can never silently rewrite a resumed task's bound.
	DelegateCaps         protocol.Capability
	DelegateScope        protocol.TaskScope
	DelegateCapsPresent  bool
	DelegateScopePresent bool
	// SpawnDepth and MaxDescendants are the spawn limits written onto the
	// task. The zero value is unbounded, and an unbounded SpawnDepth on a
	// fresh spawn gets the creator's minus one server-side. On a resume each
	// is written only behind its presence bit, like the bound.
	SpawnDepth            protocol.SpawnLimit
	MaxDescendants        protocol.SpawnLimit
	SpawnDepthPresent     bool
	MaxDescendantsPresent bool
	// Detached opens the session without attaching the caller: the server
	// creates no client stream, so the caller never holds the control seat
	// (which needs exec_control). The initial size then travels in the request,
	// because there is no stream to carry the window-size frame.
	Detached bool

	// Pillar A run mode (docs/superpowers/specs/2026-11-13-pillar-a-run-modes-design.md).
	// RunModeSet names the run mode (Background true = background, false =
	// foreground); unset, a resume keeps the task's own. Conditions (nil =
	// none) are a background run's; a deadline is required there. Prompt is
	// the agent's first turn, in either mode, carried on its own stream
	// (at most protocol.PromptMax bytes).
	RunModeSet bool
	Background bool
	Conditions *protocol.BackgroundConditions
	Prompt     string

	// RecordSet: this open says whether its run is recorded (Record), rather
	// than leaving it to the server's --record default (A-D22). Saying on
	// needs record_persist.
	RecordSet bool
	Record    bool
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
	oi.ExtraArgs = protocol.AgentArgsFromStrings(opts.ExtraArgs)
	oi.RequestedCaps = opts.Caps
	oi.SetResumeCapsOverride(opts.ResumeCapsOverride)
	oi.SetResumeConversation(opts.ResumeConversation)
	oi.SetAgentProfile([]byte(opts.AgentProfile))
	oi.Scope = opts.Scope
	oi.Overrides = opts.Overrides
	oi.OverridesLen = uint8(len(opts.Overrides))
	oi.SetScopePresent(opts.ScopePresent)
	oi.DelegateCaps, oi.DelegateScope = opts.DelegateCaps, opts.DelegateScope
	oi.SetDelegateCapsPresent(opts.DelegateCapsPresent)
	oi.SetDelegateScopePresent(opts.DelegateScopePresent)
	oi.SpawnDepth, oi.MaxDescendants = opts.SpawnDepth, opts.MaxDescendants
	oi.SetSpawnDepthPresent(opts.SpawnDepthPresent)
	oi.SetMaxDescendantsPresent(opts.MaxDescendantsPresent)
	oi.SetEventStream(opts.EventStream)
	oi.SetResumeExact(opts.ResumeExact)
	if opts.Detached {
		oi.SetDetached(true) // the discriminator before its block
		oi.SetInitialRows(opts.InitialRows)
		oi.SetInitialCols(opts.InitialCols)
	}
	oi.SetRunModeSet(opts.RunModeSet)
	oi.SetBackground(opts.Background)
	if opts.Conditions != nil {
		oi.SetConditionsPresent(true) // the discriminator before its block
		oi.SetConditions(*opts.Conditions)
	}
	if opts.RecordSet {
		var ext protocol.OpenInteractiveExt
		ext.SetRecordSet(true)
		ext.SetRecord(opts.Record)
		oi.SetExtPresent(true) // the discriminator before its byte
		oi.SetExt(ext)
	}
	return oi
}
