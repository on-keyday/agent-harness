package runner

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// grantStore holds the data-plane grants the server has pushed to this runner.
//
// It is the mirror of agentboard/registry.go, which does the same job with the
// verifier on the other side: there the server holds the store and the agent
// presents its ticket; here the runner holds it and a client presents a grant.
//
// What it deliberately is NOT is a capability set. An entry names a request the
// server has already authorized, so validation is an equality against the
// request that arrived — no mask arithmetic, no Capability value, no scope.
// The caps model stays written down in exactly one place, on the server.
type grantStore struct {
	mu      sync.Mutex
	entries map[[16]byte]*grantEntry
}

type grantEntry struct {
	grant  protocol.DataPlaneGrant
	slotID uint16
	mtu    uint16
	closer func()
}

func newGrantStore() *grantStore {
	return &grantStore{entries: make(map[[16]byte]*grantEntry)}
}

// Insert records a grant the server has authorized.
//
// A repeat grant_id is refused rather than overwritten. agentboard's Ticket()
// comment records why in the mirror case: a second Register would invalidate
// the credential a live connection is already holding, so the caller that
// needs the existing one must look it up instead of minting over it.
func (s *grantStore) Insert(g protocol.DataPlaneGrant, slotID, mtu uint16) protocol.AuthorizeDataPlaneStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[g.GrantId]; exists {
		return protocol.AuthorizeDataPlaneStatus_DuplicateGrant
	}
	s.entries[g.GrantId] = &grantEntry{grant: g, slotID: slotID, mtu: mtu}
	return protocol.AuthorizeDataPlaneStatus_Ok
}

// MTUForSlot answers the packet size the server negotiated for the connection
// that will arrive at this slot, or 0 when it negotiated none.
//
// Keyed by slot rather than by grant because the accept path has to size the
// connection BEFORE reading the hello that names the grant: the trsf layer is
// built when the conn is wrapped. The slot is on the connection id, which is
// available then.
func (s *grantStore) MTUForSlot(slot uint16) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.slotID == slot {
			return e.mtu
		}
	}
	return 0
}

// Validate answers with the ClientHelloStatus the runner should send back.
//
// The grant_id comparison is constant-time. The rest are equalities against a
// request that has already arrived, so they disclose nothing the caller did not
// supply — and the order matters: an unknown grant and a grant for another task
// must not be distinguishable from each other by timing alone, which the
// constant-time compare on the id half provides.
func (s *grantStore) Validate(
	grantID [16]byte,
	taskID protocol.TaskID,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	now time.Time,
) protocol.ClientHelloStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[grantID]
	if !ok {
		return protocol.ClientHelloStatus_BadTicket
	}
	if subtle.ConstantTimeCompare(e.grant.GrantId[:], grantID[:]) != 1 {
		return protocol.ClientHelloStatus_BadTicket
	}
	if e.grant.TaskId.Id != taskID.Id {
		return protocol.ClientHelloStatus_UnknownTask
	}
	if uint64(now.UnixMilli()) > e.grant.ExpiresUnixMs {
		return protocol.ClientHelloStatus_Expired
	}
	if e.grant.Kind != kind {
		return protocol.ClientHelloStatus_NotPermitted
	}
	if kind == protocol.TaskControlKind_OpenFileTransfer {
		// Direction is a union arm, so the getter returns nil when the kind
		// does not select it. A grant that says open_file_transfer without a
		// direction cannot authorize anything.
		got := e.grant.Direction()
		if got == nil || *got != dir {
			return protocol.ClientHelloStatus_NotPermitted
		}
	}
	return protocol.ClientHelloStatus_Ok
}

// Kind reports which request a grant names, so the accept path knows what to
// read off the connection before it can check the rest. It answers only the
// discriminator, never the grant, so nothing can route around Validate.
func (s *grantStore) Kind(grantID [16]byte) (protocol.TaskControlKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[grantID]
	if !ok {
		return 0, false
	}
	return e.grant.Kind, true
}

// OnClose registers the teardown for the connection redeeming this grant, so a
// revoke can reach work already in flight. Without it a narrowing `caps set`
// would be advisory for anything already transferring, which is the promise
// server/set_caps_handler.go makes and this design must keep.
func (s *grantStore) OnClose(grantID [16]byte, closer func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[grantID]; ok {
		e.closer = closer
	}
}

// Forget drops a grant without closing anything, for the connection that
// redeemed it reporting its own end. Revoke is the other direction -- someone
// else withdrawing authority from a connection still running.
func (s *grantStore) Forget(grantID [16]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, grantID)
}

// Revoke removes a grant and tears down whatever is redeeming it, returning how
// many connections it closed. Revoking an unknown grant is a no-op: a revoke is
// a message, and a message can arrive twice or after the grant has expired.
func (s *grantStore) Revoke(grantID [16]byte) uint32 {
	s.mu.Lock()
	e, ok := s.entries[grantID]
	if ok {
		delete(s.entries, grantID)
	}
	s.mu.Unlock()
	if !ok || e.closer == nil {
		return 0
	}
	e.closer()
	return 1
}

// Sweep drops expired grants that nobody redeemed, and returns how many went.
//
// A REDEEMED entry is left alone however old it is: its connection reports its
// own end (Forget), and dropping it early would take the OnClose hook with it,
// so a transfer running longer than the TTL would stop being reachable by a
// narrowing caps change -- the one thing revocation promises. The TTL bounds
// how long an UNREDEEMED grant stays usable, which is all it was ever for.
func (s *grantStore) Sweep(now time.Time) int {
	cutoff := uint64(now.UnixMilli())
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, e := range s.entries {
		if e.closer != nil {
			continue // redeemed and still running; its own end removes it
		}
		if cutoff > e.grant.ExpiresUnixMs {
			delete(s.entries, id)
			n++
		}
	}
	return n
}
