package tui

import (
	"fmt"
	"strings"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Action is the typed result of parsing one cmdline input; app.go switches on
// the concrete type. It is an alias of the shared interface (cli/verb), so a
// verb declared once is one type on every surface.
//
// The TUI keeps its own screen-state actions -- Clear, Quit, Help, Refresh,
// Repo, TrsfDebug, GridDiag, Grid -- which satisfy it by embedding
// verb.ActionMarker. That marker is exported for exactly this reason: an
// unexported method declared in cli/verb could not be implemented from here.
type Action = verb.Action

// CapsAction sets or shows the session-default capability mask applied to
// spawns that do not carry their own --caps.
// Show=true (no args): display current caps in the status line.
// Show=false: update sessionCaps to Caps.
//
// The default deliberately does not apply on resume — a resumed task keeps its
// persisted caps unless the resuming command names --caps explicitly. The old
// `caps --on-resume on` toggle re-granted from whatever the mode happened to
// hold at the time, silently, on every resume; it rewrote at least one live
// task's caps by accident and is gone.
type CapsAction struct {
	verb.ActionMarker
	Caps protocol.Capability
	Show bool // true = display current set (no args), false = set to Caps
}

// ScopeAction is the target-set companion to CapsAction: caps say which verbs
// a spawned task may use, scope says which tasks it may point them at. Same
// show/set shape, same "does not apply on resume" rule.
type ScopeAction struct {
	verb.ActionMarker
	Scope     protocol.TaskScope
	Overrides []protocol.ScopeOverride
	Show      bool
}

// ParseCommand tokenizes and parses one input line. defaultRepo is used when
// `submit` is invoked without --repo (typically the cwd).
// Returns (nil, nil) for empty / whitespace-only input.
func ParseCommand(input, defaultRepo string) (Action, error) {
	tokens, err := shlex.Split(input)
	if err != nil {
		return nil, fmt.Errorf("shlex: %w", err)
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	// Every verb the declaration gives this surface, parsed by the GENERATED
	// entry: it finds the path itself (longest first, so `session stream turn`
	// beats `session`), narrows to TUI, applies the surface-context tier of
	// --repo's ladder, and returns the Action already typed.
	//
	// This switch used to restate the table -- twenty-odd cases and seventeen
	// helper functions differing only in which spec they looked up -- and
	// every drift the migration found lived in the difference between the two
	// copies.
	if act, handled, perr := verb.ParseTUICommand(tokens, map[string]string{"repo": defaultRepo}); handled {
		return act, perr
	}

	// The two shapes the generated prefix match cannot reach on its own.
	switch tokens[0] {
	case "git":
		// `git <task-id> <sub>` puts the id in the MIDDLE of the path, so no
		// prefix of the line is the verb. Peeled here exactly as the CLI and
		// the WebUI peel it, then parsed through the same generated entry.
		if len(tokens) < 3 {
			return nil, fmt.Errorf("git: usage: git <task-id> {log | diff | show | status | subrepos | file} ...")
		}
		act, handled, perr := verb.ParseTUICommand(append([]string{"git", tokens[2]}, tokens[3:]...), nil)
		if perr != nil {
			return nil, perr
		}
		if !handled {
			return nil, fmt.Errorf("git: unknown sub-verb %q", tokens[2])
		}
		g := act.(verb.GitAction)
		g.TaskID = tokens[1]
		return g, nil
	case "session":
		// requests / snapshot are specified and not built (design §3). Naming
		// one has to say THAT rather than "unknown".
		if len(tokens) >= 3 && tokens[1] == "stream" &&
			(tokens[2] == "requests" || tokens[2] == "snapshot") {
			return nil, fmt.Errorf("session stream %s: specified (design §3) but not built yet", tokens[2])
		}
		return nil, fmt.Errorf("session: unknown sub-verb %q", strings.Join(tokens[1:], " "))
	case "caps":
		// `caps <mask>` and `scope <mask>` set this TUI session's spawn
		// defaults. They are the one pair the design says should be SHARED
		// (the WebUI has the same state as chips) and are not declared yet --
		// tracked as `caps set-defaults`. Until then they stay here, and
		// saying so is the difference between a known gap and a silent one.
		if len(tokens) == 1 {
			return CapsAction{Show: true}, nil
		}
		if tokens[1] == "--on-resume" {
			return nil, fmt.Errorf("caps --on-resume was removed: pass --caps on the resuming command instead (e.g. `session new --resume <id> --caps all,-spawn`), which re-grants that mask for that one resume")
		}
		c, err := cli.ParseCaps(strings.Join(tokens[1:], ""))
		if err != nil {
			return nil, err
		}
		return CapsAction{Caps: c}, nil
	case "scope":
		if len(tokens) == 1 {
			return ScopeAction{Show: true}, nil
		}
		sc, err := cli.ParseScope(strings.Join(tokens[1:], ""))
		if err != nil {
			return nil, err
		}
		return ScopeAction{Scope: sc}, nil
	}
	return nil, fmt.Errorf("unknown command: %q", tokens[0])
}

// capsLabel formats a Capability bitmask for human display:
// "all", "none", or a comma-joined list of the enabled granular caps.
func capsLabel(c protocol.Capability) string {
	if c == protocol.Capability_All {
		return "all"
	}
	if c == protocol.Capability_None {
		return "none"
	}
	var names []string
	for _, g := range cli.GrantableCaps() {
		if g == protocol.Capability_None || g == protocol.Capability_All {
			continue
		}
		if c&g == g {
			names = append(names, g.String())
		}
	}
	return strings.Join(names, ",")
}

// parseFile, parseExecRun and parseSSHGateway are TEST entry points into one
// family's grammar. The verbs themselves parse through
// verb.ParseTUICommand like every other; these exist because the tests
// predate it and drive one family with an argument slice rather than a line.
//
// Deliberately thin: they join the family word back on and go through the same
// generated parser, so a test cannot exercise a path the command line does
// not reach -- which is what the seventeen deleted helpers allowed.
func parseFile(args []string) (Action, error) {
	return parseFamilyForTest("file", args)
}

func parseExecRun(args []string) (Action, error) {
	return parseFamilyForTest("exec", args)
}

func parseSSHGateway(args []string) (Action, error) {
	if len(args) == 0 {
		args = []string{"status"} // bare `ssh-gateway` reports
	}
	return parseFamilyForTest("ssh-gateway", args)
}

func parseFamilyForTest(family string, args []string) (Action, error) {
	act, handled, err := verb.ParseTUICommand(append([]string{family}, args...), nil)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, fmt.Errorf("%s: not a declared verb for this surface", family)
	}
	return act, nil
}
