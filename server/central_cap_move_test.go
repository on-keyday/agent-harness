package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestCentralCapMatchesRequiredCap proves the move of requiredCap into
// authority was verbatim. It lives only until requiredCap is deleted and the
// central gate reads authority.CentralCap; then there is one map and nothing
// to compare.
func TestCentralCapMatchesRequiredCap(t *testing.T) {
	for k := protocol.TaskControlKind(0); k <= protocol.TaskControlKind_TaskRename; k++ {
		want, wantOK := requiredCap[k]
		got, gotOK := authority.CentralCap(k)
		if got != want || gotOK != wantOK {
			t.Errorf("%v: authority.CentralCap = (%v, %t), requiredCap = (%v, %t)", k, got, gotOK, want, wantOK)
		}
	}
}
