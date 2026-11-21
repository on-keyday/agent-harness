package server

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func credKey(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return k
}

func mustCreateCredential(t *testing.T, s *TaskStore, creator protocol.TaskID, caps protocol.Capability, pub [32]byte, expiresAt time.Time) string {
	t.Helper()
	id, err := s.CreateCredential(creator, protocol.ClientKind_Cli, authority.Grant{Caps: caps}, pub, expiresAt, "nightly")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return id
}

func wantCredential(t *testing.T, s *TaskStore, id string, now time.Time, pub [32]byte, state CredentialState) {
	t.Helper()
	got, st, ok := s.CredentialLive(id, now)
	if !ok {
		t.Fatalf("CredentialLive(%s): ok=false", id[:8])
	}
	if got != pub {
		t.Errorf("CredentialLive(%s) key = %x…, want %x…", id[:8], got[:4], pub[:4])
	}
	if st != state {
		t.Errorf("CredentialLive(%s) state = %v, want %v", id[:8], st, state)
	}
}

func TestCreateCredentialIsActiveAndNotQueued(t *testing.T) {
	s := NewTaskStore()
	var created []string
	s.OnCreate = func(id string) { created = append(created, id) }
	woke := 0
	s.OnDeadlineSet = func() { woke++ }

	id := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(1), time.Now().Add(time.Hour))

	e, ok := s.Get(id)
	if !ok {
		t.Fatal("credential not in the store")
	}
	if e.Status != protocol.TaskStatus_Active || e.Kind != protocol.TaskKind_Credential {
		t.Fatalf("status/kind = %v/%v, want active/credential", e.Status, e.Kind)
	}
	if e.StartedAt == nil {
		t.Error("StartedAt unset on a credential")
	}
	if e.RepoPath != "" || e.Prompt != "" || !e.AssignedTo.IsZero() {
		t.Errorf("credential carries repo %q prompt %q runner %v", e.RepoPath, e.Prompt, e.AssignedTo)
	}
	if len(created) != 1 || created[0] != id {
		t.Errorf("OnCreate fired for %v, want [%s]", created, id)
	}
	if woke == 0 {
		t.Error("the deadline watcher was not woken for the new expiry")
	}
	for _, q := range s.QueuedTasks() {
		if q.ID == id {
			t.Fatal("a credential is listed as Queued")
		}
	}

	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID: tcid("r1"), Hostname: "h1", AllowedRoots: []string{"/"}, MaxTasks: 4,
		ActiveTasks: map[string]struct{}{}, ConnectedAt: time.Unix(1, 0), LastTaskActivity: time.Unix(1, 0),
		Conn: &fakeConn{},
	})
	var started []string
	NewScheduler(reg, s, func(_ objproto.ConnectionID, tid string) error {
		started = append(started, tid)
		return nil
	}).Tick()
	if len(started) != 0 {
		t.Fatalf("Tick started %v", started)
	}
	if e, _ := s.Get(id); e.Status != protocol.TaskStatus_Active {
		t.Fatalf("status after Tick = %v, want active", e.Status)
	}
}

