package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type keyVerbFixture struct {
	h      *TaskHandler
	keys   *RunnerKeyStore
	kicked []objproto.ConnectionID
	mu     sync.Mutex
}

func newKeyVerbFixture(t *testing.T) *keyVerbFixture {
	t.Helper()
	keys, _ := LoadRunnerKeyStore("", nil)
	f := &keyVerbFixture{keys: keys}
	f.h = newTestTaskHandler(NewTaskStore())
	f.h.RunnerKeys = keys
	f.h.KickConn = func(cid objproto.ConnectionID) {
		f.mu.Lock()
		f.kicked = append(f.kicked, cid)
		f.mu.Unlock()
	}
	return f
}

var keyVerbConnSeq = 100

func keyVerbConn() *fakeConn {
	keyVerbConnSeq++
	return &fakeConn{id: tcid("ws:127.0.0.1:9900-" + itoa(keyVerbConnSeq)), autoSendStreams: true}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (f *keyVerbFixture) enroll(t *testing.T, conn *fakeConn, key ed25519.PublicKey, label string) protocol.RunnerKeyStatus {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerEnroll, RequestId: 1}
	r := protocol.RunnerEnrollRequest{}
	copy(r.Key[:], key)
	r.SetLabel([]byte(label))
	req.SetRunnerEnroll(r)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	if resp.RunnerEnroll() == nil {
		t.Fatalf("no runner_enroll answer: %+v", resp.Kind)
	}
	return resp.RunnerEnroll().Status
}

func (f *keyVerbFixture) unenroll(t *testing.T, conn *fakeConn, prefix string, force bool) (protocol.RunnerKeyStatus, *protocol.RunnerUnenrollBody) {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerUnenroll, RequestId: 2}
	r := protocol.RunnerUnenrollRequest{}
	r.SetPrefix([]byte(prefix))
	r.SetForce(force)
	req.SetRunnerUnenroll(r)
	before := len(conn.sendStreams)
	f.h.Handle(conn, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, conn)
	u := resp.RunnerUnenroll()
	if u == nil {
		t.Fatalf("no runner_unenroll answer: %+v", resp.Kind)
	}
	if len(conn.sendStreams) == before {
		return u.Status, nil
	}
	var body protocol.RunnerUnenrollBody
	waitStreamBytes(t, conn, before)
	if err := body.DecodeExact(conn.sendStreams[before].bytes); err != nil {
		t.Fatal(err)
	}
	return u.Status, &body
}

func waitStreamBytes(t *testing.T, conn *fakeConn, i int) {
	t.Helper()
	for n := 0; n < 100; n++ {
		conn.sentMu.Lock()
		ok := len(conn.sendStreams[i].bytes) > 0
		conn.sentMu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the body was never written")
}

func TestRunnerVerbsNeedRunnerEnroll(t *testing.T) {
	f := newKeyVerbFixture(t)
	confined := keyVerbConn()
	tid := f.h.Tasks.Create("r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None}, "")
	if f.h.principals == nil {
		f.h.principals = map[string]protocol.TaskID{}
	}
	f.h.principals[confined.ConnectionID().String()] = hexToTaskID(t, tid)
	for _, kind := range []protocol.TaskControlKind{protocol.TaskControlKind_RunnerEnroll, protocol.TaskControlKind_RunnerUnenroll, protocol.TaskControlKind_RunnerKeys} {
		if got := requiredCap[kind]; got != protocol.Capability_RunnerEnroll {
			t.Errorf("%v gated on %v, want runner_enroll", kind, got)
		}
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerEnroll, RequestId: 1}
	r := protocol.RunnerEnrollRequest{}
	copy(r.Key[:], pub)
	req.SetRunnerEnroll(r)
	f.h.Handle(confined, encodeTaskControlRequest(t, req))
	if resp := lastTaskControlResponse(t, confined); resp.Kind != protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("a --caps none task enrolled a key: %v", resp.Kind)
	}
	if _, ok := f.keys.Lookup(pub); ok {
		t.Fatal("the key was enrolled despite the denial")
	}
}

func TestRunnerEnrollRecordsEnrolledBy(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	pub, _, _ := ed25519.GenerateKey(nil)
	if st := f.enroll(t, op, pub, "gmk"); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("enroll: %v", st)
	}
	if rec, _ := f.keys.Lookup(pub); rec.EnrolledBy != "operator" || rec.Label != "gmk" {
		t.Fatalf("record %+v, want enrolled_by=operator label=gmk", rec)
	}
	if st := f.enroll(t, op, pub, ""); st != protocol.RunnerKeyStatus_AlreadyEnrolled {
		t.Fatalf("re-enroll: %v, want already_enrolled", st)
	}

	agent := keyVerbConn()
	tid := f.h.Tasks.Create("r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_RunnerEnroll}, "")
	if f.h.principals == nil {
		f.h.principals = map[string]protocol.TaskID{}
	}
	f.h.principals[agent.ConnectionID().String()] = hexToTaskID(t, tid)
	pub2, _, _ := ed25519.GenerateKey(nil)
	if st := f.enroll(t, agent, pub2, "w"); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("agent enroll: %v", st)
	}
	if rec, _ := f.keys.Lookup(pub2); rec.EnrolledBy != "task:"+tid {
		t.Fatalf("enrolled_by = %q, want task:%s", rec.EnrolledBy, tid)
	}
}

