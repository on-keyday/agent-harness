package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

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

// CapDescription returns a one-line summary of what a single granular
// capability authorizes. Descriptions mirror the server-side enforcement
// points (server/capabilities.go requiredCap + server/task_handler.go
// direction checks + agent_handler.go topic gating), so they describe the
// actual gate, not an aspiration.
func CapDescription(c protocol.Capability) string {
	switch c {
	case protocol.Capability_None:
		return "no capabilities; data-plane only (agentboard messaging, own task logs/ls)"
	case protocol.Capability_Spawn:
		return "submit tasks and open interactive sessions"
	case protocol.Capability_Cancel:
		return "cancel / kill tasks"
	case protocol.Capability_ExecAttach:
		return "attach to a session's PTY"
	case protocol.Capability_FileRead:
		return "read files from task worktrees (file pull / ls)"
	case protocol.Capability_FileWrite:
		return "write or delete files in task worktrees (file push / delete)"
	case protocol.Capability_ForwardLocal:
		return "open local port forwards (-L)"
	case protocol.Capability_ForwardRemote:
		return "open remote port forwards (-R)"
	case protocol.Capability_Notify:
		return "send operator notifications"
	case protocol.Capability_Prune:
		return "prune terminal tasks"
	case protocol.Capability_RunnerAdmin:
		return "runner administration (server dial-runner)"
	case protocol.Capability_InfoGlobal:
		return "see all tasks and agentboard topics globally (not just own subtree)"
	case protocol.Capability_All:
		return "full capability set (operator-equivalent)"
	default:
		return ""
	}
}

// CapInfo is the machine-readable description of one capability, emitted by
// `harness-cli caps --json`.
type CapInfo struct {
	Name        string `json:"name"`
	Bit         uint32 `json:"bit"`
	Description string `json:"description"`
}

// CapsCatalog returns every grantable capability with its bit value and
// description, in GrantableCaps() order (none … all).
func CapsCatalog() []CapInfo {
	caps := GrantableCaps()
	out := make([]CapInfo, len(caps))
	for i, c := range caps {
		out[i] = CapInfo{Name: c.String(), Bit: uint32(c), Description: CapDescription(c)}
	}
	return out
}

// WriteCaps renders the capability catalog to w: an aligned table by default,
// or indented JSON when asJSON is set. Backs `harness-cli caps`.
func WriteCaps(w io.Writer, asJSON bool) error {
	cat := CapsCatalog()
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(cat)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAPABILITY\tBIT\tDESCRIPTION")
	for _, ci := range cat {
		fmt.Fprintf(tw, "%s\t0x%03x\t%s\n", ci.Name, ci.Bit, ci.Description)
	}
	return tw.Flush()
}

// CapNames returns the string representation of each capability.
func CapNames(caps []protocol.Capability) []string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = c.String()
	}
	return names
}

// CapsLabel renders a capability bitmask as "all", "none", or a comma-joined
// list of the set granular cap names (from Capability.String()). Single source
// of names — no literal map.
func CapsLabel(c protocol.Capability) string {
	if c == protocol.Capability_All {
		return "all"
	}
	if c == protocol.Capability_None {
		return "none"
	}
	var names []string
	for _, g := range GrantableCaps() {
		if g == protocol.Capability_None || g == protocol.Capability_All {
			continue
		}
		if c&g == g {
			names = append(names, g.String())
		}
	}
	return strings.Join(names, ",")
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
