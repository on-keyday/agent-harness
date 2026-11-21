package server

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Credential tasks (docs/superpowers/specs/2026-11-21-credential-tasks-design.md):
// a TaskEntry with caps and scope but no runner and no process, whose Ed25519
// public key lets an external process connect with that task's authority.

// CredentialMaxTTL bounds the expiry an issue or re-key may set (D7).
const CredentialMaxTTL = 90 * 24 * time.Hour

// CredentialMaskedCaps can never be held by a credential (D14): issuing would
// let a credential mint a successor that outlives its own expiry, and a
// schedule it created would keep firing after it ends. Masked at issue, at
// re-key and by every set_caps write.
const CredentialMaskedCaps = protocol.Capability_CredentialIssue | protocol.Capability_ScheduleEdit

// CredentialState is what a credential's key is good for right now.
// Zero is no state: CredentialLive answers it only with ok=false.
type CredentialState uint8

const (
	// CredentialLive: status active and the expiry ahead.
	CredentialLive CredentialState = iota + 1
	// CredentialExpired: active past its expiry, or ended by the expiry.
	CredentialExpired
	// CredentialRevoked: ended any other way.
	CredentialRevoked
)

func (c CredentialState) String() string {
	switch c {
	case CredentialLive:
		return "live"
	case CredentialExpired:
		return "expired"
	case CredentialRevoked:
		return "revoked"
	default:
		return "none"
	}
}

// Errors RekeyCredential answers.
var (
	ErrNoSuchTask    = errors.New("no such task")
	ErrNotCredential = errors.New("task is not a credential")
)

// credentialFields are the creation-time fields only a credential carries.
type credentialFields struct {
	pub       [32]byte
	expiresAt time.Time
	name      string
}

// apply sets the credential's row at creation. Caller holds s.mu.
func (c *credentialFields) apply(e *TaskEntry, now time.Time) {
	e.Status = protocol.TaskStatus_Active
	started := now
	e.StartedAt = &started
	e.CredentialPubKey = c.pub
	e.ExpiresAt = c.expiresAt
	e.Name = c.name
}

// toWAL adds the credential's fields to its task_created record.
func (c *credentialFields) toWAL(ev *WALEvent, now time.Time) {
	ev.Ts = now.UnixNano() // replay's CreatedAt and StartedAt
	ev.CredentialPubKey = hex.EncodeToString(c.pub[:])
	ev.ExpiresAtNs = expiryToWAL(c.expiresAt)
	ev.Name = c.name
}

func expiryToWAL(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func expiryFromWAL(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// credentialKeyFromWAL decodes a recorded public key. A malformed one is
// logged and replays as the zero key, which CredentialLive treats as no
// credential at all.
func credentialKeyFromWAL(taskID, h string) [32]byte {
	var k [32]byte
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != len(k) {
		slog.Error("WAL: credential public key does not decode; the credential is unusable until re-keyed",
			"task_id", taskID, "hex_len", len(h), "valid_hex", err == nil) // the decoder's error quotes a key byte
		return k
	}
	copy(k[:], raw)
	return k
}

// CreateCredential issues a credential task: a child of creator (zero = the
// operator) holding g minus CredentialMaskedCaps, active from now until
// expiresAt, with no repo, prompt or runner. It is a creation like a spawn, so
// the spawn limits apply.
func (s *TaskStore) CreateCredential(creator protocol.TaskID, origin protocol.ClientKind, g authority.Grant, pub [32]byte, expiresAt time.Time, name string) (string, error) {
	g.Caps &^= CredentialMaskedCaps
	g.DelegateCaps &^= CredentialMaskedCaps
	id := newTaskID()
	cred := &credentialFields{pub: pub, expiresAt: expiresAt, name: name}
	if err := s.createWithinLimits(id, "", "", protocol.TaskKind_Credential, origin, creator, "", protocol.RunnerSelector{}, nil, g, "", RunSpec{}, cred); err != nil {
		return "", err
	}
	s.deadlineSet(true)
	return id, nil
}

// RekeyCredential is a credential's resume (D18): a new key and a new expiry
// under the same id, from any status — live (rotation) or ended (re-issue).
// The old key is gone from the store when this returns. u, when non-nil,
// narrows the authority through the same write SetCaps makes; its record is
// written BEFORE task_rekeyed, so a crash between the two leaves a narrowed
// credential under its old key, never the new key under the wider authority.
func (s *TaskStore) RekeyCredential(id string, pub [32]byte, expiresAt time.Time, u *GrantUpdate, by AuthorityWrite) (TaskEntry, error) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return TaskEntry{}, ErrNoSuchTask
	}
	if e.Kind != protocol.TaskKind_Credential {
		s.mu.Unlock()
		return TaskEntry{}, ErrNotCredential
	}
	if u != nil && u.Any() {
		s.applyGrantUpdateLocked(e, *u, by)
	}
	e.rekey(pub, expiresAt)
	if s.wal != nil {
		ev := WALEvent{Type: "task_rekeyed", TaskID: id, CredentialPubKey: hex.EncodeToString(pub[:]),
			ExpiresAtNs: expiryToWAL(expiresAt), Ts: now.UnixNano()}
		if err := s.wal.Write(ev); err != nil {
			slog.Error("WAL write failed", "op", "task_rekeyed", "task_id", id, "err", err)
		}
	}
	snapshot := *e
	onCreate := s.OnCreate
	s.mu.Unlock()
	// A revived credential changed status with no other event to say so; the
	// creation publisher sends the status the row holds, as Resume reuses it.
	if onCreate != nil {
		onCreate(id)
	}
	s.deadlineSet(true)
	return snapshot, nil
}

