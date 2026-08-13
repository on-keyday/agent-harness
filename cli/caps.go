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
		protocol.Capability_Purge,
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
	case protocol.Capability_Purge:
		return "purge an agentboard topic's retained-message buffer (agent purge)"
	case protocol.Capability_All:
		return "full capability set (operator-equivalent)"
	default:
		return ""
	}
}

// ScopeInfo is the machine-readable description of one scope form, emitted
// alongside the capability catalog by `harness-cli caps --json`.
type ScopeInfo struct {
	Syntax      string `json:"syntax"`
	Description string `json:"description"`
}

// ScopesCatalog describes what a --scope value may say. A capability names a
// verb; a scope names which tasks that verb may be pointed at, and without one
// every granted bit reaches every task on the server.
func ScopesCatalog() []ScopeInfo {
	return []ScopeInfo{
		{"subtree", "self + every task it spawned, transitively (the default when --scope is omitted)"},
		{"none", "self only; a task that may create children but not supervise them"},
		{"global", "every task on the server; the explicit opt-out from confinement"},
		{"ids:<id>[,<id>]", "self + exactly the named tasks, and nothing else"},
		{"subtree+ids:<id>", "self + descendants + the named tasks"},
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
		return enc.Encode(struct {
			Capabilities []CapInfo   `json:"capabilities"`
			Scopes       []ScopeInfo `json:"scopes"`
		}{cat, ScopesCatalog()})
	}
	// Each section gets its own tabwriter. One shared writer would align the
	// capability column to the width of the scope syntaxes and the trailing
	// prose, which is how the first version of this came out.
	capsTW := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(capsTW, "CAPABILITY\tBIT\tDESCRIPTION")
	for _, ci := range cat {
		fmt.Fprintf(capsTW, "%s\t0x%03x\t%s\n", ci.Name, ci.Bit, ci.Description)
	}
	if err := capsTW.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	scopeTW := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(scopeTW, "SCOPE\tDESCRIPTION")
	for _, si := range ScopesCatalog() {
		fmt.Fprintf(scopeTW, "%s\t%s\n", si.Syntax, si.Description)
	}
	if err := scopeTW.Flush(); err != nil {
		return err
	}

	_, err := fmt.Fprint(w, "\nA capability names a verb; a scope names which tasks it may target.\n"+
		"Both attenuate on spawn, and `caps set` re-grants either on a live task.\n")
	return err
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
//
// A term may be prefixed with "-" to subtract it: "all,-spawn" is every
// capability except spawn. Parsing is two-pass — every positive term is OR'd
// into the base, then every negative term is cleared — so the result does not
// depend on term order and "-spawn,all" means the same thing. A list of only
// negatives is rejected rather than assumed to start from "all": the implied
// base is not something the reader can see. Requiring a positive term also
// keeps the flag value from ever starting with "-".
func ParseCaps(s string) (protocol.Capability, error) {
	if strings.TrimSpace(s) == "" {
		return protocol.Capability_All, nil // omitted → inherit-all (server intersects with parent's caps)
	}
	grantable := GrantableCaps()
	byName := make(map[string]protocol.Capability, len(grantable))
	for _, c := range grantable {
		byName[c.String()] = c
	}
	var out, negated protocol.Capability
	sawPositive := false
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		name, negative := strings.CutPrefix(term, "-")
		name = strings.TrimSpace(name)
		c, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q (valid: %s)",
				name, strings.Join(CapNames(grantable), ", "))
		}
		if negative {
			negated |= c
			continue
		}
		sawPositive = true
		out |= c
	}
	if negated != 0 && !sawPositive {
		return 0, fmt.Errorf("caps %q: a subtractive list needs a base to subtract from (e.g. %q)",
			s, "all,"+s)
	}
	return out &^ negated, nil
}
