package server

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// Every grant-writing request reads its grant conditions from a stream it
// names, and is refused whole — nothing created, nothing rewritten — when that
// body cannot be read, does not decode, is over a limit, names a mask that is
// not a conditioned override, leaves a conditioned override without a body, or
// puts a kind on a bit it does not apply to (grant conditions C6).

const (
	gcPromptStream = 5
	gcCondStream   = 6
)

// gcConn is a fakeConn with client-created receive streams by id.
type gcConn struct {
	*fakeConn
	streams map[trsf.StreamID]trsf.ReceiveStream
}

func (c gcConn) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream { return c.streams[id] }

// errRecv fails its first read: a stream that cannot be read.
type errRecv struct {
	trsf.ReceiveStream
	cancelled bool
}

func (e *errRecv) ReadDirect(uint64) ([]byte, bool, error) { return nil, false, errors.New("reset") }
func (e *errRecv) Cancel()                                 { e.cancelled = true }

func gcPortBody(caps protocol.Capability, lo, hi uint16) *protocol.GrantConditions {
	c := &authority.Condition{Kind: protocol.ConditionKind_PortRange, Spans: []authority.PortSpan{{Lo: lo, Hi: hi}}}
	b := &protocol.GrantConditions{}
	b.SetConditions([]protocol.GrantCondition{c.ToWire(caps)})
	return b
}

// gcBody is one way of filling the request's conditions stream.
type gcBody struct {
	name string
	// mask is the override the request marks conditioned.
	mask protocol.Capability
	// stream is the conditions stream, nil = the request names none.
	stream func() trsf.ReceiveStream
	ok     bool
}

func recvOf(b []byte) func() trsf.ReceiveStream {
	return func() trsf.ReceiveStream { return &fakeRecv{data: b} }
}

func gcBodies() []gcBody {
	fwdL := protocol.Capability_ForwardLocal
	valid := gcPortBody(fwdL, 8000, 8100)
	var many []protocol.GrantCondition
	for i := 0; i < authority.MaxGrantConditions+1; i++ {
		many = append(many, valid.Conditions[0])
	}
	nine := &protocol.GrantConditions{}
	nine.SetConditions(many)
	return []gcBody{
		{name: "valid", mask: fwdL, stream: recvOf(valid.MustAppend(nil)), ok: true},
		{name: "unreadable stream", mask: fwdL, stream: func() trsf.ReceiveStream { return &errRecv{} }},
		{name: "undecodable body", mask: fwdL, stream: recvOf([]byte{0xff})},
		{name: "over the limit", mask: fwdL, stream: recvOf(nine.MustAppend(nil))},
		{name: "mask is not a conditioned override", mask: fwdL, stream: recvOf(gcPortBody(fwdL|protocol.Capability_ForwardRemote, 1, 2).MustAppend(nil))},
		{name: "conditioned override without a body", mask: fwdL},
		{name: "port_range on exec_run", mask: protocol.Capability_ExecRun, stream: recvOf(gcPortBody(protocol.Capability_ExecRun, 1, 2).MustAppend(nil))},
	}
}

// gcOverrides is the request's override list: mask marked conditioned.
func gcOverrides(mask protocol.Capability) []protocol.ScopeOverride {
	o := protocol.ScopeOverride{Caps: mask, Base: protocol.ScopeBase_Global}
	o.SetConditioned(true)
	return []protocol.ScopeOverride{o}
}

func (b gcBody) conn(fc *fakeConn, prompt string) gcConn {
	c := gcConn{fakeConn: fc, streams: map[trsf.StreamID]trsf.ReceiveStream{}}
	if prompt != "" {
		pb := protocol.PromptBody{}
		pb.SetPrompt([]byte(prompt))
		c.streams[gcPromptStream] = &fakeRecv{data: pb.MustAppend(nil)}
	}
	if b.stream != nil {
		c.streams[gcCondStream] = b.stream()
	}
	return c
}

func (b gcBody) streamID() uint64 {
	if b.stream == nil {
		return 0
	}
	return gcCondStream
}

