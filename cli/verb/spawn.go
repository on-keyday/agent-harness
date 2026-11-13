package verb

import (
	"flag"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The spawn family -- submit, session new -- shares almost all of its grammar,
// so it is built from one list rather than two that drift. The differences are
// real and few, and each is named below.
type spawnKind int

const (
	spawnSubmit spawnKind = iota
	spawnSessionNew
)

// stringList accumulates one entry per occurrence, in the order given, which
// is the order forwarded to the agent.
type stringList []string

func (l *stringList) String() string     { return fmt.Sprint([]string(*l)) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// spawnFlags returns the flag set for one spawn verb.
func spawnFlags(k spawnKind) []Flag {
	repoHelp := "repo identifier; must match a runner-registered RepoPath verbatim"
	out := []Flag{
		{
			Name: "repo", Type: FlagString, Default: "", Field: "Repo", Help: repoHelp,
			// The ladder all three surfaces had their own version of. The
			// workspace tier reached only the CLI before this.
			Resolve: []Tier{
				{Env: "HARNESS_REPO_PATH"},
				{Workspace: "repo"},
				{SurfaceContext: true},
			},
		},
		{Name: "resume", Type: FlagString, Default: "", Field: "ResumeTaskID",
			Help: "task id (32 hex) to resume — the server reuses the id and worktree branch, so the agent's project key matches the previous run"},
		{Name: "resume-conversation", Type: FlagBool, Default: false, Field: "ResumeConversation",
			Help: "with --resume, also ask the runner to resume the agent's own conversation state"},
		{Name: "exact", Type: FlagBool, Default: false, Field: "Exact",
			Help: "with --resume: resume exactly as recorded — no new prompt, args, agent, caps or scope (needs only resume_exact)"},
		{Name: "caps", Type: FlagString, Default: "", Field: "Caps",
			FieldType: "*protocol.Capability", Convert: "parseCapsFlag", PresenceField: "CapsPresent",
			Help: "comma-separated capability names to grant (e.g. spawn,file_read / all / none)"},
		{Name: "scope", Type: FlagString, Default: "", Field: "Scope",
			FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag",
			// The scope half is ONE unit: naming either --scope or --scope-for
			// makes it explicit, so a resume re-grants both together. Letting
			// --scope-for alone ride the session default's scope would write an
			// authority half typed and half inherited -- the TUI said so in
			// spawnAuthority's comment and derived presence that way, while the
			// CLI checked only "scope" and dropped a lone --scope-for on resume.
			PresenceField: "ScopePresent", PresenceAlso: "scope-for",
			Help: "which tasks those capabilities may target; default subtree (self + descendants)"},
		{Name: "agent", Type: FlagString, Default: "", Field: "Agent",
			Help: "agent profile name (empty = runner default; not to be confused with --agent-arg)"},
		{
			Name: "scope-for", Type: FlagString, Custom: scopeForValue, Field: "Overrides",
			FieldType: "[]protocol.ScopeOverride", Convert: "parseScopeForList",
			Help: "narrow ONE capability (or a comma-separated list of them) to a tighter scope than --scope",
		},
		// The delegation bound: what the new task may write onto the tasks IT
		// creates. Each half has its own presence, like --caps and --scope, so
		// a resume re-grants only the half it names.
		{Name: "delegate-caps", Type: FlagString, Default: "", Field: "DelegateCaps",
			FieldType: "*protocol.Capability", Convert: "parseCapsFlag", PresenceField: "DelegateCapsPresent",
			Help: "capabilities the new task may put into the tasks IT creates (default none)"},
		{Name: "delegate-scope", Type: FlagString, Default: "", Field: "DelegateScope",
			FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag", PresenceField: "DelegateScopePresent",
			Help: "the most scope it may give them, in --scope's grammar (default subtree)"},
		// The spawn limits, each with its own presence like the bound.
		{Name: "spawn-depth", Type: FlagString, Default: "", Field: "SpawnDepth",
			FieldType: "*protocol.SpawnLimit", Convert: "parseSpawnLimitFlag", PresenceField: "SpawnDepthPresent",
			Help: "generations that may still be created below the new task: N or unbounded (default: one less than yours)"},
		{Name: "max-descendants", Type: FlagString, Default: "", Field: "MaxDescendants",
			FieldType: "*protocol.SpawnLimit", Convert: "parseSpawnLimitFlag", PresenceField: "MaxDescendantsPresent",
			Help: "tasks the new task's subtree may hold, ended ones included until pruned: N or unbounded (default unbounded)"},
		{
			Name: "agent-arg", Type: FlagString, Custom: argListValue, Field: "ExtraArgs",
			Help: "extra CLI arg to forward to the agent (repeatable; appended after the runner-global --agent-args)",
		},
		// Runner pinning. Absent from the TUI's submit before the migration.
		{Name: "runner", Type: FlagString, Default: "", Field: "Runner",
			Help: "pin to a specific runner by ConnectionID (the id= value from `ls`)"},
		{Name: "host", Type: FlagString, Default: "", Field: "Host", Help: "pin to a runner by hostname"},
		{Name: "ip", Type: FlagString, Default: "", Field: "IP", Help: "pin to a runner by IP address"},
	}
	if k == spawnSubmit {
		out = append(out, Flag{Name: "task", Type: FlagString, Default: "", Field: "Task", Help: "prompt text"})
		return out
	}
	// `session new`: a session can be detached, streamed, X11-forwarded and
	// sized. A queued submit has no terminal for any of that to mean.
	return append(out,
		Flag{Name: "x11", Type: FlagBool, Default: false, Field: "X11",
			Help: "forward X11: inject DISPLAY/XAUTHORITY so GUI apps render on your local X server"},
		// 10, not 0: the runner binds 127.0.0.1:6000+N, and ssh picks 10 by
		// convention precisely so a forward does not land on the runner's own
		// :0 X port.
		Flag{Name: "x11-display", Type: FlagUint, Default: uint(10), Field: "X11Display",
			Help: "with --x11: the local display number (0..99)"},
		Flag{Name: "detach", Aliases: []string{"d"}, Type: FlagBool, Default: false, Field: "Detach",
			Help: "start the session and exit immediately (don't attach the terminal)"},
		Flag{Name: "stream", Type: FlagBool, Default: false, Field: "Stream",
			Help: "open an event-stream session (structured events, no PTY)"},
		// 40x120 by default (pillar A spec A-D7): a detached session nobody
		// attaches to is otherwise 0x0 for life, where some agents draw
		// nothing. 0 with --cols 0 still sends no size, for experiments.
		Flag{Name: "rows", Type: FlagUint, Default: uint(40), Field: "Rows", Help: "initial PTY rows of a detached/background open; --rows 0 --cols 0 sends no size (PTY 0x0)"},
		Flag{Name: "cols", Type: FlagUint, Default: uint(120), Field: "Cols", Help: "initial PTY columns of a detached/background open; --rows 0 --cols 0 sends no size (PTY 0x0)"},
		// Run mode (pillar A spec A-D1, A-D7, A-D15).
		Flag{Name: "background", Type: FlagBool, Default: false, Field: "Background",
			Help: "run unattended: queued until a runner is free, ended by its conditions (implies --detach; needs --prompt and --deadline, and --agent when new)"},
		Flag{Name: "foreground", Type: FlagBool, Default: false, Field: "Foreground",
			Help: "with --resume: resume a background task in foreground (without either, a resume keeps the task's run mode)"},
		Flag{Name: "prompt", Type: FlagString, Default: "", Field: "Prompt",
			Help: "the agent's first turn, in either run mode (at most 16384 bytes)"},
		Flag{Name: "deadline", Type: FlagDuration, Default: time.Duration(0), Field: "Deadline",
			Help: "background: the run is cancelled this long after it starts (required; no default)"},
		Flag{Name: "turn-end", Type: FlagString, Default: "", Field: "TurnEnd",
			Help: "background: which signal ends the run at a turn's end: off|quiet|lifecycle|program|any (default any; a stream session: lifecycle)"},
		Flag{Name: "quiet", Type: FlagDuration, Default: time.Duration(0), Field: "QuietFor",
			Help: "background: output silence that counts as a finished turn (default 5m)"},
		Flag{Name: "blocked", Type: FlagDuration, Default: time.Duration(0), Field: "BlockedFor",
			Help: "background: how long the agent may wait on a human before the run ends (default 5m)"},
		Flag{Name: "queue-timeout", Type: FlagDuration, Default: time.Duration(0), Field: "QueueTimeout",
			Help: "background: cancel the task if it is still queued this long after it was queued (default none)"},
		// The PTY record (pillar A spec, A-D22): neither = the server's
		// --record default; a resume does not reuse the previous run's.
		Flag{Name: "record", Type: FlagBool, Default: false, Field: "Record",
			Help: "record this run's PTY output on the server (`record get` reads it back; needs record_persist)"},
		Flag{Name: "no-record", Type: FlagBool, Default: false, Field: "NoRecord",
			Help: "do not record this run's PTY output, whatever the server's default"},
	)
}

// spawnRules are the cross-flag constraints of one spawn verb. They were
// enforced in cmd/harness-cli/session.go alone before the migration, so the
// TUI accepted lines the CLI refused.
func spawnRules(k spawnKind) ([]Rule, []Requirement) {
	// The selector trio names ONE runner on every spawn verb; two of them
	// name two, which is not a narrowing but a contradiction.
	excl := []Rule{{
		Flags:  []string{"runner", "host", "ip"},
		Reason: "they each name one runner, and two of them name two",
	}}
	// --exact takes the task's recorded settings, so every flag that sets one
	// contradicts it. The prompt as trailing words is refused in Validate.
	exactConflicts := []string{"agent", "agent-arg", "caps", "scope", "scope-for", "delegate-caps", "delegate-scope", "spawn-depth", "max-descendants"}
	if k == spawnSubmit {
		exactConflicts = append(exactConflicts, "task")
	}
	for _, f := range exactConflicts {
		excl = append(excl, Rule{Flags: []string{"exact", f}, Reason: "an exact resume takes the task's recorded settings"})
	}
	exactNeeds := Requirement{Flags: []string{"exact"}, Needs: "resume", Reason: "an exact resume names the task it restores"}
	if k == spawnSubmit {
		return excl, []Requirement{exactNeeds} // a queued submit has no terminal, so none of the rest apply
	}
	excl = append(excl,
		Rule{Flags: []string{"x11", "detach"},
			Reason: "a detached session has no client to host the X tunnel"},
		Rule{Flags: []string{"stream", "x11"},
			Reason: "X11 is a terminal-session concept; the server refuses the pair too"},
		Rule{Flags: []string{"background", "foreground"},
			Reason: "they name the two run modes"},
		Rule{Flags: []string{"background", "x11"},
			Reason: "a background session has no client to host the X tunnel"},
		Rule{Flags: []string{"record", "no-record"},
			Reason: "they are the two answers to one question"},
	)
	// A stream session needs --detach or --background (which implies it):
	// checked in spawnValidate, since a Requirement names one flag.
	return excl, []Requirement{exactNeeds}
}

func spawnExclusive(k spawnKind) []Rule { r, _ := spawnRules(k); return r }

func spawnRequires(k spawnKind) []Requirement { _, q := spawnRules(k); return q }

// spawnValidate is the residue: a range on one flag's VALUE, which no
// attribute expresses.
func spawnValidate(b Bound) error {
	if b.Bool("x11") && uintOf(b.Flags["x11-display"]) > 99 {
		return fmt.Errorf("--x11-display must be 0..99")
	}
	if b.Bool("stream") && !b.Bool("detach") && !b.Bool("background") {
		return fmt.Errorf("--stream needs --detach (or --background): an event-stream session has no terminal to attach; follow it with `session stream attach`")
	}
	if te := b.Str("turn-end"); te != "" {
		if _, err := ParseTurnEnd(te); err != nil {
			return err
		}
	}
	return nil
}

// ParseTurnEnd reads --turn-end. "" is the server's default.
func ParseTurnEnd(s string) (protocol.TurnEndSignal, error) {
	switch s {
	case "":
		return protocol.TurnEndSignal_Unset, nil
	case "off":
		return protocol.TurnEndSignal_Off, nil
	case "quiet":
		return protocol.TurnEndSignal_Quiet, nil
	case "lifecycle":
		return protocol.TurnEndSignal_Lifecycle, nil
	case "program":
		return protocol.TurnEndSignal_Program, nil
	case "any":
		return protocol.TurnEndSignal_Any, nil
	}
	return 0, fmt.Errorf("--turn-end %q: want off|quiet|lifecycle|program|any", s)
}

// argListValue backs --agent-arg.
var argListValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// scopeForValue backs --scope-for. The parse and merge happen in the caller's
// package (they need protocol types this one deliberately does not build on),
// so here it accumulates the raw spellings in order.
var scopeForValue = &CustomValue{
	New: func() flag.Value { return &stringList{} },
	Get: func(v flag.Value) any {
		if l, ok := v.(*stringList); ok {
			return []string(*l)
		}
		return []string(nil)
	},
}

// ParseRunModeWord reads `session mode`'s mode positional: true = background.
func ParseRunModeWord(s string) (bool, error) {
	switch s {
	case "background":
		return true, nil
	case "foreground":
		return false, nil
	}
	return false, fmt.Errorf("session mode: want foreground or background, got %q", s)
}
