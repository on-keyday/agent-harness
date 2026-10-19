package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func auditTask(first byte, status protocol.TaskStatus, caps protocol.Capability, base protocol.ScopeBase) protocol.TaskInfo {
	t := protocol.TaskInfo{Status: status, Capabilities: caps}
	t.Id.Id[0] = first
	t.Scope.Base = base
	t.SetAgentProfile([]byte("claude"))
	t.SetPrompt([]byte("do the thing carefully and then report back"))
	return t
}

func rowFor(rows []AuditRow, c protocol.Capability) *AuditRow {
	for i := range rows {
		if rows[i].Cap == c {
			return &rows[i]
		}
	}
	return nil
}

func TestCapsAuditListsHoldersPerBit(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_Spawn|protocol.Capability_FileRead, protocol.ScopeBase_Subtree)
	b := auditTask(0xb2, protocol.TaskStatus_Detached, protocol.Capability_Spawn, protocol.ScopeBase_Global)
	c := auditTask(0xc3, protocol.TaskStatus_Running, protocol.Capability_FileRead, protocol.ScopeBase_None)
	rows := CapsAudit([]protocol.TaskInfo{a, b, c}, false, false)
	sp := rowFor(rows, protocol.Capability_Spawn)
	if sp == nil || len(sp.Holders) != 2 {
		t.Fatalf("spawn row = %+v, want two holders", sp)
	}
	order := verb.AuditOrder()
	pos := map[protocol.Capability]int{}
	for i, cp := range order {
		pos[cp] = i
	}
	for i := 1; i < len(rows); i++ {
		if pos[rows[i].Cap] < pos[rows[i-1].Cap] {
			t.Fatalf("rows out of AuditOrder: %s after %s", rows[i].Cap, rows[i-1].Cap)
		}
	}
}

func TestCapsAuditLiveOnlyByDefault(t *testing.T) {
	done := auditTask(0xd4, protocol.TaskStatus_Succeeded, protocol.Capability_Spawn, protocol.ScopeBase_Subtree)
	if r := rowFor(CapsAudit([]protocol.TaskInfo{done}, false, false), protocol.Capability_Spawn); r != nil {
		t.Fatalf("an ended holder is listed by default: %+v", r)
	}
	r := rowFor(CapsAudit([]protocol.TaskInfo{done}, true, false), protocol.Capability_Spawn)
	if r == nil || len(r.Holders) != 1 || r.Holders[0].Live {
		t.Fatalf("with includeEnded: %+v, want one non-live holder", r)
	}
}

func TestCapsAuditFoldsEmptyBits(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_Spawn, protocol.ScopeBase_Subtree)
	if r := rowFor(CapsAudit([]protocol.TaskInfo{a}, false, false), protocol.Capability_Purge); r != nil {
		t.Fatal("purge has no holder but is listed")
	}
	if r := rowFor(CapsAudit([]protocol.TaskInfo{a}, false, true), protocol.Capability_Purge); r == nil || len(r.Holders) != 0 {
		t.Fatalf("with includeEmpty purge = %+v, want an empty row", r)
	}
}

// Review Focus 1: the old `all` holds every bit but the two resume ones.
func TestCapsAuditOldAll(t *testing.T) {
	old := auditTask(0x70, protocol.TaskStatus_Running, protocol.Capability(0xfffff), protocol.ScopeBase_Global)
	rows := CapsAudit([]protocol.TaskInfo{old}, false, false)
	if len(rows) != 20 {
		t.Fatalf("old all appears in %d rows, want 20", len(rows))
	}
	if rowFor(rows, protocol.Capability_Resume) != nil || rowFor(rows, protocol.Capability_ResumeExact) != nil {
		t.Fatal("old all is listed under a resume bit it does not hold")
	}
}

func TestCapsAuditOverrideScope(t *testing.T) {
	x := protocol.TaskID{}
	x.Id[0] = 0xee
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_ExecControl|protocol.Capability_ExecCowrite, protocol.ScopeBase_Subtree)
	ov := protocol.ScopeOverride{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_None}
	ov.SetIds([]protocol.TaskID{x})
	a.Overrides = []protocol.ScopeOverride{ov}
	rows := CapsAudit([]protocol.TaskInfo{a}, false, false)
	ctl := rowFor(rows, protocol.Capability_ExecControl).Holders[0]
	if !ctl.Override || ctl.Breadth != BreadthNarrow || !strings.Contains(ctl.Scope, "ids:") {
		t.Fatalf("exec_control holder = %+v, want an override narrowed to ids", ctl)
	}
	cw := rowFor(rows, protocol.Capability_ExecCowrite).Holders[0]
	if cw.Override || cw.Breadth != BreadthSubtree || cw.Scope != "subtree" {
		t.Fatalf("exec_cowrite holder = %+v, want the base subtree", cw)
	}
}

