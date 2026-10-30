package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Per-KIND coverage lives in scope_completeness_test.go: it catches a new
// TaskControlKind that carries a task id and skips the gate. This file is the
// other axis, and it opened when scope stopped being one value per task.
//
// A capability whose target is never resolved for it resolves through the BASE
// scope instead, silently — the override an operator wrote for that bit is
// simply not consulted, and nothing anywhere says so. That is invisible in
// exactly the way the per-kind gap was: the code compiles, the tests pass, and
// the authority is quietly wider than the grant.
//
// So every grantable bit is classified here, and the classification is checked
// against the source rather than trusted.
type capTargetKind int

const (
	// capResolvedLiteral: some dispatch site passes this capability to
	// inScope / scopeSet / authorize by name.
	capResolvedLiteral capTargetKind = iota
	// capResolvedVia: the site passes a variable or helper call instead,
	// because the bit is chosen at runtime (direction, mode, which of two
	// bits the caller holds). The expression is named so the check has
	// something to look for.
	capResolvedVia
	// capNoTargetResolution: the bit gates a KIND rather than a target task,
	// so there is nothing to resolve. Requires a reason.
	capNoTargetResolution
)

type capTargetClass struct {
	kind   capTargetKind
	via    string // capResolvedVia: the expression that carries it
	reason string // capNoTargetResolution: why the bit names no task
}

var capTargetClasses = map[protocol.Capability]capTargetClass{
	protocol.Capability_Cancel: {kind: capResolvedLiteral},
	protocol.Capability_Spawn: {
		kind:   capNoTargetResolution,
		reason: "creating a task names no existing one; resuming is the resume bits'",
	},
	// A resume target is allowed through ANY held resume bit whose own scope
	// covers it, so the bits are passed from a loop over resumeRequestBits, as
	// resumeBit, rather than by name. By
	// name would be unsound anyway: the scan matches substrings, and
	// Capability_Resume is a prefix of Capability_ResumeExact.
	protocol.Capability_Resume:      {kind: capResolvedVia, via: "resumeBit"},
	protocol.Capability_ResumeExact: {kind: capResolvedVia, via: "resumeBit"},
	protocol.Capability_Prune:       {kind: capResolvedLiteral},

	// Chosen at runtime, so passed as a variable rather than by name.
	protocol.Capability_FileRead:      {kind: capResolvedVia, via: "lfNeed"},
	protocol.Capability_FileWrite:     {kind: capResolvedVia, via: "need"},
	protocol.Capability_ForwardLocal:  {kind: capResolvedVia, via: "need"},
	protocol.Capability_ForwardRemote: {kind: capResolvedVia, via: "need"},
	protocol.Capability_ForwardLink:   {kind: capResolvedVia, via: "need"},

	// forward_tap names a FORWARD, which resolves to the task that owns it, and
	// the handler passes the bit to authorize by name. Not capNoTargetResolution:
	// a forward VISIBLE through the visibility rank may still belong to a task outside
	// the caller's action scope, which is the same distinction kill_port_forward
	// draws one line above.
	protocol.Capability_ForwardTap: {kind: capResolvedLiteral},
	// exec_tap names an EXEC, which resolves to the task that owns it; the
	// handler passes the bit to inScope by name.
	protocol.Capability_ExecTap: {kind: capResolvedLiteral},

	// The attach modes are three powers; the scope that binds one is the
	// MODE's own bit, not whichever stronger bit satisfied the check.
	protocol.Capability_ExecView:    {kind: capResolvedVia, via: "authority.AttachModeScopeCap"},
	protocol.Capability_ExecCowrite: {kind: capResolvedVia, via: "authority.AttachModeScopeCap"},
	protocol.Capability_ExecControl: {kind: capResolvedVia, via: "authority.AttachModeScopeCap"},

	// exec_run names a task in both of its requests: open_exec_run carries a
	// task id, and exec_run_kill carries an exec id that resolves to one. Both
	// pass the bit by name.
	protocol.Capability_ExecRun: {kind: capResolvedLiteral},

	// exec_resize rides an attach, but its target is checked against its OWN
	// scope when the attach decides whether the observer's resizes are honoured
	// (resizeAllowed), so --scope-for exec_resize narrows it.
	protocol.Capability_ExecResize: {kind: capResolvedLiteral},
	protocol.Capability_Notify: {
		kind:   capNoTargetResolution,
		reason: "a notification names no task; its origin is stamped from the sender",
	},
	protocol.Capability_RunnerAdmin: {
		kind:   capNoTargetResolution,
		reason: "server dial-runner names a RUNNER, which the task scope does not contain",
	},
	protocol.Capability_RunnerEnroll: {
		kind:   capNoTargetResolution,
		reason: "runner enroll / keys / unenroll name a KEY and the runners on it, which the task scope does not contain",
	},
	protocol.Capability_BoardPersist: {
		kind:   capNoTargetResolution,
		reason: "board retain names a TOPIC, which the task hierarchy does not contain; a task caller is bounded by the board reach check instead",
	},
	protocol.Capability_BoardObserve: {
		kind: capNoTargetResolution,
		reason: "the agentboard is keyed by topic, which the task hierarchy does not " +
			"contain, so no task scope can bound it -- see the 2026-08-20 design §8a",
	},
	// purge's topic verbs (board purge / retract and their thread forms) name a
	// TOPIC, which no task scope bounds; board clear-contacts names a TASK and
	// passes the bit to inScope by name. The literal is that call.
	protocol.Capability_BoardPurge: {kind: capResolvedLiteral},
	protocol.Capability_BoardSend: {
		kind:   capNoTargetResolution,
		reason: "board_send names a board TOPIC, for the same reason as board_observe",
	},
	// board_peer opens the chat.<id> of tasks in its target set (reach spec
	// D16); the board's reach source resolves it through authority.ActionSet by name.
	protocol.Capability_BoardPeer: {kind: capResolvedLiteral},
}

