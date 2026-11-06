package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
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
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_RunnerDialRemove, RequestId: 5}}
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

// The dial manager starts only once the WAL is replayed (spec §1.2): a runner a
// record registers before that is told none of its held tasks survive. A tiny
// WAL replays faster than any dial completes, so an end-to-end test cannot see
// the order; this checks it at the moment Start runs, wherever it is called.
func TestDialManagerStartsAfterWALReplay(t *testing.T) {
	dir := t.TempDir()
	ctx1, cancel1 := context.WithCancel(context.Background())
	s1 := New(Config{Addr: "127.0.0.1:18694", DataDir: dir})
	done1 := make(chan error, 1)
	go func() { done1 <- s1.Run(ctx1) }()
	time.Sleep(300 * time.Millisecond)
	id := s1.Tasks().Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_None}, "")
	cancel1()
	<-done1

	s2 := New(Config{Addr: "127.0.0.1:18694", DataDir: dir})
	seen := make(chan bool, 1)
	s2.dials.cfg.OnStart = func() {
		_, ok := s2.Tasks().Get(id)
		seen <- ok
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = s2.Run(ctx2) }()
	select {
	case ok := <-seen:
		if !ok {
			t.Fatal("dial records started before the WAL was replayed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial manager never started")
	}
}

// M2: once its record is removed, a live connection no longer claims to be
// that record's — D21's link answers "does kicking this remove a record".
func TestDialRecordRemoveRelinksRealConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ka, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: ka.Public().(ed25519.PublicKey)}})
	dials, _ := LoadRunnerDialStore("")
	rec, _ := dials.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:18696-*"}, Key: ka.Public().(ed25519.PublicKey)})
	f := newOriginFixtureWith(t, ctx, "18695", "18696", ka, Config{RunnerKeys: keys, RunnerDials: dials, DataDir: t.TempDir()})
	e := f.waitRunner(t, "origin-runner")
	if e.Link != protocol.RunnerLink_DialRecord || e.LinkRecord != rec.ID {
		t.Fatalf("before: link %v record %d", e.Link, e.LinkRecord)
	}
	dials.Remove(rec.ID)
	f.s.dials.Stop(rec.ID)
	after, ok := f.s.registry.Get(e.ID)
	if !ok || after.Link != protocol.RunnerLink_DialedOut || after.LinkRecord != 0 {
		t.Fatalf("after removal: link %v record %d (ok=%v), want dialed_out 0", after.Link, after.LinkRecord, ok)
	}
}

// M1: a connection whose record was removed while it was still registering
// does not come up claiming the removed record.
func TestDialRecordStoppedMidDialRegistersUnlinked(t *testing.T) {
	key, _, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: key}})
	store, _ := LoadRunnerDialStore("")
	reg := NewRegistry()
	dialed := make(chan struct{})
	release := make(chan struct{})
	var m *dialManager
	m = newDialManager(dialManagerConfig{Store: store, Keys: keys, Registry: reg, SetLink: reg.SetLink,
		Dial: func(_ context.Context, cid objproto.ConnectionID, o *DialOrigin) error {
			go func() { // the hello handler, late
				<-release
				reg.Add(&RunnerEntry{ID: cid, Identity: runnerIDOf("late"), Key: key, Conn: &fakeConn{id: cid},
					ActiveTasks: map[string]struct{}{}, Link: o.Link, LinkRecord: o.RecordID})
				o.OnRegistered(runnerIDOf("late"))
			}()
			close(dialed)
			return nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	rec, _ := store.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:9640-*"}, Key: key})
	m.Add(rec)
	<-dialed
	store.Remove(rec.ID)
	m.Stop(rec.ID)
	close(release)
	eventually(t, "the late registration is relinked", func() bool {
		e, ok := reg.GetByIdentity(runnerIDOf("late"))
		return ok && e.Link == protocol.RunnerLink_DialedOut && e.LinkRecord == 0
	})
}

// M4: kick finds a record by address from the addresses the record's own loop
// last resolved — it does no name resolution of its own.
func TestRecordsPointingAtDoesNotResolve(t *testing.T) {
	f := newDialerFixture(t)
	var mu sync.Mutex
	resolves := 0
	f.n.m.cfg.Resolve = func(spec string) (objproto.ConnectionID, error) {
		mu.Lock()
		resolves++
		mu.Unlock()
		return objproto.ParseConnectionID(spec, objproto.ParseOption_AllowRandomID)
	}
	f.n.fail = func(objproto.ConnectionID) error { return errTestRefused }
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9641-*")
	eventually(t, "waiting", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Waiting })
	mu.Lock()
	before := resolves
	mu.Unlock()
	got := f.n.m.RecordsPointingAt(RunnerEntry{ID: pcid(t, "ws:127.0.0.1:9641-9"), Identity: runnerIDOf("restarted")})
	mu.Lock()
	after := resolves
	mu.Unlock()
	if len(got) != 1 || got[0] != rec.ID {
		t.Fatalf("by address: %v", got)
	}
	if after != before {
		t.Fatalf("RecordsPointingAt resolved %d name(s)", after-before)
	}
}