func waitResponse(t *testing.T, fc *fakeConn) protocol.TaskControlResponse {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(fc.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return lastTaskControlResponse(t, fc)
}

func wantPorts(t *testing.T, what string, c *authority.Condition) {
	t.Helper()
	want := &authority.Condition{Kind: protocol.ConditionKind_PortRange, Spans: []authority.PortSpan{{Lo: 8000, Hi: 8100}}}
	if !c.Equal(want) {
		t.Fatalf("%s: stored condition %+v, want ports 8000-8100", what, c)
	}
}

func openWith(t *testing.T, h *TaskHandler, fc *fakeConn, b gcBody, oi *protocol.OpenInteractiveRequest, prompt string) protocol.OpenInteractiveResponse {
	t.Helper()
	oi.SetScopePresent(true)
	oi.SetOverrides(gcOverrides(b.mask))
	if prompt != "" {
		oi.SetPromptPresent(true)
		oi.SetPromptStreamId(gcPromptStream)
	}
	if b.stream != nil {
		var ext protocol.OpenInteractiveExt
		ext.SetGrantConditionsPresent(true)
		ext.SetGrantConditionsStreamId(gcCondStream)
		oi.SetExtPresent(true)
		oi.SetExt(ext)
	}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: 1}}
	req.SetOpenInteractive(*oi)
	h.Handle(b.conn(fc, prompt), encodeTaskControlRequest(t, req))
	resp := waitResponse(t, fc)
	r := resp.OpenInteractive()
	if r == nil {
		t.Fatalf("no open response (kind %v)", resp.Kind)
	}
	return *r
}

func gcOpConn(port int) *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + strconv.Itoa(port) + "-1"), nextStreamID: 7}
}

func TestGrantConditionsIntakeOpen(t *testing.T) {
	for _, mode := range []string{"fresh foreground", "fresh background", "resume"} {
		for _, b := range gcBodies() {
			t.Run(mode+"/"+b.name, func(t *testing.T) {
				h := newTestHandler(t)
				var oi *protocol.OpenInteractiveRequest
				var prompt, resumed string
				switch mode {
				case "fresh foreground":
					openRunner(t, h)
					oi = detachedOpen(0, 0)
				case "fresh background":
					oi, prompt = bgOpen("claude"), "p"
				case "resume":
					resumed = endedBG(t, h, protocol.TaskKind_Interactive)
					oi, prompt = resumeReq(resumed, bgOpen("")), "again"
				}
				oi.RequestedCaps = protocol.Capability_ForwardLocal | protocol.Capability_ExecRun
				fc := gcOpConn(9720)
				r := openWith(t, h, fc, b, oi, prompt)
				if b.ok {
					if r.Status != protocol.OpenInteractiveStatus_Ok {
						t.Fatalf("status %v (%s)", r.Status, r.ErrorMsg)
					}
					e, _ := h.Tasks.Get(hex.EncodeToString(r.TaskId.Id[:]))
					wantPorts(t, mode, e.Scope.CondFor(protocol.Capability_ForwardLocal))
					return
				}
				if r.Status != protocol.OpenInteractiveStatus_BadGrantConditions {
					t.Fatalf("status %v (%s), want bad_grant_conditions", r.Status, r.ErrorMsg)
				}
				switch mode {
				case "resume":
					if e, _ := h.Tasks.Get(resumed); e.Status != protocol.TaskStatus_Cancelled || len(e.Scope.Overrides) != 0 {
						t.Fatalf("a refused resume changed the task: status %v overrides %+v", e.Status, e.Scope.Overrides)
					}
				default:
					if n := len(h.Tasks.List(0)); n != 0 {
						t.Fatalf("a refused open left %d tasks", n)
					}
				}
			})
		}
	}
}

func credentialIssueWith(t *testing.T, h *TaskHandler, fc *fakeConn, b gcBody) protocol.CredentialResponse {
	t.Helper()
	_, pub := newCredentialKey(t)
	ci := protocol.CredentialIssueRequest{Pubkey: pub, TtlMs: 3600000, RequestedCaps: protocol.Capability_ForwardLocal | protocol.Capability_ExecRun,
		GrantConditionsStreamId: b.streamID()}
	ci.SetOverrides(gcOverrides(b.mask))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialIssue, RequestId: 1}}
	req.SetCredentialIssue(ci)
	h.Handle(b.conn(fc, ""), encodeTaskControlRequest(t, req))
	resp := waitResponse(t, fc)
	r := resp.CredentialIssue()
	if r == nil {
		t.Fatalf("no credential response (kind %v)", resp.Kind)
	}
	return *r
}

