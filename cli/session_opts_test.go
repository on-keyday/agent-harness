package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Caps is a plain value, not a pointer, and that is a security property rather
// than a simplification: the default is Capability_None, which IS the zero
// value, so "unset" and "explicitly --caps none" mean the same thing and there
// is nothing left for a presence bit to disambiguate. The consequence pinned
// here is that a SessionOpts nobody filled in spawns a task with NO
// capabilities — a caller that forgets the field fails closed. (It was a
// pointer while the default was Capability_All = 4095, where unset and zero
// had to be told apart or "--caps none" would have read as inherit-all.)
func TestSessionOptsCapsResolution(t *testing.T) {
	narrow := protocol.Capability_Spawn | protocol.Capability_FileRead

	tests := []struct {
		name string
		opts SessionOpts
		want protocol.Capability
	}{
		{"zero value grants nothing", SessionOpts{}, protocol.Capability_None},
		{"explicit none stays none", SessionOpts{Caps: protocol.Capability_None}, protocol.Capability_None},
		{"explicit narrow mask preserved", SessionOpts{Caps: narrow}, narrow},
		{"explicit all is all", SessionOpts{Caps: protocol.Capability_All}, protocol.Capability_All},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Both wire builders must carry the value, not re-derive it.
			if got := buildSubmitRequest("/repo", "p", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildSubmitRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
			if got := buildOpenInteractiveRequest("/repo", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildOpenInteractiveRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
		})
	}
}
