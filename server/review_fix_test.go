package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// C1 (final review): a record that ADOPTED a connection must notice when it
// drops and dial again. The adopted connection's id is the registry's, not the
// candidate re-resolved with a fresh random `-*` id.
func TestDialerAdoptedConnectionDropRedials(t *testing.T) {
	f := newDialerFixture(t)
	oneShot := pcid(t, "ws:127.0.0.1:9620-77")
	if _, err := f.n.reg.Add(&RunnerEntry{ID: oneShot, Identity: runnerIDOf("one-runner"), Key: f.key,
		Conn: &fakeConn{id: oneShot}, ActiveTasks: map[string]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9620-*")
	eventually(t, "adopted", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	f.n.reg.Remove(oneShot)
	eventually(t, "re-dialed after the adopted connection dropped", func() bool {
		e, ok := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
		return f.n.callCount() > 0 && ok && e.ID != oneShot
	})
}

// C2 (final review): `runner dial rm` stops re-dialing and leaves the live
// connection up (D17) — against a REAL connection, which fakeNet cannot show.
func TestDialRecordRemoveKeepsRealConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ka, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: ka.Public().(ed25519.PublicKey)}})
	dials, _ := LoadRunnerDialStore("")
	rec, _ := dials.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:18691-*"}, Key: ka.Public().(ed25519.PublicKey)})
	f := newOriginFixtureWith(t, ctx, "18690", "18691", ka, Config{RunnerKeys: keys, RunnerDials: dials, DataDir: t.TempDir()})
	e := f.waitRunner(t, "origin-runner")
	if _, err := dials.Remove(rec.ID); err != nil {
		t.Fatal(err)
	}
	f.s.dials.Stop(rec.ID)
	time.Sleep(time.Second)
	if _, ok := f.s.registry.Get(e.ID); !ok {
		t.Fatal("removing the record closed the runner's live connection")
	}
}

// I1 (final review): a connection the gate refuses records the gate's reason,
// not a generic "closed before registering".
func TestDialRecordNotEnrolledRunnerShowsGateReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ka, _ := ed25519.GenerateKey(nil) // the runner's key: NOT enrolled
	_, kb, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: kb.Public().(ed25519.PublicKey)}})
	dials, _ := LoadRunnerDialStore("")
	rec, _ := dials.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:18693-*"}, Key: kb.Public().(ed25519.PublicKey)})
	f := newOriginFixtureWith(t, ctx, "18692", "18693", ka, Config{RunnerKeys: keys, RunnerDials: dials, DataDir: t.TempDir()})
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = string(dialRow(f.s.dials, rec.ID).LastError)
		if strings.Contains(last, "NotEnrolled") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("last error %q, want the gate's NotEnrolled", last)
}

// I2 (final review): a removal whose write fails leaves the record in place,
// in memory as on disk, and says so.
func TestRunnerDialStoreRemoveRestoresOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	st, _ := LoadRunnerDialStore(filepath.Join(dir, "runner-dials.json"))
	rec, err := st.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:1-*"}, Key: dialTestKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := st.Remove(rec.ID); err == nil || errors.Is(err, ErrDialNotFound) {
		t.Fatalf("remove with an unwritable store: %v, want a write error", err)
	}
	if _, ok := st.Get(rec.ID); !ok {
		t.Fatal("the record left memory although the disk still has it")
	}
}

func TestRunnerDialRemoveReportsStoreFailure(t *testing.T) {
	f := newDialVerbFixture(t)
	dir := t.TempDir()
	f.h.RunnerDials, _ = LoadRunnerDialStore(filepath.Join(dir, "runner-dials.json"))
	op := keyVerbConn()
	k, _, _ := ed25519.GenerateKey(nil)
	f.enroll(t, op, k, "")
	rec, _ := f.h.RunnerDials.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:2-*"}, Key: k})
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RunnerDialRemove, RequestId: 5}
	req.SetRunnerDialRemove(protocol.RunnerDialRemoveRequest{Id: rec.ID})
	f.h.Handle(op, encodeTaskControlRequest(t, req))
	resp := lastTaskControlResponse(t, op)
	if r := resp.RunnerDialRemove(); r == nil || r.Status != protocol.RunnerDialStatus_StoreFailed {
		t.Fatalf("remove answer %+v, want store_failed", r)
	}
}

// I1, second half: a record's connection refused because its identity is held
// by a runner on another key also reports why.
func TestDialRecordIdentityHeldByOtherKeyShowsReason(t *testing.T) {
	reg := NewRegistry()
	h := newTestRunnerHandler(reg)
	keyOf := map[string]ed25519.PublicKey{}
	h.AuthOf = func(cid objproto.ConnectionID) ed25519.PublicKey { return keyOf[cid.String()] }
	h.KickConn = func(objproto.ConnectionID) {}
	var refused []string
	first, second := pcid(t, "ws:127.0.0.1:9630-1"), pcid(t, "ws:127.0.0.1:9631-1")
	keyOf[first.String()], keyOf[second.String()] = dialTestKey(1), dialTestKey(2)
	h.TakePendingDialOrigin = func(cid objproto.ConnectionID) *DialOrigin {
		if cid != second {
			return nil
		}
		return &DialOrigin{Link: protocol.RunnerLink_DialRecord, RecordID: 5,
			OnRefused: func(r string) { refused = append(refused, r) }}
	}
	hello := func() []byte {
		msg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
		msg.SetHello(protocol.RunnerHello{Version: 1, RunnerId: runnerIDOf("shared"), MaxTasks: 1})
		return msg.MustAppend(nil)
	}
	h.Handle(&fakeConn{id: first}, hello())
	h.Handle(&fakeConn{id: second}, hello())
	if len(refused) != 1 || !strings.Contains(refused[0], "held by a runner on key") {
		t.Fatalf("OnRefused got %q", refused)
	}
}
