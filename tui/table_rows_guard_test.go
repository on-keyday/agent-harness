package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every pane must set its table's rows through setTableRows, which carries the
// negative-cursor lift. A raw `table.SetRows` renders one row short with a
// blank last slot for the rest of the session once that table has ever been
// empty — and it can be, on every one of them: no conns, no forwards, no
// execs, no topics, a fresh server, everything pruned.
//
// A grep guard rather than trust, because the three-line form of this fix was
// applied to the tasks table in `db6fa142` and then NOT applied to the runners
// table when that pane gained a conditional column set months later. The same
// shape as TestNoHandWrittenVerbFlagSetsRemain and TestNoHandWrittenTrsfRowsRemain.
//
// Exempt: the helper itself, and the deliberate empty that precedes a column
// swap — those are followed by a rebuild that goes through the helper, and the
// lift is a no-op on an empty table anyway.
func TestNoRawTableSetRowsRemain(t *testing.T) {
	const exemptEmpty = ".SetRows(nil)"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "columns.go" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, ".SetRows(") || strings.Contains(trimmed, exemptEmpty) {
				continue
			}
			// A model's OWN SetRows method (a.tasks.SetRows / a.runners.SetRows)
			// is the pane's public entry point, not a bubbles table call; those
			// funnel into rebuild.
			if strings.Contains(trimmed, "a.tasks.SetRows(") || strings.Contains(trimmed, "a.runners.SetRows(") {
				continue
			}
			t.Errorf("%s:%d sets table rows directly — use setTableRows so the "+
				"negative-cursor lift cannot be forgotten:\n\t%s", name, i+1, trimmed)
		}
	}
}
