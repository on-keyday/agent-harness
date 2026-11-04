package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// fakeNet stands in for the network under a dialManager: a dial "succeeds" by
// registering a runner entry the way the RunnerHello handler would, including
// displacing a live entry of the same identity, and reports through the
// origin's callbacks.
type fakeNet struct {
	t   *testing.T
	m   *dialManager
	reg *Registry
	key ed25519.PublicKey

	mu       sync.Mutex
	calls    []string // transport:addr of every dial, in order
	fail     func(cid objproto.ConnectionID) error
	identity func(cid objproto.ConnectionID) protocol.RunnerID
	origins  map[objproto.ConnectionID]*DialOrigin
	links    map[objproto.ConnectionID]uint32 // SetLink(dial_record) calls
	sleeps   []time.Duration
}

// pcid parses a literal ConnectionID (tcid hashes its label into a port, so
// it cannot name an address a candidate resolves to).
func pcid(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	cid, err := objproto.ParseConnectionID(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

func runnerIDOf(s string) protocol.RunnerID {
	var id protocol.RunnerID
	copy(id.Id[:], s)
	return id
}

func newFakeNet(t *testing.T, keys *RunnerKeyStore, store *RunnerDialStore, key ed25519.PublicKey) *fakeNet {
	n := &fakeNet{t: t, reg: NewRegistry(), key: key,
		origins: map[objproto.ConnectionID]*DialOrigin{}, links: map[objproto.ConnectionID]uint32{},
		fail:     func(objproto.ConnectionID) error { return nil },
		identity: func(objproto.ConnectionID) protocol.RunnerID { return runnerIDOf("one-runner") },
	}
	n.m = newDialManager(dialManagerConfig{
		Store: store, Keys: keys, Registry: n.reg, Dial: n.dial,
		SetLink: func(cid objproto.ConnectionID, link protocol.RunnerLink, record uint32) {
			n.mu.Lock()
			n.links[cid] = record
			n.mu.Unlock()
		},
		BackoffInitial: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) bool {
			n.mu.Lock()
			n.sleeps = append(n.sleeps, d)
			n.mu.Unlock()
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Millisecond):
				return true
			}
		},
	})
	n.reg.OnRemove = func(id objproto.ConnectionID, _ RunnerEntry) { n.m.onConnRemoved(id) }
	return n
}

func (n *fakeNet) dial(_ context.Context, cid objproto.ConnectionID, o *DialOrigin) error {
	n.mu.Lock()
	n.calls = append(n.calls, cid.Transport+":"+cid.Addr.String())
	fail, identity := n.fail, n.identity
	n.mu.Unlock()
	if err := fail(cid); err != nil {
		return err
	}
	id := identity(cid)
	if old, ok := n.reg.GetByIdentity(id); ok {
		n.drop(old.ID) // identity takeover: the older connection ends
	}
	if _, err := n.reg.Add(&RunnerEntry{ID: cid, Identity: id, Key: n.key, Conn: &fakeConn{id: cid},
		ActiveTasks: map[string]struct{}{}}); err != nil {
		n.t.Errorf("registry add: %v", err)
	}
	n.mu.Lock()
	n.origins[cid] = o
	n.mu.Unlock()
	go o.OnRegistered(id)
	return nil
}

// drop ends a connection the way the server does: out of the registry, then
// the dialer's OnClosed.
func (n *fakeNet) drop(cid objproto.ConnectionID) {
	n.reg.Remove(cid)
	n.mu.Lock()
	o := n.origins[cid]
	delete(n.origins, cid)
	n.mu.Unlock()
	if o != nil && o.OnClosed != nil {
		go o.OnClosed()
	}
}

func (n *fakeNet) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func dialRow(m *dialManager, id uint32) protocol.RunnerDialRow {
	for _, r := range m.States() {
		if r.Id == id {
			return r
		}
	}
	return protocol.RunnerDialRow{}
}

type dialerFixture struct {
	n     *fakeNet
	store *RunnerDialStore
	key   ed25519.PublicKey
	ctx   context.Context
}

func newDialerFixture(t *testing.T) *dialerFixture {
	t.Helper()
	key, _, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: key}})
	store, _ := LoadRunnerDialStore("")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &dialerFixture{n: newFakeNet(t, keys, store, key), store: store, key: key, ctx: ctx}
}

func (f *dialerFixture) add(t *testing.T, cands ...string) RunnerDialRecord {
	t.Helper()
	rec, err := f.store.Add(RunnerDialRecord{Candidates: cands, Key: f.key})
	if err != nil {
		t.Fatal(err)
	}
	f.n.m.Add(rec)
	return rec
}

func TestDialerConnectsAndRedialsAfterDrop(t *testing.T) {
	f := newDialerFixture(t)
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9500-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	first, _ := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
	f.n.drop(first.ID)
	eventually(t, "re-dialed after the drop", func() bool {
		e, ok := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
		return ok && e.ID != first.ID && dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected
	})
}

