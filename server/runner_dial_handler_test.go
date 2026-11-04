package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// newDialVerbFixture: the key-verb fixture plus a dial store and a manager
// whose dials always fail, so a test sees the handler's decisions and none of
// the network.
func newDialVerbFixture(t *testing.T) *keyVerbFixture {
	t.Helper()
	f := newKeyVerbFixture(t)
	f.h.RunnerDials, _ = LoadRunnerDialStore("")
	f.h.DialLegs = func(transport string) bool { return transport == "ws" }
	f.h.Dials = newDialManager(dialManagerConfig{
		Store: f.h.RunnerDials, Keys: f.keys, Registry: f.h.Registry,
		Dial: func(context.Context, objproto.ConnectionID, *DialOrigin) error {
			return errors.New("no network in this test")
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.h.Dials.Start(ctx)
	return f
}

func dialAdd(t *testing.T, f *keyVerbFixture, conn *fakeConn, keyPrefix, label string, cands ...string) (protocol.RunnerDialStatus, uint32) {
	t.Helper()
	r := protocol.RunnerDialAddRequest{}
	r.SetKeyPrefix([]byte(keyPrefix))
	r.SetLabel([]byte(label))
	cs := make([]protocol.RunnerDialCandidate, len(cands))
	for i, c := range cands {
		cs[i].SetText([]byte(c))
	}
	r.SetCandidates(cs)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialAdd, RequestId: 3}
	req.SetRunnerDialAdd(r)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	a := resp.RunnerDialAdd()
	if a == nil {
		t.Fatalf("no runner_dial_add answer: %v", resp.Kind)
	}
	return a.Status, a.Id
}

func TestRunnerDialAddValidates(t *testing.T) {
	f := newDialVerbFixture(t)
	op := keyVerbConn()
	k1, _, _ := ed25519.GenerateKey(nil)
	k2 := append(ed25519.PublicKey(nil), k1...)
	k2[31] ^= 1 // shares a long prefix with k1
	for _, k := range []ed25519.PublicKey{k1, k2} {
		if st := f.enroll(t, op, k, ""); st != protocol.RunnerKeyStatus_Ok {
			t.Fatalf("enroll: %v", st)
		}
	}
	full := hex.EncodeToString(k1)
	long := strings.Repeat("x", 65)
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "ws:127.0.0.1:" + itoa(9000+i) + "-*"
	}
	big := make([]string, 3)
	for i := range big { // 3 x ~200 bytes > 512
		big[i] = "ws:" + strings.Repeat("h", 190) + ".example:" + itoa(9000+i) + "-*"
	}
	cases := []struct {
		name   string
		prefix string
		label  string
		cands  []string
		want   protocol.RunnerDialStatus
	}{
		{"no candidates", full, "", nil, protocol.RunnerDialStatus_InvalidCandidates},
		{"nine candidates", full, "", nine, protocol.RunnerDialStatus_InvalidCandidates},
		{"over 512 bytes", full, "", big, protocol.RunnerDialStatus_InvalidCandidates},
		{"label over 64", full, long, []string{"ws:127.0.0.1:9100-*"}, protocol.RunnerDialStatus_InvalidCandidates},
		{"unparsable", full, "", []string{"not-a-cid"}, protocol.RunnerDialStatus_InvalidCandidates},
		{"unknown key", "00000000", "", []string{"ws:127.0.0.1:9101-*"}, protocol.RunnerDialStatus_KeyNotFound},
		{"short prefix", full[:7], "", []string{"ws:127.0.0.1:9102-*"}, protocol.RunnerDialStatus_KeyNotFound},
		{"ambiguous key", full[:62], "", []string{"ws:127.0.0.1:9103-*"}, protocol.RunnerDialStatus_AmbiguousKey},
	}
	for _, c := range cases {
		if st, _ := dialAdd(t, f, op, c.prefix, c.label, c.cands...); st != c.want {
			t.Errorf("%s: %v, want %v", c.name, st, c.want)
		}
	}
	if n := len(f.h.RunnerDials.List()); n != 0 {
		t.Fatalf("%d records stored by refused adds", n)
	}

	st, id := dialAdd(t, f, op, full, "aws", "ws:runner.example:8540-*", "ws:127.0.0.1:8540-*")
	if st != protocol.RunnerDialStatus_Ok || id == 0 {
		t.Fatalf("ok add: %v id=%d", st, id)
	}
	rec, ok := f.h.RunnerDials.Get(id)
	if !ok || rec.Label != "aws" || !rec.Key.Equal(k1) || rec.AddedBy != "operator" || len(rec.Candidates) != 2 {
		t.Fatalf("stored %+v", rec)
	}
	if st, _ := dialAdd(t, f, op, full, "", "ws:127.0.0.1:8540-*"); st != protocol.RunnerDialStatus_DuplicateCandidate {
		t.Fatalf("same candidate text again: %v, want duplicate_candidate", st)
	}
}

func TestRunnerDialAddRefusesTransportWithoutLeg(t *testing.T) {
	f := newDialVerbFixture(t)
	op := keyVerbConn()
	k, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, k, "")
	if st, _ := dialAdd(t, f, op, hex.EncodeToString(k), "", "udp:127.0.0.1:8541-*"); st != protocol.RunnerDialStatus_InvalidCandidates {
		t.Fatalf("udp candidate on a ws-only server: %v, want invalid_candidates", st)
	}
}

