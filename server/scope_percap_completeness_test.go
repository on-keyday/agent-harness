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
	protocol.Capability_Spawn:  {kind: capResolvedLiteral}, // resume_task_id names an existing task
	protocol.Capability_Prune:  {kind: capResolvedLiteral},

	// Chosen at runtime, so passed as a variable rather than by name.
	protocol.Capability_FileRead:      {kind: capResolvedVia, via: "lfNeed"},
	protocol.Capability_FileWrite:     {kind: capResolvedVia, via: "need"},
	protocol.Capability_ForwardLocal:  {kind: capResolvedVia, via: "need"},
	protocol.Capability_ForwardRemote: {kind: capResolvedVia, via: "need"},

	// The attach modes are three powers; the scope that binds one is the
	// MODE's own bit, not whichever stronger bit satisfied the check.
	protocol.Capability_ExecView:    {kind: capResolvedVia, via: "attachModeScopeCap"},
	protocol.Capability_ExecCowrite: {kind: capResolvedVia, via: "attachModeScopeCap"},
	protocol.Capability_ExecControl: {kind: capResolvedVia, via: "attachModeScopeCap"},

	protocol.Capability_ExecResize: {
		kind: capNoTargetResolution,
		reason: "resize rides an ATTACHED stream; the attach that opened it was " +
			"already target-gated, and there is no separate request naming a task",
	},
	protocol.Capability_Notify: {
		kind:   capNoTargetResolution,
		reason: "a notification names no task; its origin is stamped from the sender",
	},
	protocol.Capability_RunnerAdmin: {
		kind:   capNoTargetResolution,
		reason: "server dial-runner names a RUNNER, which the task scope does not contain",
	},
	protocol.Capability_BoardObserve: {
		kind: capNoTargetResolution,
		reason: "the agentboard is keyed by topic, which the task hierarchy does not " +
			"contain, so no task scope can bound it -- see the 2026-08-20 design §8a",
	},
	protocol.Capability_Purge: {
		kind:   capNoTargetResolution,
		reason: "purge names a board TOPIC, for the same reason as board_observe",
	},
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

// resolutionCallLines returns every source line that resolves a target set.
func resolutionCallLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, ".inScope(") || strings.Contains(line, ".scopeSet(") {
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
