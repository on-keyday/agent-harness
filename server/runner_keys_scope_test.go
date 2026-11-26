package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// C1 of 2026-11-26-grant-conditions-and-bound-enrollment-design.md:
// runner_enroll is scoped. Global may enroll an unbound key; a narrower scope
// only keys bound to tasks inside it. Unenroll, kick and keys follow the same
// rule, and out of reach answers as an unknown key or runner does.

// scopedEnroller is a task principal holding runner_enroll with scope.
func scopedEnroller(t *testing.T, h *TaskHandler, scope authority.Scope) *fakeConn {
	t.Helper()
	id := h.Tasks.Create("r", "ctl", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil,
		authority.Grant{Caps: protocol.Capability_RunnerEnroll, Scope: scope}, "")
	conn := keyVerbConn()
	if h.principals == nil {
		h.principals = map[objproto.ConnectionID]protocol.TaskID{}
	}
	h.principals[conn.ConnectionID()] = hexToTaskID(t, id)
	return conn
}

// plainTask creates a task nothing special holds, returning its id.
func boundTarget(t *testing.T, h *TaskHandler) protocol.TaskID {
	t.Helper()
	return hexToTaskID(t, h.Tasks.Create("r", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, ""))
}

func idsScope(ids ...protocol.TaskID) authority.Scope {
	s := authority.Scope{Base: protocol.ScopeBase_None}
	for _, id := range ids {
		s.IDs = append(s.IDs, hex.EncodeToString(id.Id[:]))
	}
	return s
}

func (f *keyVerbFixture) enrollBound(t *testing.T, conn *fakeConn, key ed25519.PublicKey, label string, bound ...protocol.TaskID) protocol.RunnerKeyStatus {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerEnroll, RequestId: 1}}
	r := protocol.RunnerEnrollRequest{}
	copy(r.Key[:], key)
	r.SetLabel([]byte(label))
	r.SetBound(bound)
	req.SetRunnerEnroll(r)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if resp.RunnerEnroll() == nil {
		t.Fatalf("no runner_enroll answer: %+v", resp.Kind)
	}
	return resp.RunnerEnroll().Status
}

func (f *keyVerbFixture) keysAs(t *testing.T, conn *fakeConn) []protocol.RunnerKeyRow {
	t.Helper()
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerKeys, RequestId: 3}}
	req.SetRunnerKeys(protocol.RunnerKeysRequest{})
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if resp.RunnerKeys() == nil || resp.RunnerKeys().StreamId == 0 {
		t.Fatalf("runner_keys answer: %+v", resp.Kind)
	}
	var body protocol.RunnerKeysBody
	if err := body.DecodeExact(conn.sendStreamBytes(t, resp.RunnerKeys().StreamId)); err != nil {
		t.Fatal(err)
	}
	return body.Keys
}

func newKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestEnrollScopedToT(t *testing.T) {
	f := newKeyVerbFixture(t)
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	ctl := scopedEnroller(t, f.h, idsScope(T))

	k := newKey(t)
	if st := f.enrollBound(t, ctl, k, "vm-T", T); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("bound [T]: %v, want ok", st)
	}
	if rec, _ := f.keys.Lookup(k); len(rec.Bound) != 1 || rec.Bound[0] != T {
		t.Fatalf("stored binding %v, want [T]", rec.Bound)
	}
	for name, bound := range map[string][]protocol.TaskID{"unbound": nil, "[U]": {U}, "[T,U]": {T, U}} {
		k := newKey(t)
		if st := f.enrollBound(t, ctl, k, "", bound...); st != protocol.RunnerKeyStatus_OutOfReach {
			t.Errorf("%s: %v, want out_of_reach", name, st)
		}
		if _, ok := f.keys.Lookup(k); ok {
			t.Errorf("%s: the key was enrolled despite the refusal", name)
		}
	}
}

