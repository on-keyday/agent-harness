package server

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// The record verbs through Handle, as a task caller (pillar A spec, A-D22,
// A-D26, A-D27).

type recFixture struct {
	h       *TaskHandler
	caller  string // the calling task
	target  string // another task in the caller's scope
	conn    *fakeConn
	targetM *SessionMux
}

func newRecFixture(t *testing.T) *recFixture {
	t.Helper()
	h := newTestHandler(t)
	h.Records, _ = NewRecordStore(RecordConfig{Dir: filepath.Join(t.TempDir(), "records")})
	f := &recFixture{h: h, caller: runNumberTask(t, h.Tasks), target: runNumberTask(t, h.Tasks)}
	for _, id := range []string{f.caller, f.target} {
		w, _ := h.Records.OpenRun(id, 1, time.Unix(0, 1))
		_ = w.Append(recEntry(1, "one"))
		_ = w.Append(recEntry(2, "two"))
		_ = w.Detach()
	}
	// A parseable connection id: bindPrincipal's suffix is hex.
	cid := objproto.MustParseConnectionID("ws:127.0.0.1:9171-1")
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[cid.String()] = hexToTaskID(t, f.caller)
	f.conn = &fakeConn{id: cid, autoSendStreams: true}
	f.grant(t, protocol.Capability_ExecView, authority.Scope{Base: protocol.ScopeBase_Global})
	return f
}

func (f *recFixture) grant(t *testing.T, caps protocol.Capability, sc authority.Scope) {
	t.Helper()
	if _, ok := f.h.Tasks.SetCaps(f.caller, GrantUpdate{Caps: true, Scope: true, To: authority.Grant{Caps: caps, Scope: sc}}, AuthorityWrite{}); !ok {
		t.Fatal("SetCaps failed")
	}
}

// liveMux gives the target a live interactive session.
func (f *recFixture) liveMux(t *testing.T) *SessionMux {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewSessionMux(ctx, f.target, protocol.TaskKind_Interactive, newFakeStream(t), NewRingBuffer(4096), SessionHooks{})
	t.Cleanup(m.Stop)
	f.h.Sessions.Add(f.target, m)
	f.h.Tasks.Assign(f.target, protocol.RunnerID{}, "/wt", false)
	return m
}

func (f *recFixture) call(t *testing.T, kind protocol.TaskControlKind, set func(*protocol.TaskControlRequest)) *protocol.TaskControlResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: kind, RequestId: 9}}
	set(req)
	f.h.Handle(f.conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, f.conn)
	return &resp
}

func refOf(t *testing.T, id string, run uint32) protocol.RecordRef {
	return protocol.RecordRef{TaskId: hexToTaskID(t, id), Run: run}
}

func denied(r *protocol.TaskControlResponse) bool {
	return r.Kind == protocol.TaskControlKind_PermissionDenied
}

func (f *recFixture) get(t *testing.T, id string) *protocol.TaskControlResponse {
	return f.call(t, protocol.TaskControlKind_RecordGet, func(r *protocol.TaskControlRequest) {
		r.SetRecordGet(protocol.RecordGetRequest{Ref: refOf(t, id, 0)})
	})
}

func (f *recFixture) del(t *testing.T, id string) *protocol.TaskControlResponse {
	return f.call(t, protocol.TaskControlKind_RecordDelete, func(r *protocol.TaskControlRequest) {
		r.SetRecordDelete(protocol.RecordDeleteRequest{Ref: refOf(t, id, 1)})
	})
}

func (f *recFixture) keep(t *testing.T, id string, forever bool, ttl time.Duration) *protocol.TaskControlResponse {
	return f.call(t, protocol.TaskControlKind_RecordKeep, func(r *protocol.TaskControlRequest) {
		k := protocol.RecordKeepRequest{Ref: refOf(t, id, 1), TtlMs: uint64(ttl.Milliseconds())}
		k.SetForever(forever)
		r.SetRecordKeep(k)
	})
}

