package server

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// The action verbs and C11's gate on an edit
// (2026-11-26-grant-conditions-and-bound-enrollment-design.md, C11, C13).

const actBodyStream = 7

type actEnv struct {
	h      *TaskHandler
	editor string // the editor task E
	ecid   objproto.ConnectionID
	conn   *fakeConn
}

const actAll = protocol.Capability_ActionAdmin | protocol.Capability_ExecRun | protocol.Capability_Resume

// newActEnv makes an editor task E with caps and scope, on its own connection.
func newActEnv(t *testing.T, caps protocol.Capability, scope authority.Scope) *actEnv {
	t.Helper()
	h := newTestHandler(t)
	h.Actions, _ = LoadActionStore("")
	e := h.Tasks.Create("/r", "e", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: caps, Scope: scope}, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9720-1")}
	h.principals = map[objproto.ConnectionID]protocol.TaskID{conn.ConnectionID(): hexToTaskID(t, e)}
	return &actEnv{h: h, editor: e, ecid: conn.ConnectionID(), conn: conn}
}

var operatorCID = objproto.ConnectionID{}

func defBytes(d protocol.ActionDefinition) []byte { return d.MustAppend(nil) }

// put runs handleActionPut with def on its body stream.
func (e *actEnv) put(t *testing.T, cid objproto.ConnectionID, id [16]byte, expect uint32, body []byte) protocol.ActionPutResponse {
	t.Helper()
	conn := gcConn{fakeConn: &fakeConn{id: cid}, streams: map[trsf.StreamID]trsf.ReceiveStream{actBodyStream: &fakeRecv{data: body}}}
	return e.h.handleActionPut(conn, cid, protocol.ActionPutRequest{Id: protocol.ActionID{Id: id}, ExpectRevision: expect, PayloadStreamId: actBodyStream})
}

// create makes action name as the operator and returns its id.
func (e *actEnv) create(t *testing.T, name string) [16]byte {
	t.Helper()
	r := e.put(t, operatorCID, [16]byte{}, 0, defBytes(actDef(t, name, nil, "echo", name)))
	if r.Status != protocol.ActionStatus_Ok {
		t.Fatalf("create %s: %v %q", name, r.Status, r.Message)
	}
	return r.Id.Id
}

func actionCond(ids ...[16]byte) *authority.Condition {
	return &authority.Condition{Kind: protocol.ConditionKind_ActionSet, Actions: ids}
}

// runOver is a grant whose exec_run override is base, conditioned on ids.
func runOver(base protocol.ScopeBase, ids ...[16]byte) authority.Scope {
	return authority.Scope{Base: protocol.ScopeBase_Subtree, Overrides: []authority.ScopeOverride{
		{Caps: protocol.Capability_ExecRun, Base: base, Cond: actionCond(ids...)}}}
}

// holder makes a task created by creator ("" = the operator) whose grant
// names id through an exec_run override of base.
func (e *actEnv) holder(t *testing.T, creator string, base protocol.ScopeBase, id [16]byte) string {
	t.Helper()
	var c protocol.TaskID
	if creator != "" {
		c = hexToTaskID(t, creator)
	}
	return e.h.Tasks.Create("/r", "h", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, c, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_ExecRun, Scope: runOver(base, id)}, "")
}

// edit edits id at revision 1 as the editor, to "echo changed".
func (e *actEnv) edit(t *testing.T, id [16]byte) protocol.ActionPutResponse {
	t.Helper()
	cur, _ := e.h.Actions.Get(id)
	return e.put(t, e.ecid, id, 1, defBytes(actDef(t, string(cur.Definition.Name), nil, "echo", "changed")))
}

func (e *actEnv) unchanged(t *testing.T, id [16]byte) {
	t.Helper()
	cur, _ := e.h.Actions.Get(id)
	if cur.Revision != 1 || string(cur.Definition.Template[1].Literal().Bytes) == "changed" {
		t.Fatalf("a refused edit changed the action: %+v", cur)
	}
}

func actRefusedWith(t *testing.T, r protocol.ActionPutResponse, n uint32) {
	t.Helper()
	if r.Status != protocol.ActionStatus_HoldersUnreachable || r.HoldersRefused != n {
		t.Fatalf("edit: %v holders_refused=%d %q, want holders_unreachable with %d", r.Status, r.HoldersRefused, r.Message, n)
	}
}