func TestDialerBackoffResetsAfterSuccess(t *testing.T) {
	f := newDialerFixture(t)
	var mu sync.Mutex
	failures := 3
	f.n.fail = func(objproto.ConnectionID) error {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return errors.New("refused")
		}
		return nil
	}
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9501-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	e, _ := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
	f.n.drop(e.ID)
	eventually(t, "reconnected", func() bool { return f.n.callCount() >= 5 })
	f.n.mu.Lock()
	sleeps := append([]time.Duration(nil), f.n.sleeps...)
	f.n.mu.Unlock()
	want := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 20 * time.Millisecond}
	if len(sleeps) < len(want) || fmt.Sprint(sleeps[:len(want)]) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want prefix %v (doubling to the cap, reset after a success)", sleeps, want)
	}
}

func TestDialerWalksCandidatesInOrder(t *testing.T) {
	f := newDialerFixture(t)
	f.n.fail = func(cid objproto.ConnectionID) error {
		if cid.Addr.Port() == 9502 {
			return errors.New("dead")
		}
		return nil
	}
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9502-*", "ws:127.0.0.1:9503-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	f.n.mu.Lock()
	calls := append([]string(nil), f.n.calls...)
	f.n.mu.Unlock()
	if len(calls) < 2 || calls[0] != "ws:127.0.0.1:9502" || calls[1] != "ws:127.0.0.1:9503" {
		t.Fatalf("calls %v, want 9502 then 9503", calls)
	}
	if r := dialRow(f.n.m, rec.ID); r.Answered != 1 {
		t.Fatalf("answered = %d, want 1", r.Answered)
	}
}

