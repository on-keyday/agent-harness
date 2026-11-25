package verb

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

const gcActionHex = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"

func TestParseGrantCondition(t *testing.T) {
	p, err := ParseGrantCondition("forward_local,forward_remote=ports:8000-8100,22")
	if err != nil {
		t.Fatal(err)
	}
	if p.Caps != protocol.Capability_ForwardLocal|protocol.Capability_ForwardRemote || p.Kind != protocol.ConditionKind_PortRange ||
		p.PortRange() == nil || len(p.PortRange().Spans) != 2 {
		t.Fatalf("ports = %+v", p)
	}
	// ':' in place of '=', as ConditionsLabel prints it.
	if q, err := ParseGrantCondition("forward_local:ports:8000"); err != nil || q.PortRange().Spans[0] != (protocol.PortSpan{Lo: 8000, Hi: 8000}) {
		t.Fatalf("label spelling: %+v %v", q, err)
	}
	a, err := ParseGrantCondition("exec_run=actions:" + gcActionHex + "," + strings.ToUpper(gcActionHex))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != protocol.ConditionKind_ActionSet || a.ActionSet() == nil || len(a.ActionSet().Actions) != 1 {
		t.Fatalf("actions (one, deduped) = %+v", a)
	}
	for _, bad := range []string{
		"forward_local=ports:65536",
		"forward_local=ports:0",
		"forward_local=ports:9000-8000",
		"forward_local=ports:",
		"forward_local=hosts:x",
		"exec_run=actions:abc",
		"exec_run=ports:22",                         // a kind on a bit it does not apply to
		"forward_local,cancel=ports:22",             // part of the mask is not a forward bit
		"forward_link=ports:22",                     // ip forwards are not a PortRange bit
		"forward_local",                             // no '='
		"=ports:22",                                 // no caps
		"forward_local=ports:1,3,5,7,9,11,13,15,17", // over 8 spans (not adjacent, so they do not merge)
	} {
		if _, err := ParseGrantCondition(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGrantConditionNeedsMatchingScopeFor(t *testing.T) {
	cond, err := ParseGrantCondition("forward_local=ports:8000-8100")
	if err != nil {
		t.Fatal(err)
	}
	_, exact, _ := ParseScopeFor("forward_local=global")
	marked, err := MarkGrantConditions([]protocol.ScopeOverride{exact}, []protocol.GrantCondition{cond})
	if err != nil || !marked[0].Conditioned() {
		t.Fatalf("exact mask: %v %+v", err, marked)
	}
	if exact.Conditioned() {
		t.Fatal("MarkGrantConditions marked its input instead of a copy")
	}
	_, wider, _ := ParseScopeFor("forward_local,forward_remote=global")
	for name, ovs := range map[string][]protocol.ScopeOverride{"none": nil, "a wider mask": {wider}} {
		_, err := MarkGrantConditions(ovs, []protocol.GrantCondition{cond})
		if err == nil || !strings.Contains(err.Error(), "--scope-for forward_local=") {
			t.Errorf("%s: err = %v, want one naming --scope-for forward_local=", name, err)
		}
	}
	if _, err := parseGrantConditionList([]string{"forward_local=ports:1", "forward_local=ports:2"}); err == nil {
		t.Error("two conditions for one mask accepted")
	}
}

func TestConditionsLabel(t *testing.T) {
	cond, _ := ParseGrantCondition("forward_local=ports:8000-8100")
	_, ov, _ := ParseScopeFor("forward_local=ids:" + strings.Repeat("ab", 16))
	_, other, _ := ParseScopeFor("cancel=none")
	marked, _ := MarkGrantConditions([]protocol.ScopeOverride{ov, other}, []protocol.GrantCondition{cond})
	if got := ConditionsLabel(marked, []protocol.GrantCondition{cond}); got != "forward_local:ports:8000-8100" {
		t.Errorf("label = %q", got)
	}
	// The marker without its body (whoami's inline form, a client that did not
	// fetch it): shown as unknown, never as unconditioned.
	if got := ConditionsLabel(marked, nil); got != "forward_local:(condition unknown)" {
		t.Errorf("label without body = %q", got)
	}
	// What ConditionsLabel prints parses back.
	if _, err := ParseGrantCondition(ConditionsLabel(marked, []protocol.GrantCondition{cond})); err != nil {
		t.Errorf("label does not parse back: %v", err)
	}
	by := ConditionsByCap(protocol.Capability_ForwardLocal|protocol.Capability_ForwardRemote|protocol.Capability_Cancel, marked, []protocol.GrantCondition{cond})
	if len(by) != 1 || by["forward_local"] != "ports:8000-8100" {
		t.Errorf("by cap = %v, want forward_local only (forward_remote is unconditioned)", by)
	}
}