func TestUnenrollInUseNeedsForceAndUndoRestores(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	pub, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, pub, "gmk")
	runnerCID := tcid("ws:127.0.0.1:9901-1")
	f.h.Registry.Add(&RunnerEntry{ID: runnerCID, Identity: testRunnerID("ws:127.0.0.1:9901-1"), Hostname: "gmkhost", Key: pub, Conn: &fakeConn{id: runnerCID}})
	prefix := hex.EncodeToString(pub)[:12]

	st, body := f.unenroll(t, op, prefix, false)
	if st != protocol.RunnerKeyStatus_InUse || body == nil || len(body.Runners) != 1 || string(body.Runners[0].Hostname) != "gmkhost" {
		t.Fatalf("unforced unenroll of a key in use: %v body=%+v", st, body)
	}
	if _, ok := f.keys.Lookup(pub); !ok {
		t.Fatal("the key was removed without --force")
	}
	if len(f.kicked) != 0 {
		t.Fatal("a runner was cut off without --force")
	}

	st, body = f.unenroll(t, op, prefix, true)
	if st != protocol.RunnerKeyStatus_Ok || body == nil || !ed25519.PublicKey(body.Key[:]).Equal(pub) || string(body.Label) != "gmk" {
		t.Fatalf("forced unenroll: %v body=%+v", st, body)
	}
	if len(f.kicked) != 1 || f.kicked[0] != runnerCID {
		t.Fatalf("kicked %v, want the runner's connection", f.kicked)
	}
	// The undo the CLI prints is `runner enroll <full key> --label gmk`.
	if st := f.enroll(t, op, ed25519.PublicKey(body.Key[:]), string(body.Label)); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("undo: %v", st)
	}
}

func TestUnenrollUnusedKeyNeedsNoForce(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	pub, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, pub, "old")
	if st, _ := f.unenroll(t, op, hex.EncodeToString(pub)[:8], false); st != protocol.RunnerKeyStatus_Ok {
		t.Fatalf("unenroll of an unused key: %v", st)
	}
	if st, _ := f.unenroll(t, op, hex.EncodeToString(pub)[:8], false); st != protocol.RunnerKeyStatus_NotFound {
		t.Fatalf("second unenroll: %v, want not_found", st)
	}
	if st, _ := f.unenroll(t, op, "abc", false); st != protocol.RunnerKeyStatus_InvalidKey {
		t.Fatalf("3-hex prefix: %v, want invalid_key", st)
	}
}

func TestRunnerKeysListsUsers(t *testing.T) {
	f := newKeyVerbFixture(t)
	op := keyVerbConn()
	pub, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, pub, "gmk")
	f.h.Registry.Add(&RunnerEntry{ID: tcid("ws:127.0.0.1:9902-1"), Identity: testRunnerID("a"), Hostname: "gmkhost", Key: pub, Conn: &fakeConn{id: tcid("ws:127.0.0.1:9902-1")}})

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerKeys, RequestId: 3}
	req.SetRunnerKeys(protocol.RunnerKeysRequest{})
	f.h.Handle(op, encodeTaskControlRequest(t, req))
	if resp := lastTaskControlResponse(t, op); resp.RunnerKeys() == nil || resp.RunnerKeys().StreamId == 0 {
		t.Fatalf("runner_keys answer: %+v", resp.Kind)
	}
	waitStreamBytes(t, op, len(op.sendStreams)-1)
	var body protocol.RunnerKeysBody
	if err := body.DecodeExact(op.sendStreams[len(op.sendStreams)-1].bytes); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) != 1 || string(body.Keys[0].Label) != "gmk" || len(body.Keys[0].Runners) != 1 || string(body.Keys[0].Runners[0].Hostname) != "gmkhost" {
		t.Fatalf("keys %+v", body.Keys)
	}
	if string(body.Keys[0].EnrolledBy) != "operator" {
		t.Fatalf("enrolled_by %q", body.Keys[0].EnrolledBy)
	}
}
