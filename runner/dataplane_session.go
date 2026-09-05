package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// punchInterval is how often a runner re-probes a peer it is opening a path
// toward. 500ms is what probe 3 in the design's companion doc used, where 240
// probes over two minutes all landed.
const punchInterval = 500 * time.Millisecond

// ensureGrants lazily builds the store, so a Session assembled by an older test
// or by a code path that never authorizes a data plane does not have to know
// about it.
func (s *Session) ensureGrants() *grantStore {
	if s.Grants == nil {
		s.Grants = newGrantStore()
	}
	return s.Grants
}

// handleAuthorizeDataPlane records a grant the server has authorized and, when
// a punch target is present, opens the return path toward it.
//
// Shaped after handleEstablishRelay, which is the sibling: same
// (ctx, logger, state, endpoint, request, sendResponse) signature, and the same
// reason for the slot check — a slot id equal to the server conn's id would
// make an inbound dial at that id resolve to the existing server conn rather
// than produce a new one.
//
// The punch only runs when the server asked for it, and this version's server
// never does (it writes transport_len == 0).
func handleAuthorizeDataPlane(
	ctx context.Context,
	logger *slog.Logger,
	sess *Session,
	ep objproto.Endpoint,
	req protocol.AuthorizeDataPlaneRequest,
	sendResponse func(protocol.AuthorizeDataPlaneResponse) error,
) {
	respond := func(st protocol.AuthorizeDataPlaneStatus) {
		if err := sendResponse(protocol.AuthorizeDataPlaneResponse{Status: st}); err != nil && logger != nil {
			logger.Warn("data plane: send authorize response failed", "err", err, "status", st)
		}
	}
	if req.SlotId == sess.ServerCID.ID {
		respond(protocol.AuthorizeDataPlaneStatus_SlotCollision)
		return
	}
	if st := sess.ensureGrants().Insert(req.Grant, req.SlotId); st != protocol.AuthorizeDataPlaneStatus_Ok {
		respond(st)
		return
	}
	respond(protocol.AuthorizeDataPlaneStatus_Ok)

	if req.PunchTarget.TransportLen == 0 || ep == nil {
		return
	}
	// Bounded by the grant's own expiry: a path opened for a grant nobody can
	// redeem any more is just traffic.
	deadline := time.UnixMilli(int64(req.Grant.ExpiresUnixMs))
	go func() {
		punchCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		n := punchToward(punchCtx, ep, req.PunchTarget, punchInterval)
		if logger != nil {
			logger.Info("data plane: punch finished", "probes", n,
				"target", protocol.RunnerIDToConnID(req.PunchTarget).String())
		}
	}()
}

// handleRevokeDataPlane drops a grant and tears down whatever is redeeming it.
//
// Idempotent on purpose: a revoke is a message, and a message can arrive twice
// or not at all. The answer carries how many connections went so the server can
// tell "closed one" from "there was nothing to close", and the grant's TTL is
// the backstop for the case where this never arrives.
func handleRevokeDataPlane(
	sess *Session,
	req protocol.RevokeDataPlaneRequest,
	sendResponse func(protocol.RevokeDataPlaneResponse) error,
) {
	closed := sess.ensureGrants().Revoke(req.GrantId)
	_ = sendResponse(protocol.RevokeDataPlaneResponse{Closed: closed})
}
