package verb

import (
	"flag"
	"fmt"
)

// The spawn family -- submit, interactive, session new -- shares almost all of
// its grammar, so it is built from one list rather than three that drift. The
// differences are real and few, and each is named below.
type spawnKind int

const (
	spawnSubmit spawnKind = iota
	spawnInteractive
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
		{
			// --claude-arg is the deprecated spelling and accumulates into the
			// SAME list: the CLI renamed it and the TUI kept only the old
			// name, which is the drift this declaration ends.
			Name: "agent-arg", Aliases: []string{"claude-arg"}, Type: FlagString, Custom: argListValue, Field: "ExtraArgs",
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
	// The PTY verbs: a session can be detached, streamed, X11-forwarded and
	// sized. A queued submit has no terminal for any of that to mean.
	out = append(out,
		Flag{Name: "x11", Type: FlagBool, Default: false, Field: "X11",
			Help: "forward X11: inject DISPLAY/XAUTHORITY so GUI apps render on your local X server"},
		// 10, not 0: the runner binds 127.0.0.1:6000+N, and ssh picks 10 by
		// convention precisely so a forward does not land on the runner's own
		// :0 X port.
		Flag{Name: "x11-display", Type: FlagUint, Default: uint(10), Field: "X11Display",
			Help: "with --x11: the local display number (0..99, default 10)"},
	)
	if k == spawnSessionNew {
		out = append(out,
			Flag{Name: "detach", Aliases: []string{"d"}, Type: FlagBool, Default: false, Field: "Detach",
				Help: "start the session and exit immediately (don't attach the terminal)"},
			Flag{Name: "stream", Type: FlagBool, Default: false, Field: "Stream",
				Help: "open an event-stream session (structured events, no PTY)"},
			Flag{Name: "rows", Type: FlagUint, Default: uint(0), Field: "Rows", Help: "initial PTY rows (0 = unset; needs --cols too)"},
			Flag{Name: "cols", Type: FlagUint, Default: uint(0), Field: "Cols", Help: "initial PTY columns (0 = unset; needs --rows too)"},
		)
	}
	return out
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
	if k == spawnSubmit {
		return excl, nil // a queued submit has no terminal, so none of the rest apply
	}
	if k != spawnSessionNew {
		// --detach and --stream exist only on `session new`, so the rules
		// naming them belong there and nowhere else. Declaring them on
		// `interactive` looked harmless -- b.Set is false for a flag the verb
		// does not have -- but it reads as a constraint an operator could
		// trip, and there is no line that trips it.
		return excl, nil
	}
	excl = append(excl,
		Rule{Flags: []string{"x11", "detach"},
			Reason: "a detached session has no client to host the X tunnel"},
		Rule{Flags: []string{"stream", "x11"},
			Reason: "X11 is a terminal-session concept; the server refuses the pair too"},
	)
	return excl, []Requirement{{
		Flags: []string{"stream"}, Needs: "detach",
		Reason: "an event-stream session has no terminal to attach; follow it with `session stream attach`",
	}}
}

func spawnExclusive(k spawnKind) []Rule { r, _ := spawnRules(k); return r }

func spawnRequires(k spawnKind) []Requirement { _, q := spawnRules(k); return q }

// spawnValidate is the residue: a range on one flag's VALUE, which no
// attribute expresses.
func spawnValidate(b Bound) error {
	if b.Bool("x11") && uintOf(b.Flags["x11-display"]) > 99 {
		return fmt.Errorf("--x11-display must be 0..99")
	}
	return nil
}

// argListValue backs --agent-arg / --claude-arg.
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
