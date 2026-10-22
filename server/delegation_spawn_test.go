package server

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const delegHeld = protocol.Capability_Spawn | protocol.Capability_FileRead | protocol.Capability_Cancel |
	protocol.Capability_Resume | protocol.Capability_ResumeExact

// delegFixture is a handler with one runner on /r and an agent P (created by
// the operator, holding spawn,file_read,cancel and both resume bits over scope) carrying bound
// dcaps / dscope. It returns P's id, its connection id, and R — a terminal
// child of P to resume.
func delegFixture(t *testing.T, scope authority.Scope, dcaps protocol.Capability, dscope authority.Scope) (h *TaskHandler, p, cid, r string) {
	t.Helper()
	h = newTestHandler(t)
	openRunner(t, h)
	p = h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: delegHeld, Scope: scope, DelegateCaps: dcaps, DelegateScope: dscope}, "")
	r = h.Tasks.Create("/r", "r", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, hexToTaskID(t, p), "",
		protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	h.Tasks.Assign(r, testRunnerID("R"), "/wt/r", false)
	h.Tasks.Finish(r, 0, nil)
	cid = bindPrincipal(t, h, p)
	return h, p, cid, r
}

// spawnSite is one of the four places authority is granted. It sends a
// request carrying caps, scope and bound from (cid, creatorCaps), and returns
// the record of the task it wrote plus the refusal text, if any.
type spawnSite struct {
	name string
	do   func(t *testing.T, h *TaskHandler, cid, r string, creatorCaps protocol.Capability, a delegReq) (TaskEntry, string, bool)
}

type delegReq struct {
	caps   protocol.Capability
	scope  authority.Scope
	dcaps  protocol.Capability
	dscope authority.Scope
	// resume only
	capsPresent, scopePresent, dcapsPresent, dscopePresent bool
}

func entryOf(t *testing.T, h *TaskHandler, tid protocol.TaskID) TaskEntry {
	t.Helper()
	e, ok := h.Tasks.Get(hex.EncodeToString(tid.Id[:]))
	if !ok {
		t.Fatalf("task %x not in the store", tid.Id[:4])
	}
	return e
}

func submitReq(a delegReq) *protocol.SubmitRequest {
	req := &protocol.SubmitRequest{RequestedCaps: a.caps, DelegateCaps: a.dcaps}
	req.SetRepoPath([]byte("/r"))
	req.Scope, req.DelegateScope = a.scope.ToWire(), a.dscope.ToWire()
	req.SetOverrides(a.scope.OverridesToWire())
	req.SetResumeCapsOverride(a.capsPresent)
	req.SetScopePresent(a.scopePresent)
	req.SetDelegateCapsPresent(a.dcapsPresent)
	req.SetDelegateScopePresent(a.dscopePresent)
	return req
}

func openReq(a delegReq) *protocol.OpenInteractiveRequest {
	req := detachedOpen(0, 0)
	req.RequestedCaps, req.DelegateCaps = a.caps, a.dcaps
	req.Scope, req.DelegateScope = a.scope.ToWire(), a.dscope.ToWire()
	req.SetOverrides(a.scope.OverridesToWire())
	req.SetResumeCapsOverride(a.capsPresent)
	req.SetScopePresent(a.scopePresent)
	req.SetDelegateCapsPresent(a.dcapsPresent)
	req.SetDelegateScopePresent(a.dscopePresent)
	return req
}

var spawnSites = []spawnSite{
	{"submit", func(t *testing.T, h *TaskHandler, cid, _ string, cc protocol.Capability, a delegReq) (TaskEntry, string, bool) {
		resp, _ := h.handleSubmit(cid, submitReq(a), protocol.ClientKind_Agent, h.lookupPrincipal(cid), cc)
		if resp.Status != protocol.SubmitStatus_Ok {
			return TaskEntry{}, string(resp.ErrorMsg), false
		}
		return entryOf(t, h, resp.TaskId), "", true
	}},
	{"submit resume", func(t *testing.T, h *TaskHandler, cid, r string, cc protocol.Capability, a delegReq) (TaskEntry, string, bool) {
		req := submitReq(a)
		req.ResumeTaskId = hexToTaskID(t, r)
		resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), cc)
		if resp.Status != protocol.SubmitStatus_Ok {
			return TaskEntry{}, string(resp.ErrorMsg), false
		}
		return entryOf(t, h, resp.TaskId), "", true
	}},
	{"open", func(t *testing.T, h *TaskHandler, cid, _ string, cc protocol.Capability, a delegReq) (TaskEntry, string, bool) {
		resp, _ := h.handleOpenInteractive(cid, nil, openReq(a), protocol.ClientKind_Agent, h.lookupPrincipal(cid), cc)
		if resp.Status != protocol.OpenInteractiveStatus_Ok {
			return TaskEntry{}, string(resp.ErrorMsg), false
		}
		return entryOf(t, h, resp.TaskId), "", true
	}},
	{"open resume", func(t *testing.T, h *TaskHandler, cid, r string, cc protocol.Capability, a delegReq) (TaskEntry, string, bool) {
		req := openReq(a)
		req.ResumeTaskId = hexToTaskID(t, r)
		resp, _ := h.handleOpenInteractive(cid, nil, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), cc)
		if resp.Status != protocol.OpenInteractiveStatus_Ok {
			return TaskEntry{}, string(resp.ErrorMsg), false
		}
		return entryOf(t, h, resp.TaskId), "", true
	}},
}