func TestEnrollGlobalMayEnrollUnbound(t *testing.T) {
	f := newKeyVerbFixture(t)
	g := scopedEnroller(t, f.h, authority.Scope{Base: protocol.ScopeBase_Global})
	if st := f.enrollBound(t, g, newKey(t), "any"); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("global, unbound: %v, want ok", st)
	}
	if st := f.enrollBound(t, g, newKey(t), "one", boundTarget(t, f.h)); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("global, bound: %v, want ok", st)
	}
}

func TestEnrollBadBinding(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	many := make([]protocol.TaskID, 17)
	for i := range many {
		many[i] = boundTarget(t, f.h)
	}
	if st := f.enrollBound(t, op, newKey(t), "", many...); st != protocol.RunnerKeyStatus_BadBinding {
		t.Errorf("17 tasks: %v, want bad_binding", st)
	}
	if st := f.enrollBound(t, op, newKey(t), "", protocol.TaskID{}); st != protocol.RunnerKeyStatus_BadBinding {
		t.Errorf("zero id: %v, want bad_binding", st)
	}
}

func TestEnrollBindingDiffers(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	k := newKey(t)
	f.enrollBound(t, op, k, "a", T)
	if st := f.enrollBound(t, op, k, "b", U); st != protocol.RunnerKeyStatus_BindingDiffers {
		t.Fatalf("re-enroll with another binding: %v, want binding_differs", st)
	}
	if rec, _ := f.keys.Lookup(k); rec.Label != "a" || len(rec.Bound) != 1 || rec.Bound[0] != T {
		t.Fatalf("record changed: %+v", rec)
	}
}

func TestUnenrollOutOfReachIsNotFound(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	ku, kfree := newKey(t), newKey(t)
	f.enrollBound(t, op, ku, "u", U)
	f.enrollBound(t, op, kfree, "free")
	ctl := scopedEnroller(t, f.h, idsScope(T))
	for name, k := range map[string]ed25519.PublicKey{"bound to U": ku, "unbound": kfree} {
		if st, _ := f.unenroll(t, ctl, hex.EncodeToString(k)[:16], false); st != protocol.RunnerKeyStatus_NotFound {
			t.Errorf("%s: %v, want not_found", name, st)
		}
		if _, ok := f.keys.Lookup(k); !ok {
			t.Errorf("%s: removed by a caller that cannot reach it", name)
		}
	}
	kt := newKey(t)
	f.enrollBound(t, op, kt, "t", T)
	if st, body := f.unenroll(t, ctl, hex.EncodeToString(kt)[:16], false); st != protocol.RunnerKeyStatus_Ok || body == nil || len(body.Bound) != 1 || body.Bound[0] != T {
		t.Fatalf("own key: %v body=%+v, want ok carrying bound [T]", st, body)
	}
}

func TestUnenrollPrefixResolvesAmongReachableOnly(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	a, b := make([]byte, 32), make([]byte, 32)
	a[0], a[1], a[2], a[3], a[31] = 0xab, 0xcd, 0xef, 0x01, 1
	b[0], b[1], b[2], b[3], b[31] = 0xab, 0xcd, 0xef, 0x01, 2
	f.enrollBound(t, op, a, "mine", T)
	f.enrollBound(t, op, b, "theirs", U)
	ctl := scopedEnroller(t, f.h, idsScope(T))
	st, body := f.unenroll(t, ctl, "abcdef01", false)
	if st != protocol.RunnerKeyStatus_Ok || body == nil || body.Key[31] != 1 {
		t.Fatalf("shared prefix: %v body=%+v, want ok on the reachable key (not ambiguous_prefix)", st, body)
	}
}

func TestKeysListsOnlyReachable(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	T := boundTarget(t, f.h)
	f.enrollBound(t, op, newKey(t), "free")
	f.enrollBound(t, op, newKey(t), "vm-T", T)
	ctl := scopedEnroller(t, f.h, idsScope(T))
	rows := f.keysAs(t, ctl)
	if len(rows) != 1 || string(rows[0].Label) != "vm-T" || len(rows[0].Bound) != 1 || rows[0].Bound[0] != T {
		t.Fatalf("scoped keys = %+v, want only vm-T with bound [T]", rows)
	}
	if all := f.keysAs(t, op); len(all) != 2 {
		t.Fatalf("operator keys = %d rows, want 2", len(all))
	}
}

