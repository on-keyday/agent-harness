package verb

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseFileTransferRoute(t *testing.T) {
	for in, want := range map[string]protocol.FileTransferRoute{
		"":          protocol.FileTransferRoute_Splice,
		"splice":    protocol.FileTransferRoute_Splice,
		"forwarded": protocol.FileTransferRoute_Forwarded,
		"direct":    protocol.FileTransferRoute_Direct,
	} {
		got, err := ParseFileTransferRoute(in)
		if err != nil || got != want {
			t.Errorf("%q -> %v, %v; want %v", in, got, err, want)
		}
	}
}

// A typo must not fall through to splice. The two non-default routes exist to
// keep the server from reading these bytes, so a misspelling that quietly
// spliced would hand over exactly what the caller withheld -- and say nothing.
func TestParseFileTransferRouteRefusesATypo(t *testing.T) {
	for _, in := range []string{"dircet", "spilce", "forward", "relay", "true", "1"} {
		if _, err := ParseFileTransferRoute(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}