func isResume(site string) bool { return strings.HasSuffix(site, "resume") }

func TestSpawnCapsThroughBound(t *testing.T) {
	fr, cn := protocol.Capability_FileRead, protocol.Capability_Cancel
	requests := []protocol.Capability{0, fr, fr | cn, protocol.Capability_All}
	for _, g := range []struct {
		name     string
		operator bool
		bound    protocol.Capability
		want     []protocol.Capability
	}{
		{"operator", true, 0, requests},
		{"bound none", false, 0, []protocol.Capability{0, 0, 0, 0}},
		{"bound file_read", false, fr, []protocol.Capability{0, fr, fr, fr}},
	} {
		for _, site := range spawnSites {
			for i, req := range requests {
				h, _, cid, r := delegFixture(t, authority.Scope{}, g.bound, authority.Scope{})
				cc := delegHeld
				if g.operator {
					cid, cc = "", protocol.Capability_All
				}
				e, msg, ok := site.do(t, h, cid, r, cc, delegReq{caps: req, capsPresent: true})
				if !ok {
					t.Fatalf("%s / %s / %s: refused: %s", g.name, site.name, CapsLabelT(req), msg)
				}
				if e.Capabilities != g.want[i] {
					t.Errorf("%s / %s / requested %s: child caps %s, want %s",
						g.name, site.name, CapsLabelT(req), CapsLabelT(e.Capabilities), CapsLabelT(g.want[i]))
				}
			}
		}
	}
}

func CapsLabelT(c protocol.Capability) string { return authority.CapsLabel(c) }

// Review Focus 1: the ceiling's refusal text reaches the requester on both
// spawn paths — through error_msg on the interactive one too, which is how
// agents spawn workers.
func TestSpawnScopeCeilingRefusalText(t *testing.T) {
	x := hex.EncodeToString(make([]byte, 15)) + "ab"
	for _, site := range spawnSites {
		h, _, cid, r := delegFixture(t, authority.Scope{IDs: []string{x}}, protocol.Capability_FileRead, authority.Scope{})
		_, msg, ok := site.do(t, h, cid, r, delegHeld, delegReq{
			caps: protocol.Capability_FileRead, scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{x}},
			capsPresent: true, scopePresent: true,
		})
		want := "scope id " + x + " is outside what you may delegate (see whoami: delegate-scope)"
		if ok || msg != want {
			t.Errorf("%s: ok=%v msg=%q, want refused with %q", site.name, ok, msg, want)
		}
	}
}

func TestSpawnScopeOutsideOwnScopeText(t *testing.T) {
	x := hex.EncodeToString(make([]byte, 15)) + "cd"
	for _, site := range spawnSites {
		h, _, cid, r := delegFixture(t, authority.Scope{}, protocol.Capability_FileRead, authority.Scope{Base: protocol.ScopeBase_Global})
		_, msg, ok := site.do(t, h, cid, r, delegHeld, delegReq{
			caps: protocol.Capability_FileRead, scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{x}},
			capsPresent: true, scopePresent: true,
		})
		if want := "scope id " + x + " is outside your own scope"; ok || msg != want {
			t.Errorf("%s: ok=%v msg=%q, want refused with %q", site.name, ok, msg, want)
		}
	}
}

func TestSpawnWritesChildBound(t *testing.T) {
	for _, site := range spawnSites {
		h, _, cid, r := delegFixture(t, authority.Scope{}, protocol.Capability_FileRead, authority.Scope{})
		e, msg, ok := site.do(t, h, cid, r, delegHeld, delegReq{
			dcaps: protocol.Capability_FileRead | protocol.Capability_Cancel, dscope: authority.Scope{Base: protocol.ScopeBase_None},
			dcapsPresent: true, dscopePresent: true,
		})
		if !ok {
			t.Fatalf("%s: refused: %s", site.name, msg)
		}
		if e.DelegateCaps != protocol.Capability_FileRead || !reflect.DeepEqual(e.DelegateScope, authority.Scope{Base: protocol.ScopeBase_None}) {
			t.Errorf("%s: child bound %s / %+v, want file_read / none", site.name, CapsLabelT(e.DelegateCaps), e.DelegateScope)
		}

		// Under a none ceiling the child is not among P's delegable action ids.
		h, _, cid, r = delegFixture(t, authority.Scope{}, protocol.Capability_FileRead, authority.Scope{Base: protocol.ScopeBase_None})
		e, msg, ok = site.do(t, h, cid, r, delegHeld, delegReq{dcapsPresent: true, dscopePresent: true})
		if !ok {
			t.Fatalf("%s under none: refused: %s", site.name, msg)
		}
		if !e.DelegateScope.ExcludeSelf {
			t.Errorf("%s under a none ceiling: child bound %+v, want exclude_self forced on", site.name, e.DelegateScope)
		}
	}
}

