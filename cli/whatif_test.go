package cli

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// O (operator root, global) → P → C → G, and X a second root. P and C hold
// cancel over their subtrees and may delegate it.
func whatIfListing() []protocol.TaskInfo {
	cancel, fileRead, spawn := protocol.Capability_Cancel, protocol.Capability_FileRead, protocol.Capability_Spawn
	return []protocol.TaskInfo{
		{Id: reachID(0x1f), Capabilities: protocol.Capability_All, Scope: protocol.TaskScope{Base: protocol.ScopeBase_Global}},
		{Id: reachID(0x3f), CreatorTaskId: reachID(0x1f), Capabilities: cancel | fileRead | spawn, DelegateCaps: cancel},
		{Id: reachID(0x8f), CreatorTaskId: reachID(0x3f), Capabilities: cancel | spawn, DelegateCaps: cancel},
		{Id: reachID(0x9f), CreatorTaskId: reachID(0x8f), Capabilities: cancel},
		{Id: reachID(0xaf), Capabilities: fileRead},
	}
}

func whatIfText(t *testing.T, c authority.Change) []string {
	t.Helper()
	r, err := BuildWhatIf(whatIfListing(), c)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteWhatIf(&b, r, false); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

func bitLine(name, body string) string { return fmt.Sprintf("      %-14s %s", name, body) }

func capsP(c protocol.Capability) *protocol.Capability { return &c }

func TestWhatIfReportRegrantCascade(t *testing.T) {
	p, c, g := reachHex(0x3f), reachHex(0x8f), reachHex(0x9f)
	got := whatIfText(t, authority.Regrant{Target: p, Caps: capsP(protocol.Capability_FileRead), Cascade: true})
	all := CapsLabel(protocol.Capability_Cancel | protocol.Capability_FileRead | protocol.Capability_Spawn)
	want := []string{
		"  3f3f3f3f  caps=" + all + " → file_read",
		bitLine("cancel", "-3: 3f3f3f3f 8f8f8f8f 9f9f9f9f"),
		"  8f8f8f8f  caps=" + CapsLabel(protocol.Capability_Cancel|protocol.Capability_Spawn) + " → none",
		bitLine("cancel", "-2: 8f8f8f8f 9f9f9f9f"),
		"  9f9f9f9f  caps=cancel → none",
		bitLine("cancel", "-1: 9f9f9f9f"),
		"  command: harness-cli caps set " + p + " --caps file_read --cascade",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	_, _ = c, g
}

func TestWhatIfReportSpawn(t *testing.T) {
	got := whatIfText(t, authority.Spawn{Creator: reachHex(0x8f), Caps: protocol.Capability_Cancel})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"  3f3f3f3f",
		bitLine("cancel", "+1: new"),
		"  new  caps=cancel  scope=subtree  delegate=none  delegate-scope=subtree",
		"  command (run by 8f8f8f8f): harness-cli submit --caps cancel --scope subtree",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestWhatIfReportSpawnRefused(t *testing.T) {
	scope := authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{reachHex(0xaf)}}
	_, err := BuildWhatIf(whatIfListing(), authority.Spawn{Creator: reachHex(0x3f), Scope: scope})
	if err == nil || !strings.Contains(err.Error(), "outside your own scope") {
		t.Errorf("err = %v, want the server's own-scope refusal", err)
	}
}

func TestWhatIfReportNothingChanges(t *testing.T) {
	if got := whatIfText(t, authority.Regrant{Target: reachHex(0x3f)}); !reflect.DeepEqual(got, []string{"  nothing changes"}) {
		t.Errorf("report = %q", got)
	}
}

func TestWhatIfReportPartial(t *testing.T) {
	r, err := BuildWhatIf(whatIfListing(), authority.Regrant{Target: reachHex(0x3f), Caps: capsP(protocol.Capability_FileRead)})
	if err != nil {
		t.Fatal(err)
	}
	r.Partial = true
	var b bytes.Buffer
	_ = WriteWhatIf(&b, r, false)
	if !strings.HasPrefix(b.String(), "(over your listing only)\n") {
		t.Errorf("a partial report does not say so first:\n%s", b.String())
	}
}

// The printed line is the grammar caps set parses back: the same target and
// the same five halves.
func TestWhatIfCommandRoundTrips(t *testing.T) {
	p, x := reachHex(0x3f), reachHex(0xaf)
	scope := authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{x},
		Overrides: []authority.ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}}}
	bound := authority.Scope{Base: protocol.ScopeBase_None}
	rg := authority.Regrant{Target: p, Caps: capsP(protocol.Capability_FileRead | protocol.Capability_Cancel), Scope: &scope,
		DelegateCaps: capsP(protocol.Capability_Cancel), DelegateScope: &bound, Cascade: true}
	tok := strings.Fields(WhatIfCommand(rg))
	if strings.Join(tok[:3], " ") != "harness-cli caps set" {
		t.Fatalf("command = %v", tok)
	}
	a, err := verb.ParseCmdCapsSet(verb.CLI, tok[3:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.TaskID != p || *a.Caps != *rg.Caps || *a.DelegateCaps != *rg.DelegateCaps || !a.Cascade {
		t.Errorf("parsed back %+v", a)
	}
	if got := authority.ScopeFromWire(*a.Scope, a.Overrides); got.String() != scope.String() {
		t.Errorf("scope = %s, want %s", got, scope)
	}
	if got := authority.ScopeFromWire(*a.DelegateScope, nil); got.String() != bound.String() {
		t.Errorf("bound = %s, want %s", got, bound)
	}

	tok = strings.Fields(WhatIfCommand(authority.Reparent{Target: p}))
	sp, err := verb.ParseCmdCapsSetParent(verb.CLI, tok[3:], nil)
	if err != nil || strings.Join(tok[:3], " ") != "harness-cli caps set-parent" || sp.TaskID != p || !sp.None {
		t.Errorf("reparent command %v → %+v, %v", tok, sp, err)
	}
}

func TestWhatIfVerbRules(t *testing.T) {
	p, c := reachHex(0x3f), reachHex(0x8f)
	for _, line := range []string{
		p,
		p + " --spawn --cascade",
		p + " --spawn --parent " + c,
		p + " --spawn --parent-none",
		p + " --parent " + c + " --caps cancel",
		p + " --parent-none --scope subtree",
		p + " --parent " + c + " --parent-none",
		p + " --parent-none --cascade",
		p + " --scope-for cancel:none",
	} {
		if _, err := verb.ParseCmdCapsWhatIf(verb.CLI, strings.Fields(line), nil); err == nil {
			t.Errorf("accepted: caps what-if %s", line)
		}
	}
	if _, err := verb.ParseCmdCapsWhatIf(verb.CLI, []string{p, "--parent", ""}, nil); err == nil {
		t.Error(`accepted: --parent ""`)
	}
	for _, line := range []string{
		p + " --caps file_read --cascade",
		p + " --spawn",
		p + " --spawn --caps cancel --scope-for cancel:none",
		p + " --parent " + c,
		p + " --parent-none",
	} {
		if _, err := verb.ParseCmdCapsWhatIf(verb.CLI, strings.Fields(line), nil); err != nil {
			t.Errorf("refused: caps what-if %s: %v", line, err)
		}
	}
}

func TestWhatIfChangeFromAction(t *testing.T) {
	p, c := reachHex(0x3f), reachHex(0x8f)
	a, err := verb.ParseCmdCapsWhatIf(verb.CLI, []string{p, "--spawn", "--caps", "cancel"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp, ok := WhatIfChange(a).(authority.Spawn); !ok || sp.Creator != p || sp.Caps != protocol.Capability_Cancel {
		t.Errorf("spawn change = %+v", WhatIfChange(a))
	}
	a, _ = verb.ParseCmdCapsWhatIf(verb.CLI, []string{p, "--parent", c}, nil)
	if rp, ok := WhatIfChange(a).(authority.Reparent); !ok || rp.Target != p || rp.NewParent != c {
		t.Errorf("reparent change = %+v", WhatIfChange(a))
	}
}

// The what-if's spawn denial reads as the real submit's does.
func TestWhatIfSpawnDenialIsTheClients(t *testing.T) {
	_, err := BuildWhatIf(whatIfListing(), authority.Spawn{Creator: reachHex(0x9f), Caps: protocol.Capability_Cancel})
	want := "caps what-if: " + (&CapabilityDeniedError{RequestedKind: protocol.TaskControlKind_Submit, RequiredCap: protocol.Capability_Spawn}).Error()
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}
