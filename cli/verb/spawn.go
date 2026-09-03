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
			Name: "repo", Type: FlagString, Default: "", Help: repoHelp,
			// The ladder all three surfaces had their own version of. The
			// workspace tier reached only the CLI before this.
			Resolve: []Tier{
				{Env: "HARNESS_REPO_PATH"},
				{Workspace: "repo"},
				{SurfaceContext: true},
			},
		},
		{Name: "resume", Type: FlagString, Default: "",
			Help: "task id (32 hex) to resume — the server reuses the id and worktree branch, so the agent's project key matches the previous run"},
		{Name: "resume-conversation", Type: FlagBool, Default: false,
			Help: "with --resume, also ask the runner to resume the agent's own conversation state"},
		{Name: "caps", Type: FlagString, Default: "",
			Help: "comma-separated capability names to grant (e.g. spawn,file_read / all / none)"},
		{Name: "scope", Type: FlagString, Default: "",
			Help: "which tasks those capabilities may target; default subtree (self + descendants)"},
		{Name: "agent", Type: FlagString, Default: "",
			Help: "agent profile name (empty = runner default; not to be confused with --agent-arg)"},
		{
			Name: "scope-for", Type: FlagString, Custom: scopeForValue,
			Help: "narrow ONE capability (or a comma-separated list of them) to a tighter scope than --scope",
		},
		{
			// --claude-arg is the deprecated spelling and accumulates into the
			// SAME list: the CLI renamed it and the TUI kept only the old
			// name, which is the drift this declaration ends.
			Name: "agent-arg", Aliases: []string{"claude-arg"}, Type: FlagString, Custom: argListValue,
			Help: "extra CLI arg to forward to the agent (repeatable; appended after the runner-global --agent-args)",
		},
		// Runner pinning. Absent from the TUI's submit before the migration.
		{Name: "runner", Type: FlagString, Default: "",
			Help: "pin to a specific runner by ConnectionID (the id= value from `ls`)"},
		{Name: "host", Type: FlagString, Default: "", Help: "pin to a runner by hostname"},
		{Name: "ip", Type: FlagString, Default: "", Help: "pin to a runner by IP address"},
	}
	if k == spawnSubmit {
		out = append(out, Flag{Name: "task", Type: FlagString, Default: "", Help: "prompt text"})
		return out
	}
	// The PTY verbs: a session can be detached, streamed, X11-forwarded and
	// sized. A queued submit has no terminal for any of that to mean.
	out = append(out,
		Flag{Name: "x11", Type: FlagBool, Default: false,
			Help: "forward X11: inject DISPLAY/XAUTHORITY so GUI apps render on your local X server"},
		Flag{Name: "x11-display", Type: FlagUint, Default: uint(0), Help: "with --x11: the local display number (0..99)"},
	)
	if k == spawnSessionNew {
		out = append(out,
			Flag{Name: "detach", Aliases: []string{"d"}, Type: FlagBool, Default: false,
				Help: "start the session and exit immediately (don't attach the terminal)"},
			Flag{Name: "stream", Type: FlagBool, Default: false,
				Help: "open an event-stream session (structured events, no PTY)"},
			Flag{Name: "rows", Type: FlagUint, Default: uint(0), Help: "initial PTY rows (0 = unset; needs --cols too)"},
			Flag{Name: "cols", Type: FlagUint, Default: uint(0), Help: "initial PTY columns (0 = unset; needs --rows too)"},
		)
	}
	return out
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

// buildSpawn turns a parsed spawn line into the shared action.
func buildSpawn(kind string) func(Bound) (Action, error) {
	return func(b Bound) (Action, error) {
		a := SpawnAction{
			Kind: kind, Repo: b.Str("repo"), ResumeTaskID: b.Str("resume"),
			ResumeConversation: b.Bool("resume-conversation"),
			Agent:              b.Str("agent"),
			Runner:             b.Str("runner"), Host: b.Str("host"), IP: b.Str("ip"),
			Detach: b.Bool("detach"), Stream: b.Bool("stream"),
			X11: b.Bool("x11"),
			// Their zero values are meaningful ("none" and "subtree"), so a
			// resume must not re-grant on a flag nobody passed.
			//
			// The scope half is ONE unit: naming either --scope or --scope-for
			// makes it explicit, so a resume re-grants both together. Letting
			// --scope-for alone ride the session default's scope would write an
			// authority that is half typed and half inherited -- the TUI said so
			// in spawnAuthority's comment and derived presence that way, while
			// the CLI checked only "scope" and dropped a lone --scope-for on
			// resume. Same flag, two meanings; the declaration settles it on the
			// side that had the reason written down.
			CapsPresent:  b.Set["caps"],
			ScopePresent: b.Set["scope"] || b.Set["scope-for"],
		}
		if b.Set["caps"] {
			c, err := ParseCaps(b.Str("caps"))
			if err != nil {
				return nil, fmt.Errorf("%s: --caps: %w", kind, err)
			}
			a.Caps = &c
		}
		if b.Set["scope"] {
			sc, err := ParseScope(b.Str("scope"))
			if err != nil {
				return nil, fmt.Errorf("%s: --scope: %w", kind, err)
			}
			a.Scope = &sc
		}
		if specs, ok := b.Custom["scope-for"].([]string); ok {
			for _, one := range specs {
				_, ov, perr := ParseScopeFor(one)
				if perr != nil {
					return nil, fmt.Errorf("%s: --scope-for: %w", kind, perr)
				}
				merged, merr := MergeScopeOverride(a.Overrides, ov)
				if merr != nil {
					return nil, fmt.Errorf("%s: --scope-for: %w", kind, merr)
				}
				a.Overrides = merged
			}
		}
		if v, ok := b.Custom["agent-arg"].([]string); ok {
			a.ExtraArgs = v
		}
		if r, ok := b.Flags["rows"].(uint); ok {
			a.Rows = r
		}
		if c, ok := b.Flags["cols"].(uint); ok {
			a.Cols = c
		}
		if d, ok := b.Flags["x11-display"].(uint); ok {
			a.X11Display = d
		}
		// Cross-flag validity, stated once for every surface. Each of these
		// was enforced in cmd/harness-cli/session.go alone.
		if a.X11 && a.Detach {
			return nil, fmt.Errorf("%s: --x11 is incompatible with --detach (a detached session has no client to host the X tunnel)", kind)
		}
		if a.X11 && a.X11Display > 99 {
			return nil, fmt.Errorf("%s: --x11-display must be 0..99", kind)
		}
		// The selector trio names ONE runner; two of them name two, which is
		// not a narrowing but a contradiction.
		picked := 0
		for _, on := range []bool{a.Runner != "", a.Host != "", a.IP != ""} {
			if on {
				picked++
			}
		}
		if picked > 1 {
			return nil, fmt.Errorf("%s: --runner, --host and --ip are mutually exclusive", kind)
		}
		// A streamed session has no PTY to hand a terminal, so it can only be
		// opened detached and followed afterwards.
		if a.Stream && !a.Detach {
			return nil, fmt.Errorf("%s: --stream requires --detach (an event-stream session has no terminal to attach; follow it with `session stream attach`)", kind)
		}
		if a.Stream && a.X11 {
			return nil, fmt.Errorf("%s: --stream is incompatible with --x11 (X11 is a terminal-session concept; the server refuses the pair too)", kind)
		}
		a.Task = b.Str("task")
		if a.Task == "" {
			a.Task = b.Trail
		}
		if kind == "submit" && a.Task == "" {
			return nil, fmt.Errorf("submit: a prompt is required, as --task or as the trailing words")
		}
		return a, nil
	}
}