func TestActionCreateByHolderOfActionAdmin(t *testing.T) {
	e := newActEnv(t, protocol.Capability_ActionAdmin, authority.Scope{})
	r := e.put(t, e.ecid, [16]byte{}, 0, defBytes(actDef(t, "hello", []string{"who=token"}, "echo", "hello", "{who}")))
	if r.Status != protocol.ActionStatus_Ok || r.Revision != 1 || r.Id.Id == ([16]byte{}) {
		t.Fatalf("create: %+v", r)
	}
	rec, _ := e.h.Actions.Get(r.Id.Id)
	if string(rec.CreatedBy) != "task:"+e.editor {
		t.Fatalf("created_by = %q", rec.CreatedBy)
	}
}

func handleAs(t *testing.T, e *actEnv, req *protocol.TaskControlRequest) protocol.TaskControlResponse {
	t.Helper()
	e.h.Handle(e.conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, e.conn)
}

func TestActionPutWithoutActionAdminIsPermissionDenied(t *testing.T) {
	e := newActEnv(t, protocol.Capability_ExecRun, authority.Scope{})
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_ActionPut, protocol.TaskControlKind_ActionRemove} {
		req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: k, RequestId: 3}}
		if k == protocol.TaskControlKind_ActionPut {
			req.SetActionPut(protocol.ActionPutRequest{PayloadStreamId: actBodyStream})
		} else {
			req.SetActionRemove(protocol.ActionRemoveRequest{})
		}
		if resp := handleAs(t, e, req); resp.Kind != protocol.TaskControlKind_PermissionDenied {
			t.Fatalf("%v without action_admin: %v, want permission_denied", k, resp.Kind)
		}
	}
}

func listAs(t *testing.T, caps protocol.Capability) protocol.TaskControlResponse {
	e := newActEnv(t, caps, authority.Scope{})
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ActionList, RequestId: 4}}
	req.SetActionList(protocol.ActionListRequest{})
	return handleAs(t, e, req)
}

func TestActionListAdmitsExecRunHolder(t *testing.T) {
	for _, caps := range []protocol.Capability{protocol.Capability_ExecRun, protocol.Capability_ActionAdmin} {
		resp := listAs(t, caps)
		if resp.Kind != protocol.TaskControlKind_ActionList || resp.ActionList() == nil || resp.ActionList().Status != protocol.ActionStatus_Ok {
			t.Fatalf("list with %v: %v", caps, resp.Kind)
		}
	}
}

func TestActionListRefusesNeither(t *testing.T) {
	if resp := listAs(t, protocol.Capability_Cancel); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("list with neither bit: %v, want permission_denied", resp.Kind)
	}
}

func TestActionEditByOperator(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	id := e.create(t, "a")
	e.holder(t, "", protocol.ScopeBase_Global, id)
	r := e.put(t, operatorCID, id, 1, defBytes(actDef(t, "a", nil, "echo", "changed")))
	if r.Status != protocol.ActionStatus_Ok || r.Revision != 2 {
		t.Fatalf("operator edit: %+v", r)
	}
}

func TestActionEditByTaskThatReachesEveryHolder(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Subtree})
	id := e.create(t, "a")
	e.holder(t, e.editor, protocol.ScopeBase_Subtree, id)
	if r := e.edit(t, id); r.Status != protocol.ActionStatus_Ok || r.Revision != 2 {
		t.Fatalf("edit: %v %q", r.Status, r.Message)
	}
	rec, _ := e.h.Actions.Get(id)
	if string(rec.EditedBy) != "task:"+e.editor {
		t.Fatalf("edited_by = %q", rec.EditedBy)
	}
}

// R1: a holder that is not a strict descendant in the editor's resume scope.
func TestActionEditRefusedR1(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Global})
	id := e.create(t, "a")
	e.holder(t, e.editor, protocol.ScopeBase_Subtree, id)
	e.holder(t, "", protocol.ScopeBase_Subtree, id)
	actRefusedWith(t, e.edit(t, id), 1)
	e.unchanged(t, id)
}

