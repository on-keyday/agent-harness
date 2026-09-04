package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The listing's empty case is the whole reason RestoreWALStatus exists: one
// sentence used to cover four states, three of which are failures that
// measured nothing. These check that the operator can TELL THEM APART -- so
// they assert the lines differ from each other, not just that each is
// non-empty, because "nothing to put back" four times would satisfy that.
func TestEmptyRestorableSaysWhichOfTheFour(t *testing.T) {
	seen := map[string]protocol.RestoreWALStatus{}
	for _, status := range []protocol.RestoreWALStatus{
		protocol.RestoreWALStatus_Ok,
		protocol.RestoreWALStatus_NoDataDir,
		protocol.RestoreWALStatus_Missing,
		protocol.RestoreWALStatus_Unreadable,
	} {
		var b bytes.Buffer
		WriteRestorable(nil, status, &b)
		line := strings.TrimSpace(b.String())
		if line == "" {
			t.Fatalf("%v: printed nothing", status)
		}
		if prev, dup := seen[line]; dup {
			t.Errorf("%v and %v print the SAME line, which is the bug:\n  %s", prev, status, line)
		}
		seen[line] = status
	}

	// The three failures must not read as a measurement. "no prune is
	// standing" is a claim about what was pruned, and only Ok has grounds
	// for it.
	for _, tc := range []struct {
		status protocol.RestoreWALStatus
		want   string
	}{
		{protocol.RestoreWALStatus_NoDataDir, "--data-dir"},
		{protocol.RestoreWALStatus_Missing, "events.log"},
		{protocol.RestoreWALStatus_Unreadable, "parse"},
	} {
		var b bytes.Buffer
		WriteRestorable(nil, tc.status, &b)
		got := b.String()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%v does not name %q, so it does not say what to check:\n  %s",
				tc.status, tc.want, strings.TrimSpace(got))
		}
	}
}

// A non-empty listing renders the rows regardless of status: the status
// answers "why is this empty", and letting it swallow rows would hide the
// very ids the verb exists to reveal.
func TestRestorableRowsSurviveEveryStatus(t *testing.T) {
	rows := []RestorableTask{{TaskID: strings.Repeat("a", 32), RepoPath: "/r", Prompt: "p"}}
	for _, status := range []protocol.RestoreWALStatus{
		protocol.RestoreWALStatus_Ok,
		protocol.RestoreWALStatus_NoDataDir,
		protocol.RestoreWALStatus_Missing,
		protocol.RestoreWALStatus_Unreadable,
	} {
		var b bytes.Buffer
		WriteRestorable(rows, status, &b)
		if !strings.Contains(b.String(), rows[0].TaskID) {
			t.Errorf("%v: the row is missing from the listing", status)
		}
	}
}
