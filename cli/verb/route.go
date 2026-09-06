package verb

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ParseFileTransferRoute turns the operator's word into the route the request
// carries. It lives here, in the lower package, because cli imports verb and
// not the other way round -- and because the alternative is the list of
// spellings written down twice, which is how the CLI and the TUI drift into
// accepting different words for the same three paths.
//
// An unknown word is an ERROR, never a fall-through to splice. Two of the three
// routes exist to keep the server from reading these bytes, so a typo that
// quietly spliced would hand over exactly what the caller withheld, and would
// say nothing about it.
func ParseFileTransferRoute(s string) (protocol.FileTransferRoute, error) {
	switch s {
	case "", "splice":
		return protocol.FileTransferRoute_Splice, nil
	case "forwarded":
		return protocol.FileTransferRoute_Forwarded, nil
	case "direct":
		return protocol.FileTransferRoute_Direct, nil
	default:
		return protocol.FileTransferRoute_Splice,
			fmt.Errorf("unknown route %q: want splice, forwarded or direct", s)
	}
}

// validateRoute refuses a bad route word at BIND time, before anything dials.
// Checking it inside the action would open a connection first and report the
// typo afterwards, which is a round trip spent on a request that was never
// going to be sent.
func validateRoute(b Bound) error {
	if _, err := ParseFileTransferRoute(b.Str("route")); err != nil {
		return fmt.Errorf("--route: %w", err)
	}
	return nil
}
