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
		// Listed weakest-first so a reader scanning the catalog meets the
		// read-only power before the one that evicts whoever is driving.
		protocol.Capability_ExecView,
		protocol.Capability_ExecCowrite,
		protocol.Capability_ExecControl,
		protocol.Capability_ExecResize,
		// exec_run is separate from the three attach ranks, not a fourth one:
		// it runs a command as its OWN process in the task's worktree instead
		// of driving the session's PTY, so it neither implies nor is implied by
		// any of them.
		protocol.Capability_ExecRun,
		protocol.Capability_FileRead,
		protocol.Capability_FileWrite,
		protocol.Capability_ForwardLocal,
		protocol.Capability_ForwardRemote,
		protocol.Capability_Notify,
		protocol.Capability_Prune,
		protocol.Capability_RunnerAdmin,
		protocol.Capability_BoardObserve,
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
	case protocol.Capability_ExecView:
		return "observe an agent's output, live or recorded: a PTY session " +
			"(snapshot, grid panes, attach --view), an event-stream session, and task logs"
	case protocol.Capability_ExecCowrite:
		return "type into a session someone else is driving, without evicting them (session send / exec); implies exec_view"
	case protocol.Capability_ExecControl:
		return "take a session's PTY over as sole writer, evicting the current one (session attach); implies exec_cowrite"
	case protocol.Capability_ExecResize:
		return "resize a session's PTY as a viewer or cowriter, while no control client is attached (orthogonal to the three above; control owns the size whenever it holds the seat)"
	case protocol.Capability_ExecRun:
		return "run a command in a task's worktree as its own process (exec), separate from the session's PTY; also stops one (exec kill)"
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
	case protocol.Capability_BoardObserve:
		return "list board topics, read a topic's retained messages, and list its subscribers; " +
			"NOT required to send, subscribe, or read your own inbox"
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
		{"descendants", "the subtree WITHOUT self; e.g. may drive its workers but not itself"},
		{"<base>-self", "any base with self removed; none-self is the empty set (holds the bit, points nowhere)"},
		{"ids:<id>[,<id>]", "self + exactly the named tasks, and nothing else"},
		{"subtree+ids:<id>", "self + descendants + the named tasks"},
		{"<visibility>/<action>", "visibility rank / action rank; e.g. global/subtree sees the server, acts on its subtree"},
		{"+vis-ids:<id>", "additionally SEE those tasks without being able to act on them"},
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
		"--scope-for CAPS=SCOPE narrows ONE capability (or a comma-separated list)\n"+
		"below the task's own scope; the lists must not overlap. Visibility is a\n"+
		"property of the task, so it has no per-capability form: a verb can never\n"+
		"reach further than what `ls` shows.\n"+
		"An omitted --caps grants NONE of these: a spawn hands its child no\n"+
		"control plane unless the flag names one (the data plane - agentboard,\n"+
		"its own subtree's logs/ls - needs no capability and is always there).\n"+
		"Both attenuate on spawn, and `caps set` re-grants either on a live task.\n"+
		"subtree membership follows the task's parent link (who spawned whom);\n"+
		"`caps set-parent` re-points that link on a live task (--swap inverts\n"+
		"the task with its current parent) without touching caps or scope.\n")
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

// CapsFlagUsage is the --caps help text shared by submit / interactive /
// session new (cmd/harness-cli). It lived as three identical literals, which is
// exactly the shape that ends up describing two different defaults after one
// edit. The TUI keeps its own text because its flag overrides a session
// default rather than the parser default (tui/cmdline.go capsFlagUsage).
const CapsFlagUsage = "comma-separated capability names to grant the task " +
	"(e.g. spawn,file_read / all / none); a name may be subtracted with a " +
	"leading dash, as in all,-spawn; default: none — a spawn grants nothing " +
	"unless this flag names it. With --resume, --caps re-grants caps to the " +
	"task (else its persisted caps are kept)"

// ParseCaps converts a comma-separated list of capability names into a bitmask.
// Empty/whitespace → Capability_None (default-deny); unknown name → error.
//
// The empty case is the DEFAULT of every --caps flag, so omitting the flag
// grants nothing: authority has to be typed out. It used to mean
// Capability_All ("inherit whatever the spawner holds"), which made every
// unadorned `submit` hand its child the full control plane. Nothing is lost
// by the flip that `caps set` cannot restore on a live task.
//
// Names are case-sensitive and match the snake_case string representation
// produced by Capability.String() (e.g. "spawn", "file_read", "exec_view").
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
		return protocol.Capability_None, nil // omitted → grant nothing (default-deny)
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