func TestCreateCredentialMasksIssueAndScheduleEdit(t *testing.T) {
	s := NewTaskStore()
	id, err := s.CreateCredential(protocol.TaskID{}, protocol.ClientKind_Cli,
		authority.Grant{Caps: protocol.Capability_All, DelegateCaps: protocol.Capability_All},
		credKey(1), time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(id)
	for _, c := range []protocol.Capability{e.Capabilities, e.DelegateCaps} {
		if c&CredentialMaskedCaps != 0 {
			t.Errorf("caps %v hold a masked bit", c)
		}
		if c&protocol.Capability_Spawn == 0 {
			t.Errorf("caps %v lost spawn", c)
		}
	}
}

// D14: set_caps on a credential task can never add credential_issue or
// schedule_edit — on the target, and on a credential reached by a cascade.
// Named on the target it is refused (masked_cap), not silently masked: a
// typed grant either takes effect or errors.
func TestSetCapsCannotAddMaskedBitsToCredential(t *testing.T) {
	h, p, _, _, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9910-1")}
	cred := mustCreateCredential(t, h.Tasks, hexToTaskID(t, p), protocol.Capability_Spawn, credKey(1), time.Now().Add(time.Hour))

	direct := protocol.SetCapsRequest{TaskId: hexToTaskID(t, cred), Caps: protocol.Capability_All, DelegateCaps: protocol.Capability_All}
	direct.SetCapsPresent(true)
	direct.SetDelegateCapsPresent(true)
	if got := sendSetCaps(t, h, opConn, direct); got.Status != protocol.SetCapsStatus_MaskedCap {
		t.Fatalf("direct: status = %v, want masked_cap", got.Status)
	}
	e, _ := h.Tasks.Get(cred)
	if e.Capabilities != protocol.Capability_Spawn || e.DelegateCaps&CredentialMaskedCaps != 0 {
		t.Fatalf("direct: a refused set_caps left caps %v delegate %v", e.Capabilities, e.DelegateCaps)
	}

	cascade := protocol.SetCapsRequest{TaskId: hexToTaskID(t, p), Caps: protocol.Capability_All, DelegateCaps: protocol.Capability_All}
	cascade.SetCapsPresent(true)
	cascade.SetDelegateCapsPresent(true)
	cascade.SetCascade(true)
	if got := sendSetCaps(t, h, opConn, cascade); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("cascade: status = %v, want ok", got.Status)
	}
	e, _ = h.Tasks.Get(cred)
	if e.Capabilities&CredentialMaskedCaps != 0 || e.DelegateCaps&CredentialMaskedCaps != 0 {
		t.Fatalf("cascade: caps %v delegate %v hold a masked bit", e.Capabilities, e.DelegateCaps)
	}

	// The store is the single writer for both paths; a cascade write naming
	// every bit is masked there too.
	after, ok := h.Tasks.SetCaps(cred, GrantUpdate{Caps: true, DelegateCaps: true,
		To: authority.Grant{Caps: protocol.Capability_All, DelegateCaps: protocol.Capability_All}},
		AuthorityWrite{Source: protocol.AuthoritySource_Cascade, Actor: hexToTaskID(t, p)})
	if !ok || after.Capabilities&CredentialMaskedCaps != 0 || after.DelegateCaps&CredentialMaskedCaps != 0 {
		t.Fatalf("store cascade write: ok=%v caps %v delegate %v", ok, after.Capabilities, after.DelegateCaps)
	}
}

func TestCredentialExpiresThroughDeadlineScan(t *testing.T) {
	s := NewTaskStore()
	h := &TaskHandler{Tasks: s}

	expiresAt := time.Now().Add(time.Hour)
	live := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(1), expiresAt)
	due, next := s.BackgroundDeadlines(time.Now().UnixNano())
	if len(due) != 0 {
		t.Fatalf("due before expiry: %v", due)
	}
	if next != expiresAt.UnixNano() {
		t.Fatalf("next = %d, want the expiry %d", next, expiresAt.UnixNano())
	}
	due, _ = s.BackgroundDeadlines(expiresAt.UnixNano())
	if len(due) != 1 || due[0].ID != live || due[0].Reason != protocol.EndReason_Expired {
		t.Fatalf("due at expiry = %v, want one expired hit for %s", due, live[:8])
	}

	// One already past, ended through the watcher's end callback.
	past := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(2), time.Now().Add(-time.Second))
	due, _ = s.BackgroundDeadlines(time.Now().UnixNano())
	if len(due) != 1 || due[0].ID != past {
		t.Fatalf("due = %v, want the past credential", due)
	}
	h.endDeadlineHit(due[0])
	e, _ := s.Get(past)
	if e.Status != protocol.TaskStatus_Cancelled || e.EndReason != protocol.EndReason_Expired {
		t.Fatalf("after end: status %v reason %v, want cancelled/expired", e.Status, e.EndReason)
	}
	wantCredential(t, s, past, time.Now(), credKey(2), CredentialExpired)
	if due, _ := s.BackgroundDeadlines(time.Now().UnixNano()); len(due) != 0 {
		t.Fatalf("an ended credential is still due: %v", due)
	}

	// A hit read before a re-key moved the expiry ends nothing.
	racer := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(3), time.Now().Add(-time.Second))
	due, _ = s.BackgroundDeadlines(time.Now().UnixNano())
	if len(due) != 1 || due[0].ID != racer {
		t.Fatalf("due = %v, want the racer", due)
	}
	if _, err := s.RekeyCredential(racer, credKey(4), time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != nil {
		t.Fatal(err)
	}
	h.endDeadlineHit(due[0])
	if e, _ := s.Get(racer); e.Status != protocol.TaskStatus_Active {
		t.Fatalf("a stale expiry hit ended a re-keyed credential: %v %v", e.Status, e.EndReason)
	}
}

