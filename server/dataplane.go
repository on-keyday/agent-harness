package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
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

// dataPlaneRoute reports whether a client and a runner can be joined by packet
// forwarding rather than by splicing.
//
// The rule is transport equality. Forwarding rewrites the connection id of a
// packet and sends it back out; sending one that arrived over WebSocket out
// over UDP (or the reverse) has never been exercised on this transport, and a
// file transfer is not the place to find out. Same-transport pairs are what the
// throughput ladder priced and what the relay paths already in this repo use.
func dataPlaneRoute(clientCID, runnerCID objproto.ConnectionID) bool {
	return clientCID.Transport == runnerCID.Transport && clientCID.Transport != ""
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

	req := protocol.AuthorizeDataPlaneRequest{SlotId: slot, Grant: grant}
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
	return slot, nil
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
