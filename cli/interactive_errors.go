package cli

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerCandidate is one runner that tied for a repo when an interactive open
// was ambiguous. Cid is a ConnectionID string — pass it verbatim as
// SelectorOpts{Runner: Cid} to pin a retry.
type RunnerCandidate struct {
	Cid         string
	Hostname    string
	MatchedRoot string
	ActiveTasks int
	MaxTasks    int
}

// AmbiguousRunnerError is returned when opening/resuming an interactive session
// matched >=2 runners under an Any selector. Callers (TUI/CLI/WebUI) use
// errors.As to pull out Candidates and let the user pick one.
type AmbiguousRunnerError struct {
	Candidates []RunnerCandidate
}

func (e *AmbiguousRunnerError) Error() string {
	return fmt.Sprintf("ambiguous_runner: %d runners match; pick one (or pin with --runner/--host/--ip)", len(e.Candidates))
}

// candidatesFromResponse maps the wire candidates into the client-facing slice.
// NOTE (from Task 2): the generated getter Candidates() returns a POINTER
// (*[]protocol.RunnerCandidate) — nil unless Status == ambiguous_runner — so
// deref with a nil guard.
func candidatesFromResponse(oir *protocol.OpenInteractiveResponse) []RunnerCandidate {
	cands := oir.Candidates()
	if cands == nil {
		return nil
	}
	out := make([]RunnerCandidate, 0, len(*cands))
	for _, c := range *cands {
		out = append(out, RunnerCandidate{
			Cid:         string(c.Cid),
			Hostname:    string(c.Hostname),
			MatchedRoot: string(c.MatchedRoot),
			ActiveTasks: int(c.ActiveTasks),
			MaxTasks:    int(c.MaxTasks),
		})
	}
	return out
}