func TestGrantConditionsIntakeCredential(t *testing.T) {
	for _, b := range gcBodies() {
		t.Run("issue/"+b.name, func(t *testing.T) {
			h := newTestHandler(t)
			fc := gcOpConn(9721)
			r := credentialIssueWith(t, h, fc, b)
			if b.ok {
				if r.Status != protocol.CredentialStatus_Ok {
					t.Fatalf("status %v (%s)", r.Status, r.Message)
				}
				e, _ := h.Tasks.Get(hex.EncodeToString(r.TaskId.Id[:]))
				wantPorts(t, "issue", e.Scope.CondFor(protocol.Capability_ForwardLocal))
				return
			}
			if r.Status != protocol.CredentialStatus_BadGrantConditions {
				t.Fatalf("status %v (%s), want bad_grant_conditions", r.Status, r.Message)
			}
			if n := len(h.Tasks.List(0)); n != 0 {
				t.Fatalf("a refused issue left %d tasks", n)
			}
		})
		t.Run("rekey/"+b.name, func(t *testing.T) {
			h := newTestHandler(t)
			_, oldKey := newCredentialKey(t)
			id, err := h.Tasks.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli,
				authority.Grant{Caps: protocol.Capability_ForwardLocal | protocol.Capability_ExecRun}, oldKey, time.Now().Add(time.Hour), "")
			if err != nil {
				t.Fatal(err)
			}
			_, pub := newCredentialKey(t)
			cr := protocol.CredentialRekeyRequest{TaskId: hexToTaskID(t, id), Pubkey: pub, TtlMs: 3600000, GrantConditionsStreamId: b.streamID()}
			cr.SetScopePresent(true)
			cr.SetOverrides(gcOverrides(b.mask))
			req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialRekey, RequestId: 1}}
			req.SetCredentialRekey(cr)
			fc := gcOpConn(9722)
			h.Handle(b.conn(fc, ""), encodeTaskControlRequest(t, req))
			resp := waitResponse(t, fc)
			r := resp.CredentialRekey()
			if r == nil {
				t.Fatalf("no rekey response (kind %v)", resp.Kind)
			}
			e, _ := h.Tasks.Get(id)
			if b.ok {
				if r.Status != protocol.CredentialStatus_Ok {
					t.Fatalf("status %v (%s)", r.Status, r.Message)
				}
				wantPorts(t, "rekey", e.Scope.CondFor(protocol.Capability_ForwardLocal))
				return
			}
			if r.Status != protocol.CredentialStatus_BadGrantConditions {
				t.Fatalf("status %v (%s), want bad_grant_conditions", r.Status, r.Message)
			}
			if e.CredentialPubKey != oldKey || len(e.Scope.Overrides) != 0 {
				t.Fatal("a refused rekey changed the credential")
			}
		})
	}
}

func setCapsWith(t *testing.T, h *TaskHandler, conn ConnHandle, fc *fakeConn, target string, b gcBody) protocol.SetCapsResponse {
	t.Helper()
	sc := protocol.SetCapsRequest{TaskId: hexToTaskID(t, target), GrantConditionsStreamId: b.streamID()}
	sc.SetScopePresent(true)
	sc.SetOverrides(gcOverrides(b.mask))
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SetCaps, RequestId: 1}}
	req.SetSetCaps(sc)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := waitResponse(t, fc)
	r := resp.SetCaps()
	if r == nil {
		t.Fatalf("no set_caps response (kind %v)", resp.Kind)
	}
	return *r
}