func TestCredentialSurvivesReplay(t *testing.T) {
	s, path := storeWithWAL(t)
	expiresAt := time.Now().Add(time.Hour)
	id := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn|protocol.Capability_ScheduleRun, credKey(7), expiresAt)
	s.TouchCredential(id, time.Now())
	if e, _ := s.Get(id); e.LastUsedAt.IsZero() {
		t.Fatal("TouchCredential did not set LastUsedAt")
	}

	r := replayInto(t, path)
	e, ok := r.Get(id)
	if !ok {
		t.Fatal("credential lost on replay")
	}
	if e.Status != protocol.TaskStatus_Active || e.Kind != protocol.TaskKind_Credential {
		t.Fatalf("replayed status/kind = %v/%v, want active/credential", e.Status, e.Kind)
	}
	if e.CredentialPubKey != credKey(7) || !e.ExpiresAt.Equal(expiresAt) || e.CredentialName != "nightly" {
		t.Errorf("replayed key %x… expiry %v name %q", e.CredentialPubKey[:4], e.ExpiresAt, e.CredentialName)
	}
	if !e.LastUsedAt.IsZero() {
		t.Errorf("LastUsedAt = %v after replay, want zero (memory only, D20)", e.LastUsedAt)
	}
	if e.StartedAt == nil {
		t.Error("StartedAt lost on replay")
	}
	if e.Capabilities != protocol.Capability_Spawn|protocol.Capability_ScheduleRun {
		t.Errorf("replayed caps = %v", e.Capabilities)
	}
	wantCredential(t, r, id, time.Now(), credKey(7), CredentialLive)
}

// D8: a credential that expired while the server was down is ended once at
// start, through the cancel path, so the end is itself recorded.
func TestCredentialExpiredDuringDowntimeEndsAtStart(t *testing.T) {
	s, path := storeWithWAL(t)
	id := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(1), time.Now().Add(-time.Minute))

	// The restart: replay, attach a WAL, run the watcher's startup pass.
	r := replayInto(t, path)
	if e, _ := r.Get(id); e.Status != protocol.TaskStatus_Active {
		t.Fatalf("replay alone moved the credential to %v", e.Status)
	}
	wal2 := filepath.Join(t.TempDir(), "after.log")
	w, err := OpenWAL(wal2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck
	r.SetWAL(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &TaskHandler{Tasks: r}
	runDeadlineWatcher(ctx, r, make(chan struct{}), h.endDeadlineHit)

	e, _ := r.Get(id)
	if e.Status != protocol.TaskStatus_Cancelled || e.EndReason != protocol.EndReason_Expired {
		t.Fatalf("after start: status %v reason %v, want cancelled/expired", e.Status, e.EndReason)
	}
	events, _, err := ReadWAL(wal2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "task_cancelled" || events[0].TaskID != id ||
		protocol.EndReason(events[0].EndReason) != protocol.EndReason_Expired {
		t.Fatalf("start-time end recorded %+v, want one task_cancelled/expired", events)
	}
}

func TestRekeyRevivesRevokedAndExpired(t *testing.T) {
	s, path := storeWithWAL(t)
	now := time.Now()

	revoked := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(1), now.Add(time.Hour))
	s.Cancel(revoked, protocol.EndReason_Cancelled, protocol.TaskID{})
	wantCredential(t, s, revoked, now, credKey(1), CredentialRevoked)

	expired := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn|protocol.Capability_ScheduleRun, credKey(2), now.Add(-time.Second))
	wantCredential(t, s, expired, now, credKey(2), CredentialExpired) // active, past its expiry
	if !s.ExpireCredential(expired, now) {
		t.Fatal("ExpireCredential did not end a past-expiry credential")
	}
	wantCredential(t, s, expired, now, credKey(2), CredentialExpired) // cancelled, reason expired

	newExpiry := now.Add(2 * time.Hour)
	for _, id := range []string{revoked, expired} {
		var u *GrantUpdate
		if id == expired {
			// Narrowing rides the re-key; a masked bit in it is dropped.
			u = &GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_ScheduleRun | protocol.Capability_CredentialIssue}}
		}
		e, err := s.RekeyCredential(id, credKey(9), newExpiry, u, AuthorityWrite{Source: protocol.AuthoritySource_Resume})
		if err != nil {
			t.Fatalf("rekey %s: %v", id[:8], err)
		}
		if e.Status != protocol.TaskStatus_Active || e.EndedAt != nil || e.EndReason != protocol.EndReason_None || e.CancelledBy != (protocol.TaskID{}) {
			t.Errorf("rekey %s: status %v ended %v reason %v by %x", id[:8], e.Status, e.EndedAt, e.EndReason, e.CancelledBy.Id[:4])
		}
		wantCredential(t, s, id, now, credKey(9), CredentialLive)
	}
	if e, _ := s.Get(expired); e.Capabilities != protocol.Capability_ScheduleRun {
		t.Errorf("narrowed caps = %v, want schedule_run", e.Capabilities)
	}

	r := replayInto(t, path)
	for _, id := range []string{revoked, expired} {
		wantCredential(t, r, id, now, credKey(9), CredentialLive)
		if e, _ := r.Get(id); !e.ExpiresAt.Equal(newExpiry) || e.EndedAt != nil {
			t.Errorf("replayed %s: expiry %v ended %v", id[:8], e.ExpiresAt, e.EndedAt)
		}
	}
	if e, _ := r.Get(expired); e.Capabilities != protocol.Capability_ScheduleRun {
		t.Errorf("replayed narrowed caps = %v, want schedule_run", e.Capabilities)
	}
}

