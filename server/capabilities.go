package server

import "github.com/on-keyday/agent-harness/runner/protocol"

// hasCap reports whether have includes every bit in want.
func hasCap(have, want protocol.Capability) bool {
	return have&want == want
}

// intersectCaps is spawn-time attenuation: a child receives the bits its
// creator holds AND requested. Monotonically non-increasing.
func intersectCaps(creator, requested protocol.Capability) protocol.Capability {
	return creator & requested
}
