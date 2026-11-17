package cli

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerStatusWord is a runner status as the runner tables and the overview
// print it.
func RunnerStatusWord(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle"
	case protocol.RunnerStatus_Busy:
		return "Busy"
	default:
		return "Offline"
	}
}

// RunnerStatusCounts counts rs by RunnerStatusWord's three words.
func RunnerStatusCounts(rs []protocol.RunnerInfo) (idle, busy, offline int) {
	for _, r := range rs {
		switch RunnerStatusWord(r.Status) {
		case "Idle":
			idle++
		case "Busy":
			busy++
		default:
			offline++
		}
	}
	return idle, busy, offline
}

// RunnerSummary is the one-line fleet state the TUI shows above whichever
// table is visible: "runners N: X idle Y busy …". The labels are
// RunnerStatusWord's, lowercased, so the vocabulary is the table's. Idle and
// busy exist on every fleet and keep a zero; offline appears only when present.
func RunnerSummary(rs []protocol.RunnerInfo) string {
	idle, busy, offline := RunnerStatusCounts(rs)
	var b strings.Builder
	fmt.Fprintf(&b, "runners %d: %d %s %d %s", len(rs),
		idle, strings.ToLower(RunnerStatusWord(protocol.RunnerStatus_Idle)),
		busy, strings.ToLower(RunnerStatusWord(protocol.RunnerStatus_Busy)))
	if offline > 0 {
		fmt.Fprintf(&b, " %d %s", offline, strings.ToLower(RunnerStatusWord(protocol.RunnerStatus_Offline)))
	}
	return b.String()
}
