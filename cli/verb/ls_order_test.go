package verb

import (
	"strings"
	"testing"
)

// --tree is its own order, so `ls` refuses it with --order; --json combines.
func TestLsOrderExclusiveWithTree(t *testing.T) {
	if _, err := ParseCmdLs(CLI, []string{"--tree", "--order", "live"}, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("--tree --order: err = %v, want mutually exclusive", err)
	}
	a, err := ParseCmdLs(CLI, []string{"--json", "--order", "repo"}, nil)
	if err != nil || !a.JSON || a.Order != "repo" {
		t.Errorf("--json --order repo = %+v, %v", a, err)
	}
}