// serverSources concatenates the package's non-test sources, which is what the
// classification is checked against.
func serverSources(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sb.Write(b)
	}
	return sb.String()
}

// resolutionCallLines returns every source line that resolves a target set:
// the handler's inScope / scopeSet, and authority.ActionSet, which the board's reach
// source calls from a task rather than a connection (reach spec D16).
func resolutionCallLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, ".inScope(") || strings.Contains(line, ".scopeSet(") ||
			strings.Contains(line, "authority.ActionSet(") {
			out = append(out, line)
		}
	}
	return out
}

func TestEveryCapabilityDeclaresHowItsTargetIsResolved(t *testing.T) {
	for _, bit := range GrantableCapsForTest() {
		if _, ok := capTargetClasses[bit]; !ok {
			t.Errorf("%s is not classified in capTargetClasses — say how its target is "+
				"resolved, or why it names no task. An unclassified bit resolves through "+
				"the base scope and ignores its own override, silently", bit)
		}
	}
}

func TestCapabilityTargetClassesMatchTheSource(t *testing.T) {
	lines := resolutionCallLines(serverSources(t))
	if len(lines) == 0 {
		t.Fatal("found no inScope/scopeSet call sites — the scan is broken, not the code")
	}
	appears := func(needle string) bool {
		for _, l := range lines {
			if strings.Contains(l, needle) {
				return true
			}
		}
		return false
	}

	for bit, class := range capTargetClasses {
		switch class.kind {
		case capResolvedLiteral:
			if !appears("protocol."+bit.String()) && !appears(capConstName(bit)) {
				t.Errorf("%s is classified as resolved by name, but no inScope/scopeSet "+
					"call passes it", bit)
			}
		case capResolvedVia:
			if class.via == "" {
				t.Errorf("%s: capResolvedVia with no expression named", bit)
				continue
			}
			if !appears(class.via) {
				t.Errorf("%s is classified as resolved via %q, but no inScope/scopeSet "+
					"call mentions it — the indirection moved or went away",
					bit, class.via)
			}
		case capNoTargetResolution:
			if class.reason == "" {
				t.Errorf("%s: exempted without a reason — every exemption must be justified", bit)
			}
			// The exemption goes stale the moment some verb under the bit starts
			// naming a task: purge sat here as "names a TOPIC" after board
			// clear-contacts began passing it to inScope.
			if appears("protocol."+bit.String()) || appears(capConstName(bit)) {
				t.Errorf("%s is classified as naming no task, but a target resolution "+
					"passes it — reclassify it", bit)
			}
		}
	}
}

// GrantableCapsForTest enumerates the single-bit capabilities, derived from
// Capability_All rather than a second hand-written list -- a copy would be one
// more thing to forget when a bit is added.
func GrantableCapsForTest() []protocol.Capability {
	var out []protocol.Capability
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if protocol.Capability_All&bit != 0 {
			out = append(out, bit)
		}
	}
	return out
}

// capConstName is the Go identifier for a bit, for source scanning: the wire
// name is snake_case ("exec_view") while the constant is CamelCase.
func capConstName(c protocol.Capability) string {
	parts := strings.Split(c.String(), "_")
	var sb strings.Builder
	sb.WriteString("Capability_")
	for _, p := range parts {
		if p == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return sb.String()
}