// kick names a runner; its key's binding decides.
func TestKickOutOfReachIsNotFound(t *testing.T) {
	f := newKickFixture(t)
	keys, _ := LoadRunnerKeyStore("", nil)
	f.h.RunnerKeys = keys
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	ku, kfree, kt := newKey(t), newKey(t), newKey(t)
	keys.Enroll(RunnerKeyRecord{Key: ku, Bound: []protocol.TaskID{U}})
	keys.Enroll(RunnerKeyRecord{Key: kfree})
	keys.Enroll(RunnerKeyRecord{Key: kt, Bound: []protocol.TaskID{T}})
	add := func(cid, ident string, key ed25519.PublicKey) {
		id := pcid(t, cid)
		if _, err := f.n.reg.Add(&RunnerEntry{ID: id, Identity: runnerIDOf(ident), Hostname: "h", Key: key,
			Conn: &fakeConn{id: id}, MaxTasks: 1}); err != nil {
			t.Fatal(err)
		}
	}
	add("ws:127.0.0.1:9700-1", "on-u-key", ku)
	add("ws:127.0.0.1:9701-1", "on-free-key", kfree)
	add("ws:127.0.0.1:9702-1", "on-t-key", kt)
	ctl := scopedEnroller(t, f.h, idsScope(T))
	for _, ident := range []string{"on-u-key", "on-free-key"} {
		if st, _ := f.kick(t, ctl, identityHex(ident), false); st != protocol.RunnerKickStatus_NotFound {
			t.Errorf("kick %s: %v, want not_found", ident, st)
		}
	}
	// A fresh connection: an in-reach kick answers asynchronously, and the
	// helper reads the connection's last answer.
	ctl2 := scopedEnroller(t, f.h, idsScope(T))
	if st, _ := f.kick(t, ctl2, identityHex("on-t-key"), false); st != protocol.RunnerKickStatus_ClosedWithoutLeaving && st != protocol.RunnerKickStatus_Left {
		t.Fatalf("kick of a runner on a key bound to T: %v, want it kicked", st)
	}
}

func TestKickPrefixResolvesAmongReachableOnly(t *testing.T) {
	f := newKickFixture(t)
	keys, _ := LoadRunnerKeyStore("", nil)
	f.h.RunnerKeys = keys
	T, U := boundTarget(t, f.h), boundTarget(t, f.h)
	kt, ku := newKey(t), newKey(t)
	keys.Enroll(RunnerKeyRecord{Key: kt, Bound: []protocol.TaskID{T}})
	keys.Enroll(RunnerKeyRecord{Key: ku, Bound: []protocol.TaskID{U}})
	for _, r := range []struct {
		cid, ident string
		key        ed25519.PublicKey
	}{{"ws:127.0.0.1:9710-1", "twin-a", kt}, {"ws:127.0.0.1:9711-1", "twin-b", ku}} {
		id := pcid(t, r.cid)
		if _, err := f.n.reg.Add(&RunnerEntry{ID: id, Identity: runnerIDOf(r.ident), Hostname: "h", Key: r.key,
			Conn: &fakeConn{id: id}, MaxTasks: 1}); err != nil {
			t.Fatal(err)
		}
	}
	ctl := scopedEnroller(t, f.h, idsScope(T))
	st, body := f.kick(t, ctl, identityHex("twin-a")[:8], false)
	if st == protocol.RunnerKickStatus_AmbiguousPrefix || body == nil || body.Runner.Id != runnerIDOf("twin-a") {
		t.Fatalf("shared prefix: %v body=%+v, want the reachable runner twin-a", st, body)
	}
}