func TestGrantConditionsIntakeSetCaps(t *testing.T) {
	for _, by := range []string{"operator", "task"} {
		for _, b := range gcBodies() {
			t.Run(by+"/"+b.name, func(t *testing.T) {
				h := newTestHandler(t)
				fc := gcOpConn(9723)
				all := authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{Base: protocol.ScopeBase_Global},
					DelegateCaps: protocol.Capability_All, DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}}
				parent := protocol.TaskID{}
				if by == "task" {
					p := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
						protocol.RunnerSelector{}, nil, all, "")
					parent = hexToTaskID(t, p)
					h.principals = map[objproto.ConnectionID]protocol.TaskID{fc.ConnectionID(): parent}
				}
				target := h.Tasks.Create("/r", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, parent, "",
					protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_ForwardLocal | protocol.Capability_ExecRun}, "")
				r := setCapsWith(t, h, b.conn(fc, ""), fc, target, b)
				e, _ := h.Tasks.Get(target)
				if b.ok {
					if r.Status != protocol.SetCapsStatus_Ok {
						t.Fatalf("status %v (%s)", r.Status, r.Message)
					}
					wantPorts(t, by, e.Scope.CondFor(protocol.Capability_ForwardLocal))
					return
				}
				if r.Status != protocol.SetCapsStatus_BadGrantConditions {
					t.Fatalf("status %v (%s), want bad_grant_conditions", r.Status, r.Message)
				}
				if len(e.Scope.Overrides) != 0 {
					t.Fatalf("a refused set_caps rewrote the scope: %+v", e.Scope.Overrides)
				}
			})
		}
	}
}

func scheduleSpecWith(b gcBody, sp *protocol.ScheduleSpec) *protocol.ScheduleSpec {
	sp.SetRequestedCapsPresent(true)
	sp.SetRequestedCaps(protocol.Capability_ForwardLocal | protocol.Capability_ExecRun)
	sp.SetScopePresent(true)
	sp.SetScope(protocol.TaskScope{})
	sp.SetOverrides(gcOverrides(b.mask))
	sp.GrantConditionsStreamId = b.streamID()
	return sp
}

func TestGrantConditionsIntakeSchedule(t *testing.T) {
	for _, op := range []string{"create", "edit"} {
		for _, b := range gcBodies() {
			t.Run(op+"/"+b.name, func(t *testing.T) {
				e := newSchedEnv(t)
				e.addCandidate("claude")
				fc := schedOpConn()
				var existing string
				kind := protocol.TaskControlKind_ScheduleCreate
				req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind, RequestId: 1}}
				prompt := ""
				if op == "create" {
					sp := scheduleSpecWith(b, createSpec())
					sp.SetPromptPresent(true)
					sp.SetPromptStreamId(gcPromptStream)
					prompt = "p"
					req.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *sp})
				} else {
					r := e.create(t, objproto.ConnectionID{}, createSpec(), "p")
					if r.Status != protocol.ScheduleStatus_Ok {
						t.Fatalf("setup create: %v", r.Status)
					}
					existing = schedIDHex(r.Id)
					kind = protocol.TaskControlKind_ScheduleEdit
					req.Header.Kind = kind
					req.SetScheduleEdit(protocol.ScheduleEditRequest{Id: r.Id, Spec: *scheduleSpecWith(b, &protocol.ScheduleSpec{})})
				}
				e.h.Handle(b.conn(fc, prompt), encodeTaskControlRequest(t, req))
				waitResponse(t, fc)
				r := scheduleResp(t, fc, kind)
				if b.ok {
					if r.Status != protocol.ScheduleStatus_Ok {
						t.Fatalf("status %v (%s)", r.Status, r.Message)
					}
					sc, _ := e.store.Get(schedIDHex(r.Id))
					wantPorts(t, op, sc.Grant.Scope.CondFor(protocol.Capability_ForwardLocal))
					return
				}
				if r.Status != protocol.ScheduleStatus_BadGrantConditions {
					t.Fatalf("status %v (%s), want bad_grant_conditions", r.Status, r.Message)
				}
				if op == "create" {
					if n := len(e.store.List()); n != 0 {
						t.Fatalf("a refused create left %d schedules", n)
					}
				} else if sc, _ := e.store.Get(existing); len(sc.Grant.Scope.Overrides) != 0 {
					t.Fatalf("a refused edit rewrote the schedule: %+v", sc.Grant.Scope.Overrides)
				}
			})
		}
	}
}

