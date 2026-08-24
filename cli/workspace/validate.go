package workspace

import (
	"fmt"
	"strings"

	"github.com/google/shlex"
	"github.com/on-keyday/agent-harness/cli"
)

// ForwardDir distinguishes the two forward flags a workspace may carry. -W is
// deliberately absent: it splices one process's stdin/stdout and has no meaning
// in a file that describes long-lived listeners.
//
// NOTE this is not tui.ForwardDirection. The two enums name the same two
// directions in different packages; do not mix them.
type ForwardDir int

const (
	ForwardLocal ForwardDir = iota
	ForwardRemote
)

// ParseForwardValue splits a `forward` value into its flag and its spec, and
// hands the spec to the same parser `harness-cli forward` uses.
func ParseForwardValue(value string) (ForwardDir, cli.ForwardSpec, cli.RemoteForwardSpec, error) {
	flag, rest, ok := strings.Cut(strings.TrimSpace(value), " ")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return 0, cli.ForwardSpec{}, cli.RemoteForwardSpec{},
			fmt.Errorf("forward = %q: want `-L [bind:]localport:remotehost:remoteport` or `-R [bind:]runnerport:dialhost:dialport`", value)
	}
	switch flag {
	case "-L":
		sp, err := cli.ParseForwardSpec(rest)
		return ForwardLocal, sp, cli.RemoteForwardSpec{}, err
	case "-R":
		sp, err := cli.ParseRemoteForwardSpec(rest)
		return ForwardRemote, cli.ForwardSpec{}, sp, err
	}
	return 0, cli.ForwardSpec{}, cli.RemoteForwardSpec{},
		fmt.Errorf("forward = %q: only -L and -R are accepted here", value)
}

// GridArgs splits the workspace's grid value the way a shell would, so the
// value is written exactly as it is typed after `grid`.
func (w *Workspace) GridArgs() ([]string, error) {
	if !w.GridSet || strings.TrimSpace(w.Grid) == "" {
		return nil, nil
	}
	return shlex.Split(w.Grid)
}

// Validate checks every value this workspace carries against the parser that
// owns its grammar. Callers run it once after Parse; an apply then works from
// values already known to be well formed.
func (w *Workspace) Validate() error {
	if w.GridSet {
		args, err := w.GridArgs()
		if err != nil {
			return fmt.Errorf("workspace %s: grid: %w", w.Name, err)
		}
		if _, _, _, err := cli.ParseGridArgs(args); err != nil {
			return fmt.Errorf("workspace %s: %w", w.Name, err)
		}
	}
	for _, t := range w.Tasks {
		for _, fw := range t.Forwards {
			if _, _, _, err := ParseForwardValue(fw); err != nil {
				return fmt.Errorf("workspace %s task %s: %w", w.Name, t.ID[:8], err)
			}
		}
	}
	return nil
}
