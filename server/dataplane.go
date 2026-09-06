package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// dataPlaneGrantTTL bounds how long an unredeemed grant stays usable. It does
// NOT bound a transfer: a push longer than this is ordinary, and the connection
// carrying it took its authorization at accept time.
const dataPlaneGrantTTL = 5 * time.Minute

// mintGrant produces the credential the runner will check.
//
// It names the REQUEST the caps check just passed, never a capability. The
// runner must not hold anything it could interpret as policy, so what crosses
// is a TaskControlKind — the enum that already names every client request — and
// a direction for the one kind that needs a sub-discriminator.
func mintGrant(
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	taskID protocol.TaskID,
	ttl time.Duration,
) protocol.DataPlaneGrant {
	g := protocol.DataPlaneGrant{
		TaskId:        taskID,
		ExpiresUnixMs: uint64(time.Now().Add(ttl).UnixMilli()),
		Kind:          kind,
	}
	if _, err := rand.Read(g.GrantId[:]); err != nil {
		// A process that cannot draw randomness would mint guessable grants
		// from here on, which is worse than stopping.
		panic(fmt.Sprintf("data plane: crypto/rand: %v", err))
	}
	if kind == protocol.TaskControlKind_OpenFileTransfer {
		g.SetDirection(dir)
	}
	return g
}

// randomSlotID draws the connection id the forwarded packets will carry.
// Collisions with either end's existing conn id are refused by the runner
// (slot_collision) and by SetProxy, so this only has to avoid the two ids it
// can see.
func randomSlotID(avoid ...uint16) uint16 {
	var b [2]byte
	for i := 0; i < 8; i++ {
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Sprintf("data plane: crypto/rand: %v", err))
		}
		id := binary.BigEndian.Uint16(b[:])
		if id == 0 {
			continue
		}
		clash := false
		for _, a := range avoid {
			if a == id {
				clash = true
				break
			}
		}
		if !clash {
			return id
		}
	}
	// Eight draws all clashing is not a condition worth a retry loop; the
	// caller surfaces this as an internal error.
	return 0
}

// negotiatedMTU is the packet size a client and a runner on the given
// transports must both use, or 0 when they can each keep their own.
//
// Only the server can compute this: it is the one party that sees both ends.
// The smaller of the two is the answer, and it is applied as the maximum as
// well, so neither end's PLPMTUD probes back above what the other can carry.
func negotiatedMTU(clientTransport, runnerTransport string) uint16 {
	if clientTransport == runnerTransport {
		return 0
	}
	ci, _ := peer.MTUForTransport(clientTransport)
	ri, _ := peer.MTUForTransport(runnerTransport)
	if ri < ci {
		ci = ri
	}
	if ci <= 0 || ci > 0xFFFF {
		return 0
	}
	return uint16(ci)
}

// dataPlaneMinPushBytes is the size above which a push is worth routing end to
// end rather than splicing.
//
// The route buys throughput -- 65.6 MB/s against 36.4 on the ladder, about
// 1.8x -- and pays a fixed setup: a server-to-runner authorize round trip that
// blocks the client's request, then a fresh P521 ECDH and a PSK hello between
// client and runner, then a teardown. A few milliseconds on a LAN, more on a
// slow link. Below the size where 1.8x repays that, the route is a pure loss.
//
// 1 MiB is where the saving is unambiguous: at those two rates it is about
// 12 ms, comfortably more than the setup even with a WAN round trip in it.
// REASONED, NOT MEASURED -- the honest way to tune it is netem-lab across a
// size ladder at two RTTs and to find where the two curves cross.
const dataPlaneMinPushBytes = 1 << 20