func (f *recFixture) sw(t *testing.T, id string, on bool) *protocol.TaskControlResponse {
	return f.call(t, protocol.TaskControlKind_RecordSwitch, func(r *protocol.TaskControlRequest) {
		s := protocol.RecordSwitchRequest{TaskId: hexToTaskID(t, id)}
		s.SetOn(on)
		r.SetRecordSwitch(s)
	})
}

func TestRecordGetNeedsExecViewInScope(t *testing.T) {
	f := newRecFixture(t)
	f.grant(t, protocol.Capability_Notify, authority.Scope{Base: protocol.ScopeBase_Global})
	if r := f.get(t, f.target); !denied(r) {
		t.Fatalf("get without exec_view: %+v", r)
	}
	f.grant(t, protocol.Capability_ExecView, authority.Scope{Base: protocol.ScopeBase_None})
	if r := f.get(t, f.target).RecordGet(); r == nil || r.Status != protocol.RecordStatus_NotFound {
		t.Fatalf("get out of scope: %+v, want not_found", r)
	}
	if r := f.get(t, f.caller).RecordGet(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("get of its own record: %+v", r)
	}
}

func TestRecordGetStreamsEntriesInOrder(t *testing.T) {
	f := newRecFixture(t)
	r := f.get(t, f.target).RecordGet()
	if r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("get: %+v", r)
	}
	rd := bytes.NewReader(f.conn.sendStreamBytes(t, r.StreamId))
	var got []string
	for {
		var e protocol.RecordEntry
		if err := e.Read(rd); err != nil {
			break
		}
		got = append(got, string(e.Payload))
	}
	if fmt.Sprint(got) != "[one two]" {
		t.Fatalf("streamed %v", got)
	}
}

func TestRecordListOverStreamHoldsManyRecords(t *testing.T) {
	f := newRecFixture(t)
	for run := uint32(2); run <= 100; run++ {
		w, _ := f.h.Records.OpenRun(f.target, run, time.Unix(0, int64(run)))
		_ = w.Append(recEntry(1, "x"))
		_ = w.Detach()
	}
	r := f.call(t, protocol.TaskControlKind_RecordList, func(r *protocol.TaskControlRequest) {
		var l protocol.RecordListRequest
		l.SetTaskPresent(true)
		l.SetTaskId(hexToTaskID(t, f.target))
		r.SetRecordList(l)
	}).RecordList()
	if r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("list: %+v", r)
	}
	var body protocol.RecordListBody
	f.conn.streamBody(t, r.StreamId, &body)
	if body.Count != 100 || len(body.Records) != 100 {
		t.Fatalf("listed %d records, want 100", body.Count)
	}
}

func TestRecordDeleteNeedsRecordDelete(t *testing.T) {
	f := newRecFixture(t)
	if r := f.del(t, f.target); !denied(r) {
		t.Fatalf("delete with exec_view only: %+v", r)
	}
	f.grant(t, protocol.Capability_RecordDelete, authority.Scope{Base: protocol.ScopeBase_Global})
	if r := f.del(t, f.target).RecordDelete(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("delete: %+v", r)
	}
	if len(f.h.Records.List(f.target)) != 0 {
		t.Fatal("record still there")
	}
}

// A later retention keeps (record_persist); an earlier one deletes sooner
// (record_delete). The default retention is forever.
func TestRecordKeepGatedByDirection(t *testing.T) {
	f := newRecFixture(t)
	global := authority.Scope{Base: protocol.ScopeBase_Global}
	f.grant(t, protocol.Capability_RecordPersist, global)
	if r := f.keep(t, f.target, false, time.Hour); !denied(r) {
		t.Fatalf("shorten forever→1h with record_persist only: %+v", r)
	}
	f.grant(t, protocol.Capability_RecordDelete, global)
	if r := f.keep(t, f.target, false, time.Hour).RecordKeep(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("shorten with record_delete: %+v", r)
	}
	f.grant(t, protocol.Capability_ExecView, global)
	if r := f.keep(t, f.target, true, 0); !denied(r) {
		t.Fatalf("extend to forever with exec_view only: %+v", r)
	}
	f.grant(t, protocol.Capability_RecordPersist, global)
	if r := f.keep(t, f.target, true, 0).RecordKeep(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("extend with record_persist: %+v", r)
	}
	if forever, _, _ := f.h.Records.Retention(f.target, 1); !forever {
		t.Fatal("retention not changed to forever")
	}
}