// A task caller whose forward_local is conditioned to 8000-8100 asking for a
// wider condition gets each request's existing containment refusal.
func TestGrantConditionWiderThanCallerRefused(t *testing.T) {
	wide := gcBody{name: "wide", mask: protocol.Capability_ForwardLocal, stream: recvOf(gcPortBody(protocol.Capability_ForwardLocal, 7000, 9000).MustAppend(nil))}
	callerGrant := authority.Grant{Caps: protocol.Capability_All, DelegateCaps: protocol.Capability_All,
		Scope: authority.Scope{Base: protocol.ScopeBase_Global, Overrides: []authority.ScopeOverride{{
			Caps: protocol.Capability_ForwardLocal, Base: protocol.ScopeBase_Global,
			Cond: &authority.Condition{Kind: protocol.ConditionKind_PortRange, Spans: []authority.PortSpan{{Lo: 8000, Hi: 8100}}},
		}}},
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global}}
	caller := func(t *testing.T, h *TaskHandler, fc *fakeConn) string {
		p := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
			protocol.RunnerSelector{}, nil, callerGrant, "")
		if h.principals == nil {
			h.principals = map[objproto.ConnectionID]protocol.TaskID{}
		}
		h.principals[fc.ConnectionID()] = hexToTaskID(t, p)
		return p
	}

	t.Run("open", func(t *testing.T) {
		h := newTestHandler(t)
		fc := gcOpConn(9730)
		caller(t, h, fc)
		oi := bgOpen("claude")
		oi.RequestedCaps = protocol.Capability_ForwardLocal
		if r := openWith(t, h, fc, wide, oi, "p"); r.Status != protocol.OpenInteractiveStatus_ScopeNotPermitted || !strings.Contains(string(r.ErrorMsg), "wider") {
			t.Fatalf("status %v (%s), want scope_not_permitted", r.Status, r.ErrorMsg)
		}
	})
	t.Run("credential issue", func(t *testing.T) {
		h := newTestHandler(t)
		fc := gcOpConn(9731)
		caller(t, h, fc)
		if r := credentialIssueWith(t, h, fc, wide); r.Status != protocol.CredentialStatus_ScopeNotPermitted || !strings.Contains(string(r.Message), "wider") {
			t.Fatalf("status %v (%s), want scope_not_permitted", r.Status, r.Message)
		}
	})
	t.Run("set_caps", func(t *testing.T) {
		h := newTestHandler(t)
		fc := gcOpConn(9732)
		p := caller(t, h, fc)
		target := h.Tasks.Create("/r", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, hexToTaskID(t, p), "",
			protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_ForwardLocal}, "")
		if r := setCapsWith(t, h, wide.conn(fc, ""), fc, target, wide); r.Status != protocol.SetCapsStatus_InvalidScope || !strings.Contains(string(r.Message), "wider") {
			t.Fatalf("status %v (%s), want invalid_scope (a task's grantFor refusal)", r.Status, r.Message)
		}
	})
	t.Run("schedule create", func(t *testing.T) {
		e := newSchedEnv(t)
		e.addCandidate("claude")
		fc := schedOpConn()
		caller(t, e.h, fc)
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleCreate, RequestId: 1}}
		sp := scheduleSpecWith(wide, createSpec())
		sp.SetRequestedCaps(protocol.Capability_ForwardLocal)
		sp.SetPromptPresent(true)
		sp.SetPromptStreamId(gcPromptStream)
		req.SetScheduleCreate(protocol.ScheduleCreateRequest{Spec: *sp})
		e.h.Handle(wide.conn(fc, "p"), encodeTaskControlRequest(t, req))
		waitResponse(t, fc)
		if r := scheduleResp(t, fc, protocol.TaskControlKind_ScheduleCreate); r.Status != protocol.ScheduleStatus_ScopeNotPermitted || !strings.Contains(string(r.Message), "wider") {
			t.Fatalf("status %v (%s), want scope_not_permitted", r.Status, r.Message)
		}
	})
}