// rekey is the state change of a re-key, shared by RekeyCredential and the
// replay of its task_rekeyed record.
func (e *TaskEntry) rekey(pub [32]byte, expiresAt time.Time) {
	e.Status = protocol.TaskStatus_Active
	e.EndedAt = nil
	e.EndReason = protocol.EndReason_None
	e.CancelledBy = protocol.TaskID{}
	e.CredentialPubKey = pub
	e.ExpiresAt = expiresAt
}

// TouchCredential records an accepted use of a credential. Memory only (D20):
// after a restart LastUsedAt is unknown until the next use.
func (s *TaskStore) TouchCredential(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.tasks[id]; ok && e.Kind == protocol.TaskKind_Credential {
		e.LastUsedAt = at
	}
}

// CredentialLive reports a credential's current key and what it is good for
// at now. ok=false for a missing task, one that is not a credential, or one
// whose key is all zero — what a record whose key did not decode replays as.
// The zero key is a small-order Ed25519 point that ed25519.Verify accepts
// forged signatures under, so it must answer as unknown, never as live. The
// expiry is compared here as well as ended by the deadline watcher, so a key
// stops working at its expiry whether or not the watcher has run (D8).
func (s *TaskStore) CredentialLive(id string, now time.Time) (pub [32]byte, state CredentialState, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.tasks[id]
	if !found || e.Kind != protocol.TaskKind_Credential || e.CredentialPubKey == ([32]byte{}) {
		return [32]byte{}, 0, false
	}
	switch {
	case e.Status == protocol.TaskStatus_Active && e.ExpiresAt.After(now):
		state = CredentialLive
	case e.Status == protocol.TaskStatus_Active,
		e.Status == protocol.TaskStatus_Cancelled && e.EndReason == protocol.EndReason_Expired:
		state = CredentialExpired
	default:
		state = CredentialRevoked
	}
	return e.CredentialPubKey, state, true
}

// ExpireCredential ends a credential whose expiry has passed (D11): a cancel
// with reason expired, through the same path as any cancel. It re-checks under
// the lock that the credential is still active and past its expiry at now, so
// an expiry hit read before a re-key moved the expiry ends nothing. Reports
// whether it ended the credential.
func (s *TaskStore) ExpireCredential(id string, now time.Time) bool {
	return s.cancel(id, protocol.EndReason_Expired, protocol.TaskID{}, func(e *TaskEntry) bool {
		return e.Kind == protocol.TaskKind_Credential && e.Status == protocol.TaskStatus_Active && !e.ExpiresAt.After(now)
	})
}