func TestDialerUnresolvableCandidateWaitsThenConnects(t *testing.T) {
	f := newDialerFixture(t)
	var mu sync.Mutex
	resolvable := false
	f.n.m.cfg.Resolve = func(spec string) (objproto.ConnectionID, error) {
		mu.Lock()
		defer mu.Unlock()
		if !resolvable {
			return objproto.ConnectionID{}, fmt.Errorf("candidate %q: no such host", spec)
		}
		return objproto.ParseConnectionID("ws:127.0.0.1:9504-*", objproto.ParseOption_AllowRandomID)
	}
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:runner.example:9504-*")
	eventually(t, "waiting with the resolution error", func() bool {
		r := dialRow(f.n.m, rec.ID)
		return r.DialState == protocol.RunnerDialState_Waiting && strings.Contains(string(r.LastError), "runner.example")
	})
	mu.Lock()
	resolvable = true
	mu.Unlock()
	eventually(t, "connected once it resolves", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
}

func TestDialerAdoptsOneShotConnectionWithoutTouchingTasks(t *testing.T) {
	f := newDialerFixture(t)
	oneShot := pcid(t, "ws:127.0.0.1:9505-77")
	if _, err := f.n.reg.Add(&RunnerEntry{ID: oneShot, Identity: runnerIDOf("one-runner"), Key: f.key,
		Conn: &fakeConn{id: oneShot}, ActiveTasks: map[string]struct{}{"00112233445566778899aabbccddeeff": {}}}); err != nil {
		t.Fatal(err)
	}
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9505-*")
	eventually(t, "adopted", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	if c := f.n.callCount(); c != 0 {
		t.Fatalf("%d dials; adopting must not dial", c)
	}
	f.n.mu.Lock()
	link := f.n.links[oneShot]
	f.n.mu.Unlock()
	if link != rec.ID {
		t.Fatalf("SetLink record = %d, want %d", link, rec.ID)
	}
	if e, ok := f.n.reg.Get(oneShot); !ok || len(e.ActiveTasks) != 1 {
		t.Fatal("the adopted connection or its task was disturbed")
	}
}

func TestSameAddressSecondRecordIsDuplicateWithoutDialing(t *testing.T) {
	f := newDialerFixture(t)
	f.n.m.Start(f.ctx)
	a := f.add(t, "ws:127.0.0.1:9506-*")
	eventually(t, "a connected", func() bool { return dialRow(f.n.m, a.ID).DialState == protocol.RunnerDialState_Connected })
	calls := f.n.callCount()
	b := f.add(t, "ws:127.0.0.1:9506-1") // same address, different text
	eventually(t, "b duplicate", func() bool { return dialRow(f.n.m, b.ID).DialState == protocol.RunnerDialState_Duplicate })
	if r := dialRow(f.n.m, b.ID); r.DuplicateOf != a.ID {
		t.Fatalf("duplicate_of = %d, want %d", r.DuplicateOf, a.ID)
	}
	if f.n.callCount() != calls {
		t.Fatal("the second record dialed a runner the first already holds")
	}
}

func TestRuntimeDuplicateStopsDialing(t *testing.T) {
	f := newDialerFixture(t)
	f.n.m.Start(f.ctx)
	// One runner reachable over two transports: the addresses differ, so the
	// adopt check cannot see it; the identity takeover does.
	a := f.add(t, "ws:127.0.0.1:9507-*")
	eventually(t, "a connected", func() bool { return dialRow(f.n.m, a.ID).DialState == protocol.RunnerDialState_Connected })
	b := f.add(t, "udp:127.0.0.1:9508-*")
	eventually(t, "one of them duplicate", func() bool {
		return dialRow(f.n.m, a.ID).DialState == protocol.RunnerDialState_Duplicate ||
			dialRow(f.n.m, b.ID).DialState == protocol.RunnerDialState_Duplicate
	})
	settled := f.n.callCount()
	time.Sleep(3 * 80 * time.Millisecond) // 3x the backoff cap
	if f.n.callCount() != settled {
		t.Fatalf("still switching: %d dials after settling at %d", f.n.callCount(), settled)
	}
}

func TestTwoSlotsOneKeyAreNotDuplicates(t *testing.T) {
	f := newDialerFixture(t)
	f.n.identity = func(cid objproto.ConnectionID) protocol.RunnerID {
		return runnerIDOf(fmt.Sprintf("slot-%d", cid.Addr.Port()))
	}
	f.n.m.Start(f.ctx)
	a := f.add(t, "ws:127.0.0.1:9509-*")
	b := f.add(t, "ws:127.0.0.1:9510-*")
	eventually(t, "both connected", func() bool {
		return dialRow(f.n.m, a.ID).DialState == protocol.RunnerDialState_Connected &&
			dialRow(f.n.m, b.ID).DialState == protocol.RunnerDialState_Connected
	})
}

func TestLoadedRecordWithUnknownKeyStaysAndReportsIt(t *testing.T) {
	f := newDialerFixture(t)
	stranger, _, _ := ed25519.GenerateKey(nil)
	rec, _ := f.store.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:9511-*"}, Key: stranger})
	f.n.m.Start(f.ctx) // starts every stored record
	eventually(t, "waiting on the key", func() bool {
		r := dialRow(f.n.m, rec.ID)
		return r.DialState == protocol.RunnerDialState_Waiting && string(r.LastError) == "key not enrolled"
	})
	if f.n.callCount() != 0 {
		t.Fatal("dialed for a key that is not enrolled")
	}
	if _, ok := f.store.Get(rec.ID); !ok {
		t.Fatal("the record was deleted")
	}
}

func TestRemoveLeavesConnectionUp(t *testing.T) {
	f := newDialerFixture(t)
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9512-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	e, _ := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
	f.n.m.Stop(rec.ID)
	if f.n.m.running(rec.ID) {
		t.Fatal("the loop still runs after Stop")
	}
	if _, ok := f.n.reg.Get(e.ID); !ok {
		t.Fatal("Stop closed the live connection")
	}
	calls := f.n.callCount()
	f.n.drop(e.ID)
	time.Sleep(100 * time.Millisecond)
	if f.n.callCount() != calls {
		t.Fatal("a stopped record dialed again")
	}
}

func TestRecordsPointingAt(t *testing.T) {
	f := newDialerFixture(t)
	f.n.m.Start(f.ctx)
	rec := f.add(t, "ws:127.0.0.1:9513-*")
	eventually(t, "connected", func() bool { return dialRow(f.n.m, rec.ID).DialState == protocol.RunnerDialState_Connected })
	e, _ := f.n.reg.GetByIdentity(runnerIDOf("one-runner"))
	if got := f.n.m.RecordsPointingAt(e); len(got) != 1 || got[0] != rec.ID {
		t.Fatalf("live connection: %v", got)
	}
	// A restarted runner: new identity, same port, not connected to the record.
	other := RunnerEntry{ID: pcid(t, "ws:127.0.0.1:9513-5"), Identity: runnerIDOf("restarted")}
	if got := f.n.m.RecordsPointingAt(other); len(got) != 1 || got[0] != rec.ID {
		t.Fatalf("by address: %v", got)
	}
	stranger := RunnerEntry{ID: pcid(t, "ws:127.0.0.1:9999-5"), Identity: runnerIDOf("stranger")}
	if got := f.n.m.RecordsPointingAt(stranger); len(got) != 0 {
		t.Fatalf("a runner the record never named: %v", got)
	}
}

// The server loads runner-dials.json and dials it from serve — end to end over
// a real listen-mode runner.
func TestServerDialsStoredRecordAtStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	_, ka, _ := ed25519.GenerateKey(nil)
	keys, _ := LoadRunnerKeyStore("", []RunnerKeyRecord{{Key: ka.Public().(ed25519.PublicKey)}})
	dials, err := LoadRunnerDialStore(dir + "/runner-dials.json")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := dials.Add(RunnerDialRecord{Candidates: []string{"ws:127.0.0.1:18671-*"}, Key: ka.Public().(ed25519.PublicKey)})
	f := newOriginFixtureWith(t, ctx, "18670", "18671", ka, Config{RunnerKeys: keys, RunnerDials: dials, DataDir: dir})
	e := f.waitRunner(t, "origin-runner")
	if info := toRunnerInfo(e); info.Link != protocol.RunnerLink_DialRecord || info.LinkRecord != rec.ID {
		t.Fatalf("link %v record %d, want dial_record %d", info.Link, info.LinkRecord, rec.ID)
	}
	f.s.kickConn(e.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if again, ok := f.s.registry.GetByIdentity(e.Identity); ok && again.ID != e.ID {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the record did not re-dial the runner after its connection was closed")
}
