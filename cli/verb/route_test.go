package verb

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseDataPlaneRoute(t *testing.T) {
	for in, want := range map[string]protocol.DataPlaneRoute{
		"":          protocol.DataPlaneRoute_Splice,
		"splice":    protocol.DataPlaneRoute_Splice,
		"forwarded": protocol.DataPlaneRoute_Forwarded,
		"direct":    protocol.DataPlaneRoute_Direct,
	} {
		got, err := ParseDataPlaneRoute(in)
		if err != nil || got != want {
			t.Errorf("%q -> %v, %v; want %v", in, got, err, want)
		}
	}
}

// A typo must not fall through to splice. The two non-default routes exist to
// keep the server from reading these bytes, so a misspelling that quietly
// spliced would hand over exactly what the caller withheld -- and say nothing.
func TestParseDataPlaneRouteRefusesATypo(t *testing.T) {
	for _, in := range []string{"dircet", "spilce", "forward", "relay", "true", "1"} {
		if _, err := ParseDataPlaneRoute(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}
