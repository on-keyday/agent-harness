package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The Caps pointer is a security boundary, not a convenience. Capability_All is
// 4095 (not the zero value) and Capability(0) is a real user value ("--caps
// none" = maximal confinement). A plain Capability field could not tell "unset"
// from "explicitly none": a zero default would silently confine every task, and
// normalizing 0→All would turn "--caps none" into a privilege escalation. These
// tests pin both directions through the actual request builders.
func TestSessionOptsCapsResolution(t *testing.T) {
	confining := protocol.Capability(0)
	narrow := protocol.Capability_Spawn | protocol.Capability_FileRead

	tests := []struct {
		name string
		opts SessionOpts
		want protocol.Capability
	}{
		{"unset (nil) inherits all", SessionOpts{}, protocol.Capability_All},
		{"explicit none stays none (no escalation)", SessionOpts{Caps: CapsPtr(confining)}, confining},
		{"explicit narrow mask preserved", SessionOpts{Caps: CapsPtr(narrow)}, narrow},
		{"explicit all is all", SessionOpts{Caps: CapsPtr(protocol.Capability_All)}, protocol.Capability_All},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.resolvedCaps(); got != tc.want {
				t.Errorf("resolvedCaps() = %v, want %v", got, tc.want)
			}
			// Both wire builders must carry the same resolved value, not re-derive it.
			if got := buildSubmitRequest("/repo", "p", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildSubmitRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
			if got := buildOpenInteractiveRequest("/repo", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildOpenInteractiveRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
		})
	}
}