// dataPlaneWorthIt reports whether a request moves enough bytes to repay the
// route's setup cost.
//
// This is the half the first version was missing: it asked only whether a data
// plane COULD move end to end (nothing on the server reads those bytes) and
// never whether it SHOULD. A `file ls` moves a few hundred bytes and pays two
// extra round trips and a key exchange for them, which is slower than the
// splice it replaced -- reported from the TUI, where it is the common case.
func dataPlaneWorthIt(kind protocol.TaskControlKind, dir protocol.FileTransferDirection, expectedSize uint64) bool {
	switch kind {
	case protocol.TaskControlKind_ListFiles:
		// A listing is a few hundred bytes and the setup dwarfs it.
		return false
	case protocol.TaskControlKind_OpenFileTransfer:
		switch dir {
		case protocol.FileTransferDirection_Delete,
			protocol.FileTransferDirection_DirDelete,
			protocol.FileTransferDirection_Mkdir:
			// No body at all: these are an ack, and there is nothing to carry.
			return false
		case protocol.FileTransferDirection_Push:
			// The only direction whose size is known before the transfer.
			return expectedSize >= dataPlaneMinPushBytes
		case protocol.FileTransferDirection_Pull,
			protocol.FileTransferDirection_DirPull,
			protocol.FileTransferDirection_DirPush:
			// A pull's size is not known until the runner opens the file, and a
			// directory is a tar stream whose size is never sent. Both are
			// bulk by intent -- fetching a file IS the payload -- so they take
			// the route and a small one occasionally pays setup it did not
			// need.
			return true
		default:
			return false
		}
	default:
		// A kind added later opts in deliberately rather than inheriting a
		// yes from a switch that never considered it.
		return false
	}
}

// dataPlaneRoute reports whether a client and a runner can be joined by packet
// forwarding rather than by splicing.
//
// Both transports are allowed. A mixed pair used to be refused because each end
// sizes packets from its OWN transport (peer.MTUForTransport) and forwarding
// re-emits them byte for byte, so a WebSocket end's 16 KB packets were past the
// datagram MTU on the UDP leg. negotiatedMTU removes that: the server picks the
// smaller size and both ends take it.
func dataPlaneRoute(clientCID, runnerCID objproto.ConnectionID) bool {
	return clientCID.Transport != "" && runnerCID.Transport != ""
}

// setupDataPlane mints a grant, pushes it to the runner, and installs the
// forwarding entry that carries the client's packets to it.
//
// Order matters: the runner must be holding the grant before any packet can
// arrive redeeming it, and the forwarding entry must exist before the client is
// told to dial. A failure at either step leaves nothing behind that a later
// request could trip over — an unredeemed grant expires, and no proxy entry is
// installed unless the runner acknowledged.
func (s *Server) setupDataPlane(
	ctx context.Context,
	ep objproto.Endpoint,
	clientCID objproto.ConnectionID,
	entry *RunnerEntry,
	grant protocol.DataPlaneGrant,
) (uint16, error) {
	if ep == nil {
		return 0, fmt.Errorf("data plane: no endpoint")
	}
	if entry == nil || entry.Conn == nil {
		return 0, fmt.Errorf("data plane: runner offline")
	}
	runnerCID := entry.Conn.ConnectionID()
	slot := randomSlotID(clientCID.ID, runnerCID.ID)
	if slot == 0 {
		return 0, fmt.Errorf("data plane: could not draw a slot id")
	}

	req := protocol.AuthorizeDataPlaneRequest{
		SlotId: slot,
		Mtu:    negotiatedMTU(clientCID.Transport, runnerCID.Transport),
		Grant:  grant,
	}
	// punch_target stays absent: this route forwards, and the runner's punch
	// handler is here for the direct route that does not exist yet.
	resp, err := s.sendAuthorizeDataPlaneRequest(ctx, entry, req)
	if err != nil {
		return 0, fmt.Errorf("data plane: authorize: %w", err)
	}
	if resp.Status != protocol.AuthorizeDataPlaneStatus_Ok {
		return 0, fmt.Errorf("data plane: runner refused: %v", resp.Status)
	}

	owned := objproto.NewConnectionID(clientCID.Transport, clientCID.Addr, slot)
	allocate := objproto.NewConnectionID(runnerCID.Transport, runnerCID.Addr, slot)
	if err := ep.SetProxy(owned, allocate); err != nil {
		// The grant is already at the runner; it expires on its own, and the
		// client is answered with an error rather than a slot it cannot use.
		s.sendRevokeDataPlaneRequest(entry, grant.GrantId)
		return 0, fmt.Errorf("data plane: SetProxy(%v, %v): %w", owned, allocate, err)
	}
	s.rememberGrant(hex.EncodeToString(grant.TaskId.Id[:]), issuedGrant{
		grantID:      grant.GrantId,
		entry:        entry,
		clientCID:    clientCID,
		slot:         slot,
		issuedUnixMs: uint64(time.Now().UnixMilli()),
	})
	return slot, nil
}