// R2: a descendant holder whose exec_run reaches a target the editor's does not.
func TestActionEditRefusedR2(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Subtree})
	id := e.create(t, "a")
	e.holder(t, e.editor, protocol.ScopeBase_Global, id)
	actRefusedWith(t, e.edit(t, id), 1)
	e.unchanged(t, id)
}

func TestActionEditRefusedWhenEditorExecRunConditioned(t *testing.T) {
	other := [16]byte{0xee}
	e := newActEnv(t, actAll, runOver(protocol.ScopeBase_Subtree, other))
	id := e.create(t, "a")
	e.holder(t, e.editor, protocol.ScopeBase_Subtree, id)
	actRefusedWith(t, e.edit(t, id), 1)
	e.unchanged(t, id)
}

func TestActionEditRefusedForSelfHolder(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Subtree})
	id := e.create(t, "a")
	e.h.Tasks.mu.Lock()
	e.h.Tasks.tasks[e.editor].Scope = runOver(protocol.ScopeBase_Subtree, id)
	e.h.Tasks.mu.Unlock()
	r := e.edit(t, id)
	if r.Status != protocol.ActionStatus_HoldersUnreachable || !strings.Contains(string(r.Message), "own") {
		t.Fatalf("self holder: %v %q", r.Status, r.Message)
	}
	e.unchanged(t, id)
}

// A finished task is a holder: it can be resumed with its recorded caps.
func TestActionEditRefusedForFinishedHolderOutsideReach(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Subtree})
	id := e.create(t, "a")
	h := e.holder(t, "", protocol.ScopeBase_Subtree, id)
	e.h.Tasks.Finish(h, 0, nil)
	actRefusedWith(t, e.edit(t, id), 1)
	e.unchanged(t, id)
}

func TestActionEditRefusedForScheduleHolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps protocol.Capability
		ok   bool
	}{
		{"without schedule_edit", actAll, false},
		{"with schedule_edit and reach", actAll | protocol.Capability_ScheduleEdit, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newActEnv(t, tc.caps, authority.Scope{Base: protocol.ScopeBase_Subtree})
			st, _ := LoadScheduleStore("")
			e.h.Sched = &ScheduleOps{Store: st}
			id := e.create(t, "a")
			sc := Schedule{ID: strings.Repeat("ab", 16), Cron: "0 * * * *", TZ: "UTC", State: protocol.ScheduleTaskState_Unbound,
				Creator: e.editor, Grant: authority.Grant{Caps: protocol.Capability_ExecRun, Scope: runOver(protocol.ScopeBase_Subtree, id)}}
			if err := st.Put(sc); err != nil {
				t.Fatal(err)
			}
			r := e.edit(t, id)
			if tc.ok {
				if r.Status != protocol.ActionStatus_Ok {
					t.Fatalf("edit: %v %q", r.Status, r.Message)
				}
				return
			}
			actRefusedWith(t, r, 1)
			e.unchanged(t, id)
		})
	}
}

// Deleting only narrows, so action_admin alone suffices, holders or not.
func TestActionRemoveNeedsOnlyActionAdmin(t *testing.T) {
	e := newActEnv(t, protocol.Capability_ActionAdmin, authority.Scope{})
	id := e.create(t, "a")
	e.holder(t, "", protocol.ScopeBase_Global, id)
	if r := e.h.handleActionRemove(e.ecid, protocol.ActionRemoveRequest{Id: protocol.ActionID{Id: id}}); r.Status != protocol.ActionStatus_Ok {
		t.Fatalf("remove: %v", r.Status)
	}
	if r := e.h.handleActionRemove(e.ecid, protocol.ActionRemoveRequest{Id: protocol.ActionID{Id: id}}); r.Status != protocol.ActionStatus_NotFound {
		t.Fatalf("remove twice: %v", r.Status)
	}
}

func TestActionPutStaleRevision(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	id := e.create(t, "a")
	if r := e.put(t, operatorCID, id, 1, defBytes(actDef(t, "a", nil, "echo", "x"))); r.Status != protocol.ActionStatus_Ok {
		t.Fatalf("first edit: %v", r.Status)
	}
	if r := e.put(t, operatorCID, id, 1, defBytes(actDef(t, "a", nil, "echo", "y"))); r.Status != protocol.ActionStatus_StaleRevision {
		t.Fatalf("second edit at 1: %v", r.Status)
	}
	if r := e.put(t, operatorCID, [16]byte{9}, 1, defBytes(actDef(t, "z", nil, "true"))); r.Status != protocol.ActionStatus_NotFound {
		t.Fatalf("edit of an unknown id: %v", r.Status)
	}
}

