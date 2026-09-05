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

	// The one other shape the generated prefix match cannot reach.
	switch tokens[0] {
	case "session":
		// requests / snapshot are specified and not built (design §3). Naming
		// one has to say THAT rather than "unknown".
		if len(tokens) >= 3 && tokens[1] == "stream" &&
			(tokens[2] == "requests" || tokens[2] == "snapshot") {
			return nil, fmt.Errorf("session stream %s: specified (design §3) but not built yet", tokens[2])
		}
		return nil, fmt.Errorf("session: unknown sub-verb %q", strings.Join(tokens[1:], " "))
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