// issuedGrant is what the server must remember to withdraw a grant later: who
// to tell, and which forwarding entry to remove.
type issuedGrant struct {
	grantID   [16]byte
	entry     *RunnerEntry
	clientCID objproto.ConnectionID
	slot      uint16
	// issuedUnixMs is when this record was made, for the age floor. NOT the
	// grant's expiry: a transfer may legitimately outlive that, and pruning on
	// it would drop the record of one still running.
	issuedUnixMs uint64
}

// dataPlaneRecordMaxAge bounds the bookkeeping if a runner vanishes mid
// transfer and its finish message never arrives. It is deliberately far longer
// than any transfer: pruning at the GRANT's expiry instead would take the
// record of a transfer still running, and with it the server's ability to
// revoke that transfer -- which is the one thing a narrowing caps change
// promises. finishDataPlane is what normally removes an entry; this is only
// the floor under a runner that died without saying so.
const dataPlaneRecordMaxAge = time.Hour

// rememberGrant records an issued grant against its task, and drops records too
// old to belong to anything still running while it already holds the lock.
//
// Some pruning has to happen here: finishDataPlane covers the normal case, but
// a runner that dies mid transfer sends nothing, and without a floor the map
// grows by one per file operation for the life of the process, pinning a
// RunnerEntry with each.
func (s *Server) rememberGrant(taskIDHex string, g issuedGrant) {
	cutoff := uint64(time.Now().Add(-dataPlaneRecordMaxAge).UnixMilli())
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	if s.issuedGrants == nil {
		s.issuedGrants = make(map[string][]issuedGrant)
	}
	for task, gs := range s.issuedGrants {
		live := gs[:0]
		for _, e := range gs {
			if e.issuedUnixMs > cutoff {
				live = append(live, e)
			}
		}
		if len(live) == 0 {
			delete(s.issuedGrants, task)
			continue
		}
		s.issuedGrants[task] = live
	}
	s.issuedGrants[taskIDHex] = append(s.issuedGrants[taskIDHex], g)
}

// revokeDataPlaneForTask withdraws every grant issued for a task. All three
// parts of the revocation happen here: the forwarding entry goes, so no further
// packet crosses this process; the runner is told, because a client that can
// reach it without this process would otherwise keep going; and the grant's own
// TTL stays the backstop for a message that never lands.
func (s *Server) revokeDataPlaneForTask(taskIDHex string) int {
	s.grantsMu.Lock()
	grants := s.issuedGrants[taskIDHex]
	delete(s.issuedGrants, taskIDHex)
	s.grantsMu.Unlock()

	ep := s.dataPlaneEndpoint.Load()
	for _, g := range grants {
		if ep != nil {
			owned := objproto.NewConnectionID(g.clientCID.Transport, g.clientCID.Addr, g.slot)
			(*ep).DeleteProxy(owned) //nolint:errcheck
		}
		s.sendRevokeDataPlaneRequest(g.entry, g.grantID)
	}
	return len(grants)
}