func TestRekeyReplacesKeyOnLiveCredential(t *testing.T) {
	s := NewTaskStore()
	now := time.Now()
	id := mustCreateCredential(t, s, protocol.TaskID{}, protocol.Capability_Spawn, credKey(1), now.Add(time.Hour))
	woke := 0
	s.OnDeadlineSet = func() { woke++ }
	if _, err := s.RekeyCredential(id, credKey(2), now.Add(3*time.Hour), nil, AuthorityWrite{}); err != nil {
		t.Fatal(err)
	}
	if pub, _, _ := s.CredentialLive(id, now); pub == credKey(1) {
		t.Fatal("the replaced key is still returned")
	}
	wantCredential(t, s, id, now, credKey(2), CredentialLive)
	// The old expiry no longer ends it.
	if due, _ := s.BackgroundDeadlines(now.Add(2 * time.Hour).UnixNano()); len(due) != 0 {
		t.Fatalf("due at the old expiry: %v", due)
	}
	if woke == 0 {
		t.Error("the deadline watcher was not woken for the new expiry")
	}
}

func TestRekeyRefusesNonCredential(t *testing.T) {
	s := NewTaskStore()
	plain := createTask(s, protocol.TaskID{})
	if _, err := s.RekeyCredential(plain, credKey(1), time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != ErrNotCredential {
		t.Fatalf("rekey of a session task: err = %v, want ErrNotCredential", err)
	}
	missing := newTaskID()
	if _, err := s.RekeyCredential(missing, credKey(1), time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != ErrNoSuchTask {
		t.Fatalf("rekey of a missing task: err = %v, want ErrNoSuchTask", err)
	}
	for _, id := range []string{plain, missing} {
		if _, _, ok := s.CredentialLive(id, time.Now()); ok {
			t.Errorf("CredentialLive(%s) ok=true for a non-credential", id[:8])
		}
	}
}

// The all-zero key is a small-order point that ed25519.Verify accepts forged
// signatures under, so a record whose key does not decode must not leave a
// usable credential: CredentialLive answers it as no credential at all.
func TestMalformedWALKeyIsNoCredential(t *testing.T) {
	expires := time.Now().Add(time.Hour).UnixNano()
	created := WALEvent{Type: "task_created", TaskID: newTaskID(), Kind: uint8(protocol.TaskKind_Credential),
		Capabilities: uint64(protocol.Capability_Spawn), CredentialPubKey: "zz01", ExpiresAtNs: expires, Ts: time.Now().UnixNano()}
	good := WALEvent{Type: "task_created", TaskID: newTaskID(), Kind: uint8(protocol.TaskKind_Credential),
		Capabilities: uint64(protocol.Capability_Spawn), ExpiresAtNs: expires, Ts: time.Now().UnixNano()}
	k := credKey(5)
	good.CredentialPubKey = hexKey(k)
	rekeyed := WALEvent{Type: "task_rekeyed", TaskID: good.TaskID, CredentialPubKey: hexKey(k)[:62], ExpiresAtNs: expires}

	s := NewTaskStore()
	s.ReplayEvents([]WALEvent{created, good})
	if _, _, ok := s.CredentialLive(created.TaskID, time.Now()); ok {
		t.Error("task_created with a malformed key replayed as a usable credential")
	}
	wantCredential(t, s, good.TaskID, time.Now(), k, CredentialLive)

	s = NewTaskStore()
	s.ReplayEvents([]WALEvent{good, rekeyed})
	if _, _, ok := s.CredentialLive(good.TaskID, time.Now()); ok {
		t.Error("task_rekeyed with a malformed key replayed as a usable credential")
	}
}

func hexKey(k [32]byte) string { return fmt.Sprintf("%x", k[:]) }