func TestResumePresenceBitsWriteOneHalf(t *testing.T) {
	recorded := authority.Grant{DelegateCaps: protocol.Capability_FileRead, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}}
	ask := delegReq{dcaps: protocol.Capability_Cancel, dscope: authority.Scope{Base: protocol.ScopeBase_None}}
	for _, site := range spawnSites {
		if !isResume(site.name) {
			continue
		}
		for _, tc := range []struct {
			name         string
			dcaps, dscop bool
			wantCaps     protocol.Capability
			wantScope    authority.Scope
		}{
			{"delegate caps only", true, false, protocol.Capability_Cancel, recorded.DelegateScope},
			{"delegate scope only", false, true, recorded.DelegateCaps, authority.Scope{Base: protocol.ScopeBase_None}},
			{"neither", false, false, recorded.DelegateCaps, recorded.DelegateScope},
		} {
			h, _, cid, r := delegFixture(t, authority.Scope{}, protocol.Capability_Cancel, authority.Scope{Base: protocol.ScopeBase_Global})
			h.Tasks.SetCaps(r, GrantUpdate{DelegateCaps: true, DelegateScope: true, To: recorded}, AuthorityWrite{})
			a := ask
			a.dcapsPresent, a.dscopePresent = tc.dcaps, tc.dscop
			e, msg, ok := site.do(t, h, cid, r, delegHeld, a)
			if !ok {
				t.Fatalf("%s / %s: refused: %s", site.name, tc.name, msg)
			}
			if e.DelegateCaps != tc.wantCaps || !reflect.DeepEqual(e.DelegateScope, tc.wantScope) {
				t.Errorf("%s / %s: bound %s / %+v, want %s / %+v", site.name, tc.name,
					CapsLabelT(e.DelegateCaps), e.DelegateScope, CapsLabelT(tc.wantCaps), tc.wantScope)
			}
		}
	}
}

func TestExactResumeRefusesBoundHalves(t *testing.T) {
	for _, tc := range []struct {
		flag         string
		dcaps, dscop bool
	}{{"--delegate-caps", true, false}, {"--delegate-scope", false, true}} {
		h, p, cid, r := delegFixture(t, authority.Scope{}, 0, authority.Scope{})
		req := submitReq(delegReq{dcapsPresent: tc.dcaps, dscopePresent: tc.dscop})
		req.ResumeTaskId = hexToTaskID(t, r)
		req.SetRepoPath(nil)
		req.SetResumeExact(true)
		resp, _ := h.handleSubmit(cid, req, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
		if resp.Status != protocol.SubmitStatus_ResumeExactMismatch || !strings.Contains(string(resp.ErrorMsg), tc.flag) {
			t.Errorf("submit %s: %v %q, want resume_exact_mismatch naming %s", tc.flag, resp.Status, resp.ErrorMsg, tc.flag)
		}

		// An interactive record, so the kind check passes and only the bound
		// bit can be the mismatch.
		ri := h.Tasks.Create("/r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, hexToTaskID(t, p), "",
			protocol.RunnerSelector{}, nil, authority.Grant{}, "")
		h.Tasks.Finish(ri, 0, nil)
		oreq := openReq(delegReq{dcapsPresent: tc.dcaps, dscopePresent: tc.dscop})
		oreq.ResumeTaskId = hexToTaskID(t, ri)
		oreq.SetResumeExact(true)
		oresp, _ := h.handleOpenInteractive(cid, nil, oreq, protocol.ClientKind_Agent, h.lookupPrincipal(cid), delegHeld)
		if oresp.Status != protocol.OpenInteractiveStatus_ResumeExactMismatch || !strings.Contains(string(oresp.ErrorMsg), tc.flag) {
			t.Errorf("open %s: %v %q, want resume_exact_mismatch naming %s", tc.flag, oresp.Status, oresp.ErrorMsg, tc.flag)
		}
	}
}

// The reviewer's probe: a resume that re-grants only delegate_caps, carrying
// an UNWRITTEN delegate_scope its granter would refuse, writes the caps it
// asked for and reports ok — the unwritten half's refusal is not its concern.
func TestResumeCapsHalfIgnoresUnwrittenScopeRefusal(t *testing.T) {
	x := hex.EncodeToString(make([]byte, 15)) + "ef"
	for _, site := range spawnSites {
		if !isResume(site.name) {
			continue
		}
		h, _, cid, r := delegFixture(t, authority.Scope{IDs: []string{x}}, protocol.Capability_Cancel, authority.Scope{})
		e, msg, ok := site.do(t, h, cid, r, delegHeld, delegReq{
			dcaps: protocol.Capability_Cancel, dscope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{x}},
			dcapsPresent: true,
		})
		if !ok || e.DelegateCaps != protocol.Capability_Cancel {
			t.Errorf("%s: ok=%v msg=%q delegate_caps=%s, want ok and cancel", site.name, ok, msg, CapsLabelT(e.DelegateCaps))
		}
	}
}