// finishDataPlane drops everything this process holds for one grant, on the
// runner's word that the connection carrying it has ended.
//
// This is the primary reclaim path. The expiry pruning in rememberGrant and the
// objproto proxy TTL are the backstop for a runner that dies without saying
// anything, not the mechanism: without this the entry's lifetime is the TTL
// rather than the transfer's.
func (s *Server) finishDataPlane(grantID [16]byte) {
	var found *issuedGrant
	s.grantsMu.Lock()
	for task, gs := range s.issuedGrants {
		for i := range gs {
			if gs[i].grantID == grantID {
				g := gs[i]
				found = &g
				s.issuedGrants[task] = append(gs[:i], gs[i+1:]...)
				if len(s.issuedGrants[task]) == 0 {
					delete(s.issuedGrants, task)
				}
				break
			}
		}
		if found != nil {
			break
		}
	}
	s.grantsMu.Unlock()
	if found == nil {
		return
	}
	if ep := s.dataPlaneEndpoint.Load(); ep != nil {
		owned := objproto.NewConnectionID(found.clientCID.Transport, found.clientCID.Addr, found.slot)
		(*ep).DeleteProxy(owned) //nolint:errcheck
	}
}

// sendAuthorizeDataPlaneRequest pushes a grant to a runner and waits for its
// answer, correlating on the runner's conn id exactly as
// sendEstablishRelayRequest does.
func (s *Server) sendAuthorizeDataPlaneRequest(
	ctx context.Context, entry *RunnerEntry, req protocol.AuthorizeDataPlaneRequest,
) (protocol.AuthorizeDataPlaneResponse, error) {
	if entry == nil || entry.Conn == nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("nil entry / Conn")
	}
	connCID := entry.Conn.ConnectionID()
	respCh := make(chan protocol.AuthorizeDataPlaneResponse, 1)
	s.dpRespChMu.Lock()
	if s.dpRespCh == nil {
		s.dpRespCh = make(map[objproto.ConnectionID]chan protocol.AuthorizeDataPlaneResponse)
	}
	s.dpRespCh[connCID] = respCh
	s.dpRespChMu.Unlock()
	defer func() {
		s.dpRespChMu.Lock()
		if cur, ok := s.dpRespCh[connCID]; ok && cur == respCh {
			delete(s.dpRespCh, connCID)
		}
		s.dpRespChMu.Unlock()
	}()

	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_AuthorizeDataPlane
	rr.SetAuthorizeDataPlane(req)
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("encode AuthorizeDataPlane: %w", err)
	}
	if _, _, err := entry.Conn.SendMessage(payload); err != nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("send AuthorizeDataPlane: %w", err)
	}
	select {
	case <-ctx.Done():
		return protocol.AuthorizeDataPlaneResponse{}, ctx.Err()
	case resp := <-respCh:
		return resp, nil
	}
}

// sendRevokeDataPlaneRequest is fire-and-forget. A revoke is idempotent on the
// runner and the grant's TTL is the backstop if it never lands, so there is
// nothing useful to do with a delivery failure here.
func (s *Server) sendRevokeDataPlaneRequest(entry *RunnerEntry, grantID [16]byte) {
	if entry == nil || entry.Conn == nil {
		return
	}
	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_RevokeDataPlane
	rr.SetRevokeDataPlane(protocol.RevokeDataPlaneRequest{GrantId: grantID})
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return
	}
	entry.Conn.SendMessage(payload) //nolint:errcheck
}

// deliverAuthorizeDataPlaneResponse routes a runner's answer back to the
// goroutine waiting on it, the mirror of deliverEstablishRelayResponse.
func (s *Server) deliverAuthorizeDataPlaneResponse(conn ConnHandle, resp protocol.AuthorizeDataPlaneResponse) {
	cid := conn.ConnectionID()
	s.dpRespChMu.Lock()
	ch, ok := s.dpRespCh[cid]
	s.dpRespChMu.Unlock()
	if !ok {
		s.cfg.Logger.Warn("server: AuthorizeDataPlaneResponse without waiter",
			"runner", cid.String(), "status", resp.Status)
		return
	}
	select {
	case ch <- resp:
	default:
	}
}
