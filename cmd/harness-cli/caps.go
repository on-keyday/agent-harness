//go:build !js

package main

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// grantableCaps lists the individual capability values that --caps may name.
// Names come from Capability.String() — the single source so they can never
// drift from the enum definition.
var grantableCaps = []protocol.Capability{
	protocol.Capability_None,
	protocol.Capability_Spawn,
	protocol.Capability_Cancel,
	protocol.Capability_ExecAttach,
	protocol.Capability_FileRead,
	protocol.Capability_FileWrite,
	protocol.Capability_ForwardLocal,
	protocol.Capability_ForwardRemote,
	protocol.Capability_Notify,
	protocol.Capability_Prune,
	protocol.Capability_RunnerAdmin,
	protocol.Capability_InfoGlobal,
	protocol.Capability_All,
}

// capNames returns the string representation of each capability in the slice.
func capNames(caps []protocol.Capability) []string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = c.String()
	}
	return names
}

// parseCaps converts a comma-separated list of capability names into a
// Capability bitmask.
//
//   - Empty (or whitespace-only) string → Capability_All (inherit-all).
//   - Comma-separated names (e.g. "spawn,file_read") → OR of the matched bits.
//   - Unknown name → error listing valid names (sourced from Capability.String()).
//
// Names are case-sensitive and match the snake_case string representation
// produced by Capability.String() (e.g. "spawn", "file_read", "exec_attach").
func parseCaps(s string) (protocol.Capability, error) {
	if strings.TrimSpace(s) == "" {
		return protocol.Capability_All, nil // omitted → inherit-all (server intersects with parent's caps)
	}
	byName := make(map[string]protocol.Capability, len(grantableCaps))
	for _, c := range grantableCaps {
		byName[c.String()] = c
	}
	var out protocol.Capability
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		c, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q (valid: %s)",
				name, strings.Join(capNames(grantableCaps), ", "))
		}
		out |= c
	}
	return out, nil
}
