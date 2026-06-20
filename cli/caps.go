package cli

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GrantableCaps lists the individual capability values a --caps flag (or a UI)
// may name. Names come from Capability.String() — the single source so they
// never drift from the enum.
func GrantableCaps() []protocol.Capability {
	return []protocol.Capability{
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
}

// CapNames returns the string representation of each capability.
func CapNames(caps []protocol.Capability) []string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = c.String()
	}
	return names
}

// ParseCaps converts a comma-separated list of capability names into a bitmask.
// Empty/whitespace → Capability_All (inherit-all); unknown name → error.
//
// Names are case-sensitive and match the snake_case string representation
// produced by Capability.String() (e.g. "spawn", "file_read", "exec_attach").
func ParseCaps(s string) (protocol.Capability, error) {
	if strings.TrimSpace(s) == "" {
		return protocol.Capability_All, nil // omitted → inherit-all (server intersects with parent's caps)
	}
	grantable := GrantableCaps()
	byName := make(map[string]protocol.Capability, len(grantable))
	for _, c := range grantable {
		byName[c.String()] = c
	}
	var out protocol.Capability
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		c, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q (valid: %s)",
				name, strings.Join(CapNames(grantable), ", "))
		}
		out |= c
	}
	return out, nil
}
