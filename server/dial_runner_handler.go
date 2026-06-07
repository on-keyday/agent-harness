package server

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ViaRegistrationInfo carries the Phase C registration metadata that must travel
// from HandleWithVia (where target and the resolved proxy entry are in scope) to
// the Hello handler (where RunnerEntry is constructed). Handle (Phase A direct)
// passes nil; HandleWithVia (Phase C) passes a populated struct.
type ViaRegistrationInfo struct {
	// Via is the proxy_runner entry through which the target was registered.
	Via *RunnerEntry
	// ViaDialAddr = protocol.RunnerIDToConnID(target), the addr the proxy_runner
	// actually dialed to reach the target during EstablishRelay. Only Transport +
	// Addr are load-bearing; ID happens to carry admin's UniqueNumber.
	ViaDialAddr objproto.ConnectionID
}

// DialRunnerHandler handles a single TaskControlKind_DialRunner request:
// converts the embedded RunnerID into an objproto.ConnectionID, calls
// objproto.DoECDHHandshake on the server's existing Endpoint, and reports
// the dial outcome.
//
// Design note: we call objproto.DoECDHHandshake directly rather than
// peer.Dial so the raw objproto.Connection is delivered to OnDialed without
// a peer.Conn wrapper. handleConnection builds its own trsf layer on top;
// double-wrapping would create two competing AutoSend/AutoPing goroutines on
// the same underlying connection.
//
// Outbound-Dial connections are delivered via the hsDone channel inside
// objproto (not GetNewActiveConnectionChannel, which is inbound-only).
// OnDialed bridges the gap: the server wires it to
//
//	func(ctx, conn) { go s.handleConnection(ctx, conn) }
//
// so the new connection enters the standard registration path (PSK gate →
// RunnerHello → Registry insert).
type DialRunnerHandler struct {
	Logger      *slog.Logger
	Endpoint    objproto.Endpoint
	DialTimeout time.Duration // 0 → 10s default

	// OnDialed, when non-nil, is called with the server root context, the
	// raw objproto.Connection produced by a successful ECDH handshake, and
	// optional via-registration metadata. viaInfo is non-nil only for Phase C
	// (HandleWithVia) registrations; nil for Phase A direct (Handle).
	// If nil, the connection is closed immediately (useful in tests that only
	// check status codes without wanting a live connection).
	OnDialed func(ctx context.Context, conn objproto.Connection, viaInfo *ViaRegistrationInfo)

	// ResolveVia, when non-nil, is called for via-relay dispatch to look up
	// the registered proxy_runner by exact ConnectionID match. Returns the
	// RunnerEntry and true on hit, nil/false on miss. Server.New wires this
	// to Registry.GetByConnectionID.
	ResolveVia func(cid objproto.ConnectionID) (*RunnerEntry, bool)

	// ViaSendEstablishRelay, when non-nil, sends an EstablishRelayRequest
	// over the given proxy_runner's existing registered ConnHandle and blocks
	// until the corresponding EstablishRelayResponse arrives or ctx cancels.
	// Server.New wires this to Server.sendEstablishRelayRequest, which
	// correlates the response via a per-conn channel registered before send.
	ViaSendEstablishRelay func(ctx context.Context, entry *RunnerEntry, req protocol.EstablishRelayRequest) (protocol.EstablishRelayResponse, error)
}

// Handle performs the dial and returns the response struct. Does NOT wait
// for PSK / Hello to complete — those happen asynchronously in the goroutine
// spawned by OnDialed.
func (h *DialRunnerHandler) Handle(ctx context.Context, target protocol.RunnerID) protocol.DialRunnerResponse {
	if len(target.Transport) == 0 {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: invalid target: empty transport")
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}
	switch len(target.IpAddr) {
	case 0, 4, 16:
		// 0 is allowed by the schema constraint (ip_addr_len == 0 || 4 || 16)
	default:
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: invalid target: bad ip_addr_len", "len", len(target.IpAddr))
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}

	cid := protocol.RunnerIDToConnID(target)

	timeout := h.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := objproto.DoECDHHandshake(dialCtx, h.Endpoint, cid, ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: ECDH handshake failed", "target", fmt.Sprintf("%v", cid), "err", err)
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	// Emit a DialGreeting so the runner's accept handler can identify this
	// as a server-dialed conn (vs an agent-dialed conn that would send
	// ProxyControl). The greeting carries a version byte for forward
	// compatibility; runner ignores unknown versions.
	greeting := protocol.DialGreeting{Version: 1}
	greetingPayload := greeting.MustAppend([]byte{byte(wire.ApplicationPayloadKind_DialGreeting)})
	if _, _, err := conn.SendMessage(greetingPayload); err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner: failed to send greeting", "err", err)
		}
		// Close the conn so the runner's accept-side goroutine (blocked
		// in handleAcceptedConn waiting for the first inbound payload)
		// unblocks via pc.Done() instead of leaking until ctx cancellation.
		_ = conn.Close()
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	if h.OnDialed != nil {
		h.OnDialed(ctx, conn, nil)
	} else {
		conn.Close() //nolint:errcheck
	}

	return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok}
}

