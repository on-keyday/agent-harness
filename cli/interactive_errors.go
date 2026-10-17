package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// RunnerCandidate is one runner offered when a spawn was ambiguous: more than
// one (runner, agent profile) choice matched. Profiles are the agents the
// caller may pick on it. RunnerID is the 32-hex identity, empty for a runner
// that predates identity; Cid is its connection, for display and as the pin
// of last resort (see Pin).
type RunnerCandidate struct {
	RunnerID    string
	Cid         string
	Hostname    string
	MatchedRoot string
	ActiveTasks int
	MaxTasks    int
	Profiles    []string
}

// Pin is the selector a pick of this candidate re-issues with: its identity,
// which survives the runner reconnecting, or its connection for a runner
// that predates identity. Every picker goes through here, so no surface
// chooses the pin form itself.
func (c RunnerCandidate) Pin() SelectorOpts {
	if c.RunnerID != "" {
		return SelectorOpts{Runner: c.RunnerID}
	}
	return SelectorOpts{Runner: c.Cid}
}

// RunnerCombo is one (runner, profile) choice out of a candidate list.
type RunnerCombo struct {
	Candidate RunnerCandidate
	Profile   string
}

// FlattenCandidates lists every (runner, profile) choice in candidate order.
func FlattenCandidates(cs []RunnerCandidate) []RunnerCombo {
	var out []RunnerCombo
	for _, c := range cs {
		for _, p := range c.Profiles {
			out = append(out, RunnerCombo{Candidate: c, Profile: p})
		}
	}
	return out
}

// AmbiguousRunnerError is returned when a spawn — an interactive open or a
// submit — matched more than one (runner, agent) choice under an unpinned
// selector. Callers (TUI/CLI/WebUI) use errors.As to pull out Candidates and
// let the user pick one.
type AmbiguousRunnerError struct {
	Candidates []RunnerCandidate
}

func (e *AmbiguousRunnerError) Error() string {
	return fmt.Sprintf("ambiguous_runner: %d runners match; pick one (or pin with --runner/--host/--ip and --agent)", len(e.Candidates))
}

func candidatesFromBody(body *protocol.RunnerCandidatesBody) []RunnerCandidate {
	out := make([]RunnerCandidate, 0, len(body.Candidates))
	for _, c := range body.Candidates {
		rc := RunnerCandidate{
			Cid: string(c.Cid), Hostname: string(c.Hostname), MatchedRoot: string(c.MatchedRoot),
			ActiveTasks: int(c.ActiveTasks), MaxTasks: int(c.MaxTasks),
		}
		if !c.RunnerId.IsZero() {
			rc.RunnerID = c.RunnerId.Hex()
		}
		for _, p := range c.Profiles {
			rc.Profiles = append(rc.Profiles, string(p.Name))
		}
		out = append(out, rc)
	}
	return out
}

// readCandidates reads the candidate body an ambiguous response names. Every
// ambiguous answer — interactive and submit, native and wasm — goes through
// here.
func readCandidates(ctx context.Context, tr trsf.Transport, streamID *uint64) ([]RunnerCandidate, error) {
	if streamID == nil {
		return nil, fmt.Errorf("ambiguous_runner without a candidate stream")
	}
	var body protocol.RunnerCandidatesBody
	if err := ReadResponseBody(ctx, tr, *streamID, &body); err != nil {
		return nil, fmt.Errorf("ambiguous_runner: candidates: %w", err)
	}
	return candidatesFromBody(&body), nil
}