// Review Focus 4: a bit no task scope bounds never shows a breadth.
func TestCapsAuditScopeNoneIgnoresOverride(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_BoardObserve, protocol.ScopeBase_None)
	a.Overrides = []protocol.ScopeOverride{{Caps: protocol.Capability_BoardObserve, Base: protocol.ScopeBase_None}}
	h := rowFor(CapsAudit([]protocol.TaskInfo{a}, false, false), protocol.Capability_BoardObserve).Holders[0]
	if h.ScopeKind != verb.ScopeNone || h.Scope != "" {
		t.Fatalf("board_observe holder = %+v, want ScopeNone and no scope", h)
	}
}

func TestScopeBreadthOfActionHalf(t *testing.T) {
	for in, want := range map[string]ScopeBreadth{
		"global": BreadthGlobal, "global/subtree": BreadthSubtree, "subtree-self": BreadthSubtree,
		"descendants": BreadthSubtree, "none/global": BreadthGlobal, "ids:a,b": BreadthNarrow, "none": BreadthNarrow,
		"subtree+ids:a": BreadthSubtree,
	} {
		if got := ScopeBreadthOf(in); got != want {
			t.Errorf("ScopeBreadthOf(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCapsAuditHolderOrder(t *testing.T) {
	n := auditTask(0x01, protocol.TaskStatus_Running, protocol.Capability_Cancel, protocol.ScopeBase_None)
	s := auditTask(0x02, protocol.TaskStatus_Running, protocol.Capability_Cancel, protocol.ScopeBase_Subtree)
	g := auditTask(0x03, protocol.TaskStatus_Running, protocol.Capability_Cancel, protocol.ScopeBase_Global)
	h := rowFor(CapsAudit([]protocol.TaskInfo{n, s, g}, false, false), protocol.Capability_Cancel).Holders
	if h[0].Breadth != BreadthGlobal || h[1].Breadth != BreadthSubtree || h[2].Breadth != BreadthNarrow {
		t.Fatalf("holder order = %v %v %v, want global, subtree, narrow", h[0].Breadth, h[1].Breadth, h[2].Breadth)
	}
}

func TestCapsAuditImpliedNote(t *testing.T) {
	ctl := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_ExecControl, protocol.ScopeBase_Subtree)
	view := auditTask(0xb2, protocol.TaskStatus_Running, protocol.Capability_ExecView, protocol.ScopeBase_Subtree)
	r := rowFor(CapsAudit([]protocol.TaskInfo{ctl, view}, false, false), protocol.Capability_ExecView)
	if len(r.Holders) != 1 {
		t.Fatalf("exec_view holders = %d, want only the literal holder", len(r.Holders))
	}
	found := false
	for _, c := range r.ImpliedBy {
		found = found || c == protocol.Capability_ExecControl
	}
	if !found {
		t.Fatalf("exec_view ImpliedBy = %v, want exec_control named", r.ImpliedBy)
	}
}

// Review Focus 2: the redacted parent row has no agent and no prompt.
func TestAuditLabelRedactedParent(t *testing.T) {
	p := protocol.TaskInfo{Status: protocol.TaskStatus_Running}
	p.Id.Id[0] = 0xab
	if got := AuditLabel(p); got != "ab000000 Running" {
		t.Fatalf("label = %q, want the id8 and the status only", got)
	}
}

// D19: a holder is recognisable by its repo and status, the prompt head is
// 16 runes, and an interactive session's empty prompt adds nothing.
func TestAuditLabelCarriesRepoAndStatus(t *testing.T) {
	d := auditTask(0xab, protocol.TaskStatus_Detached, 0, protocol.ScopeBase_Subtree)
	d.SetPrompt(nil)
	d.SetRepoPath([]byte("/home/u/remote-agent-harness"))
	if got := AuditLabel(d); got != "ab000000 claude remote-agent-harness Detached" {
		t.Fatalf("label = %q", got)
	}
}

func TestAuditLabelPromptHead16(t *testing.T) {
	full := auditTask(0xa1, protocol.TaskStatus_Running, 0, protocol.ScopeBase_Subtree)
	if got := AuditLabel(full); got != "a1000000 claude Running do the thing car" {
		t.Fatalf("label = %q", got)
	}
}

// Review Focus 4.
func TestAuditLabelRepoBasename(t *testing.T) {
	for _, path := range []string{`C:\workspace\x`, "C:/workspace/x/", "/a/x"} {
		task := protocol.TaskInfo{Status: protocol.TaskStatus_Running}
		task.SetRepoPath([]byte(path))
		if got := AuditLabel(task); got != "00000000 x Running" {
			t.Errorf("repo %q: label = %q, want basename x", path, got)
		}
	}
}

// Review Focus 3.
func TestCapsAuditEmpty(t *testing.T) {
	if rows := CapsAudit(nil, false, false); len(rows) != 0 {
		t.Fatalf("no tasks gave %d rows", len(rows))
	}
}