// HandleWithVia is the via-relay path for TaskControlKind_DialRunner. The
// production caller (task_handler.go) branches into HandleWithVia when
// req.Via.TransportLen != 0; an empty via short-circuits to Handle here so the
// caller only needs to invoke this single entry point.
//
// Ceremony (eager-SetProxy path, Task 4):
//
//  1. Validate target.Transport (InvalidTarget on empty).
//  2. Resolve via against the registry (ViaNotFound on miss).
//  3. Send EstablishRelay(target, slot_id) to proxy_runner over its existing
//     registered conn; proxy installs eager SetProxy(owned=(proxy.addr,slot),
//     allocate=(target.addr,slot)); wait for EstablishRelayResponse (ViaRelayFailed
//     on non-Ok).
//  4. SendHandshake to (proxy_runner.Addr, slot_id) — proxy_runner's eager
//     proxySettings entry forwards the Handshake raw to target; target ECDH's it;
//     the ACK flows back through proxy's SetProxy to server. Resulting activeConn
//     IS the end-to-end conn with target. No RehandshakeForProxy step needed.
//  5. Send DialGreeting{Version:1} as the first app message on the end-to-end
//     conn so target's listen handler discriminates this as a server-dial.
//  6. Hand off to OnDialed; PSK + RunnerHello + Registry insert run in the
//     normal handleConnection goroutine — identical to the direct-dial path.
//
// All wait points respect the dial timeout (DialTimeout, default 10s).
func (h *DialRunnerHandler) HandleWithVia(ctx context.Context, target, via protocol.RunnerID) protocol.DialRunnerResponse {
	if via.TransportLen == 0 {
		// "via not specified" — fall through to direct-dial path.
		return h.Handle(ctx, target)
	}

	// Step 1: target validation, before any registry / network work.
	if len(target.Transport) == 0 {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: invalid target: empty transport")
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}
	switch len(target.IpAddr) {
	case 0, 4, 16:
		// allowed by schema
	default:
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: invalid target: bad ip_addr_len", "len", len(target.IpAddr))
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}

	if h.ResolveVia == nil || h.ViaSendEstablishRelay == nil {
		// Programming error: via requested but server didn't wire the hooks.
		// Map to InvalidTarget so the admin sees a deterministic failure code
		// instead of a Go panic / nil deref.
		if h.Logger != nil {
			h.Logger.Error("dial-runner via: hooks not wired (programming error)")
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}

	// Step 2: resolve via against registered runners.
	viaCID := protocol.RunnerIDToConnID(via)
	entry, ok := h.ResolveVia(viaCID)
	if !ok {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: registered runner not found", "via", viaCID.String())
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaNotFound}
	}

	// Step 3: use target.UniqueNumber as slot_id and request relay setup.
	// target.UniqueNumber is generated at admin's CLI by ParseConnectionID's
	// `*` wildcard (random uint16) — equivalent in randomness to rand.Uint32
	// truncated. Reusing it stops orphaning the field and removes a second
	// randomness source. The proxy still enforces "slot_id != serverCID.ID"
	// via SlotCollision response on rare collision.
	slotID := target.UniqueNumber

	relayReq := protocol.EstablishRelayRequest{
		Target: target,
		SlotId: slotID,
	}

	timeout := h.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	relayCtx, relayCancel := context.WithTimeout(ctx, timeout)
	defer relayCancel()

	relayResp, err := h.ViaSendEstablishRelay(relayCtx, entry, relayReq)
	if err != nil || relayResp.Status != protocol.EstablishRelayStatus_Ok {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: EstablishRelay non-Ok",
				"via", viaCID.String(),
				"relay_status", relayResp.Status,
				"err", err)
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaRelayFailed}
	}

	// Step 3b: if the direct via is itself a via-registered runner (entry.Via != nil),
	// walk up the chain and set up EstablishRelay at each upstream hop in parallel.
	// This is required for N-hop registration: the Handshake sent in Step 4 travels
	// along the same path as the relay chain. Each hop from entry.Via upward must have
	// a SetProxy entry for slotID so the Handshake is forwarded rather than accepted
	// locally.
	if entry.Via != nil {
		allOk, walkErr, hopErrs := walkAndDispatchUpstreamHops(
			relayCtx, entry, slotID, h.DialTimeout, h.ViaSendEstablishRelay, h.Logger,
		)
		if walkErr != nil {
			if h.Logger != nil {
				h.Logger.Warn("dial-runner via: loop in Via chain, aborting registration",
					"err", walkErr)
			}
			return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaRelayFailed}
		}
		if !allOk {
			if h.Logger != nil {
				for _, he := range hopErrs {
					h.Logger.Warn("dial-runner via: upstream hop EstablishRelay non-Ok",
						"slot_id", slotID, "hop", he.HopID, "status", he.Status, "err", he.Err)
				}
			}
			return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_ViaRelayFailed}
		}
	}

	if h.Endpoint == nil {
		// Hooks wired but no endpoint — only reachable in misconfigured tests.
		if h.Logger != nil {
			h.Logger.Error("dial-runner via: Endpoint nil after EstablishRelay Ok")
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	// Step 4: ECDH server↔target at slot_id. With eager SetProxy installed at
	// proxy_runner (Task 4), the Handshake packet is forwarded raw by
	// proxy_runner's proxySettings entry to target's listen addr. target ECDH's
	// it; the ACK travels back through proxy's SetProxy to server. The resulting
	// activeConn IS the end-to-end conn — no rehandshake needed.
	//
	// entry.Conn is the ConnHandle registered at RunnerHello time; its
	// ConnectionID().Addr is the proxy_runner's listen address. slotCID points
	// there so the underlying transport reuses the live conn to proxy_runner to
	// send the packet — proxy's proxySettings entry then forwards it to target.
	proxyAddr := entry.Conn.ConnectionID().Addr
	proxyTransport := entry.Conn.ConnectionID().Transport
	slotCID := objproto.NewConnectionID(proxyTransport, proxyAddr, slotID)

	priv, hs, err := objproto.NewECDHHandshake(ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: NewECDHHandshake failed", "err", err)
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}
	ch, err := h.Endpoint.SendHandshake(slotCID, priv, hs)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: SendHandshake failed", "slot_cid", slotCID.String(), "err", err)
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}
	var endToEndConn objproto.Connection
	select {
	case <-relayCtx.Done():
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: timeout waiting end-to-end activeConn", "err", relayCtx.Err())
		}
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	case endToEndConn = <-ch.C:
		if endToEndConn == nil {
			if h.Logger != nil {
				h.Logger.Warn("dial-runner via: end-to-end activeConn nil (handshake table cleared)")
			}
			return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
		}
	}

	// Step 5: send DialGreeting on the end-to-end conn. AEAD validates that
	// keys are end-to-end server↔target (not relayed via proxy decrypt/re-encrypt).
	greeting := protocol.DialGreeting{Version: 1}
	greetingPayload := greeting.MustAppend([]byte{byte(wire.ApplicationPayloadKind_DialGreeting)})
	if _, _, err := endToEndConn.SendMessage(greetingPayload); err != nil {
		if h.Logger != nil {
			h.Logger.Warn("dial-runner via: send DialGreeting failed", "err", err)
		}
		_ = endToEndConn.Close()
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	// Step 6: hand off to OnDialed so handleConnection drives the standard
	// PSK + RunnerHello + Registry-insert flow. Pass ViaRegistrationInfo so the
	// Hello handler can populate entry.Via + entry.ViaDialAddr on the RunnerEntry.
	viaInfo := &ViaRegistrationInfo{
		Via:         entry,
		ViaDialAddr: protocol.RunnerIDToConnID(target),
	}
	if h.OnDialed != nil {
		h.OnDialed(ctx, endToEndConn, viaInfo)
	} else {
		// Tests / callers without OnDialed see a clean close — matches Handle.
		_ = endToEndConn.Close()
	}

	if h.Logger != nil {
		h.Logger.Info("dial-runner via: relay established",
			"target", protocol.RunnerIDToConnID(target).String(),
			"via", viaCID.String(),
			"slot_id", slotID)
	}
	return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok}
}