func TestRecordSwitchGates(t *testing.T) {
	f := newRecFixture(t)
	global := authority.Scope{Base: protocol.ScopeBase_Global}
	if r := f.sw(t, f.target, true); !denied(r) {
		t.Fatalf("switch on with exec_view only: %+v", r)
	}
	f.grant(t, protocol.Capability_RecordPersist, global)
	if r := f.sw(t, f.target, true).RecordSwitch(); r == nil || r.Status != protocol.RecordStatus_NotRecordable {
		t.Fatalf("switch on with no live session: %+v, want not_recordable", r)
	}
	m := f.liveMux(t)
	if r := f.sw(t, f.target, true).RecordSwitch(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("switch on: %+v", r)
	}
	if e, _ := f.h.Tasks.Get(f.target); !m.Recording() || !e.Record {
		t.Fatalf("after on: mux recording %v, task record %v", m.Recording(), e.Record)
	}
	if r := f.sw(t, f.target, false); !denied(r) {
		t.Fatalf("switch off with record_persist only: %+v", r)
	}
	f.grant(t, protocol.Capability_RecordDelete, global)
	if r := f.sw(t, f.target, false).RecordSwitch(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("switch off: %+v", r)
	}
	if e, _ := f.h.Tasks.Get(f.target); m.Recording() || e.Record {
		t.Fatalf("after off: mux recording %v, task record %v", m.Recording(), e.Record)
	}
}

// A grant whose record_delete override excludes self cannot reach the
// holder's own record, and reaches another task in scope (A-D22, A-D26).
// The override's base is subtree: a global base is every task, self included,
// and exclude_self narrows only the non-global forms (authority.ActionSet).
func TestExcludeSelfProtectsOwnRecord(t *testing.T) {
	f := newRecFixture(t)
	// The target becomes the caller's child, so a subtree covers it.
	if _, ok := f.h.Tasks.SetParent(f.target, hexToTaskID(t, f.caller)); !ok {
		t.Fatal("SetParent failed")
	}
	f.grant(t, protocol.Capability_RecordDelete|protocol.Capability_ExecView, authority.Scope{
		Base:      protocol.ScopeBase_Global,
		Overrides: []authority.ScopeOverride{{Caps: protocol.Capability_RecordDelete, Base: protocol.ScopeBase_Subtree, ExcludeSelf: true}},
	})
	if r := f.del(t, f.caller).RecordDelete(); r == nil || r.Status != protocol.RecordStatus_NotFound {
		t.Fatalf("delete own: %+v, want not_found", r)
	}
	if r := f.keep(t, f.caller, false, time.Hour).RecordKeep(); r == nil || r.Status != protocol.RecordStatus_NotFound {
		t.Fatalf("shorten own: %+v, want not_found", r)
	}
	if r := f.sw(t, f.caller, false).RecordSwitch(); r == nil || r.Status != protocol.RecordStatus_NotFound {
		t.Fatalf("switch own off: %+v, want not_found", r)
	}
	if r := f.keep(t, f.target, false, time.Hour).RecordKeep(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("shorten another's: %+v", r)
	}
	if r := f.del(t, f.target).RecordDelete(); r == nil || r.Status != protocol.RecordStatus_Ok {
		t.Fatalf("delete another's: %+v", r)
	}
	if len(f.h.Records.List(f.caller)) != 1 {
		t.Fatal("the caller's own record is gone")
	}
}