func TestRunnerDialCapabilities(t *testing.T) {
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_RunnerDialAdd,
		protocol.TaskControlKind_RunnerDialList, protocol.TaskControlKind_RunnerDialRemove} {
		if got := requiredCap[k]; got != protocol.Capability_RunnerAdmin {
			t.Errorf("%v gated on %v, want runner_admin", k, got)
		}
	}
	f := newDialVerbFixture(t)
	op := keyVerbConn()
	k, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, k, "")
	// A task holding runner_enroll but not runner_admin is refused.
	agent := keyVerbConn()
	tid := f.h.Tasks.Create("r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_RunnerEnroll}, "")
	if f.h.principals == nil {
		f.h.principals = map[string]protocol.TaskID{}
	}
	f.h.principals[agent.ConnectionID().String()] = hexToTaskID(t, tid)
	r := protocol.RunnerDialAddRequest{}
	r.SetKeyPrefix([]byte(hex.EncodeToString(k)))
	r.SetCandidates([]protocol.RunnerDialCandidate{{}})
	r.Candidates[0].SetText([]byte("ws:127.0.0.1:9200-*"))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialAdd, RequestId: 1}
	req.SetRunnerDialAdd(r)
	f.h.Handle(agent, encodeTaskControlRequest(t, req))
	if resp := lastTaskControlResponse(t, agent); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("runner_enroll alone added a dial record: %v", resp.Kind)
	}
	if n := len(f.h.RunnerDials.List()); n != 0 {
		t.Fatal("the record was stored despite the denial")
	}
}

func dialList(t *testing.T, f *keyVerbFixture, conn *fakeConn) []protocol.RunnerDialRow {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialList, RequestId: 4}
	req.SetRunnerDialList(protocol.RunnerDialListRequest{})
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	l := resp.RunnerDialList()
	if l == nil {
		t.Fatalf("no runner_dial_list answer: %v", resp.Kind)
	}
	var body protocol.RunnerDialListBody
	if err := body.DecodeExact(conn.sendStreamBytes(t, l.StreamId)); err != nil {
		t.Fatal(err)
	}
	return body.Rows
}

func TestRunnerDialListAndRemove(t *testing.T) {
	f := newDialVerbFixture(t)
	op := keyVerbConn()
	k, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, k, "")
	_, id := dialAdd(t, f, op, hex.EncodeToString(k), "aws", "ws:127.0.0.1:9300-*")
	rows := dialList(t, f, op)
	if len(rows) != 1 || rows[0].Id != id || string(rows[0].Label) != "aws" || string(rows[0].AddedBy) != "operator" {
		t.Fatalf("rows %+v", rows)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialRemove, RequestId: 5}
	req.SetRunnerDialRemove(protocol.RunnerDialRemoveRequest{Id: id})
	removeStatus := func() protocol.RunnerDialStatus {
		f.h.Handle(op, encodeTaskControlRequest(t, req))
		resp := lastTaskControlResponse(t, op)
		r := resp.RunnerDialRemove()
		if r == nil {
			t.Fatalf("no runner_dial_remove answer: %v", resp.Kind)
		}
		return r.Status
	}
	if st := removeStatus(); st != protocol.RunnerDialStatus_Ok {
		t.Fatalf("remove: %v", st)
	}
	if st := removeStatus(); st != protocol.RunnerDialStatus_NotFound {
		t.Fatalf("second remove: %v", st)
	}
	if rows := dialList(t, f, op); len(rows) != 0 {
		t.Fatalf("after remove: %+v", rows)
	}
}

func TestUnenrollRemovesBoundRecords(t *testing.T) {
	f := newDialVerbFixture(t)
	op := keyVerbConn()
	k, _, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, k, "")
	f.enroll(t, op, other, "")
	_, a := dialAdd(t, f, op, hex.EncodeToString(k), "", "ws:127.0.0.1:9400-*")
	_, b := dialAdd(t, f, op, hex.EncodeToString(k), "", "ws:127.0.0.1:9401-*")
	_, keep := dialAdd(t, f, op, hex.EncodeToString(other), "", "ws:127.0.0.1:9402-*")
	st, body := f.unenroll(t, op, hex.EncodeToString(k)[:8], false)
	if st != protocol.RunnerKeyStatus_Ok || body == nil {
		t.Fatalf("unenroll: %v", st)
	}
	got := map[uint32]bool{}
	for _, id := range body.RemovedDials {
		got[id] = true
	}
	if len(got) != 2 || !got[a] || !got[b] {
		t.Fatalf("removed_dials = %v, want %d and %d", body.RemovedDials, a, b)
	}
	if l := f.h.RunnerDials.List(); len(l) != 1 || l[0].ID != keep {
		t.Fatalf("left %+v", l)
	}
	if f.h.Dials.running(a) || f.h.Dials.running(b) {
		t.Fatal("the removed records' loops still run")
	}
}
