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
		grantID:   grant.GrantId,
		entry:     entry,
		clientCID: clientCID,
		slot:      slot,
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
}

// rememberGrant records an issued grant against its task.
func (s *Server) rememberGrant(taskIDHex string, g issuedGrant) {
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	if s.issuedGrants == nil {
		s.issuedGrants = make(map[string][]issuedGrant)
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