func TestActionPutNameTaken(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	e.create(t, "a")
	if r := e.put(t, operatorCID, [16]byte{}, 0, defBytes(actDef(t, "a", nil, "true"))); r.Status != protocol.ActionStatus_NameTaken {
		t.Fatalf("create a twice: %v", r.Status)
	}
}

func TestActionPutInvalidDefinitionNamesTheRule(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	d := actDef(t, "a", []string{"who=token"}, "echo", "{who}")
	d.SetTemplate(d.Template[:1]) // {who} no longer used
	r := e.put(t, operatorCID, [16]byte{}, 0, defBytes(d))
	if r.Status != protocol.ActionStatus_InvalidDefinition || !strings.Contains(string(r.Message), "not used") {
		t.Fatalf("unused param: %v %q", r.Status, r.Message)
	}
	if r := e.put(t, operatorCID, [16]byte{}, 3, defBytes(actDef(t, "b", nil, "true"))); r.Status != protocol.ActionStatus_InvalidDefinition {
		t.Fatalf("create naming a revision: %v", r.Status)
	}
	id := e.create(t, "c")
	if r := e.put(t, operatorCID, id, 0, defBytes(actDef(t, "c", nil, "true"))); r.Status != protocol.ActionStatus_InvalidDefinition {
		t.Fatalf("edit naming revision 0: %v", r.Status)
	}
}

func TestActionPutBadBody(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	if r := e.put(t, operatorCID, [16]byte{}, 0, []byte{0xff}); r.Status != protocol.ActionStatus_BadBody {
		t.Fatalf("undecodable body: %v", r.Status)
	}
}

// The response is inline: its message is cut so the whole fits one datagram.
func TestActionPutResponseFitsADatagram(t *testing.T) {
	e := newActEnv(t, actAll, authority.Scope{})
	d := actDef(t, "a", nil, "true")
	d.SetName([]byte(strings.Repeat("X", 255))) // invalid; the error quotes it
	r := e.put(t, operatorCID, [16]byte{}, 0, defBytes(d))
	if r.Status != protocol.ActionStatus_InvalidDefinition {
		t.Fatalf("status %v", r.Status)
	}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ActionPut, RequestId: 0xffffffff}
	resp.SetActionPut(r)
	if n := len(resp.MustAppend(nil)); n > 300 {
		t.Fatalf("response is %d bytes, want <= 300", n)
	}
}

// schedule show carries the overlap warning ls and whoami carry: a stored
// grant whose ActionSet-conditioned exec_run reaches a target it may also
// write files in is computed over the task each firing runs as.
func TestScheduleDetailCarriesActionOverlap(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps protocol.Capability
		want protocol.Capability
	}{
		{"file_write over the same task", protocol.Capability_ExecRun | protocol.Capability_FileWrite, protocol.Capability_FileWrite},
		{"control: exec_run alone", protocol.Capability_ExecRun, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newActEnv(t, actAll, authority.Scope{Base: protocol.ScopeBase_Subtree})
			st, _ := LoadScheduleStore("")
			e.h.Sched = &ScheduleOps{Store: st}
			id := e.create(t, "a")
			sc := Schedule{ID: strings.Repeat("ab", 16), Cron: "0 * * * *", TZ: "UTC", State: protocol.ScheduleTaskState_PendingCreate,
				Grant: authority.Grant{Caps: tc.caps, Scope: runOver(protocol.ScopeBase_Subtree, id)}}
			if err := st.Put(sc); err != nil {
				t.Fatal(err)
			}
			var sid protocol.ScheduleID
			copy(sid.Id[:], []byte{0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab})
			d, refusal := e.h.scheduleDetail(operatorCID, sid)
			if refusal != nil {
				t.Fatalf("%v %s", refusal.Status, refusal.Message)
			}
			if d.ActionOverlap != tc.want {
				t.Fatalf("action_overlap %v, want %v", d.ActionOverlap, tc.want)
			}
		})
	}
}
