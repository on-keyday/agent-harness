package runner

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// checkDataPlaneHello is the runner's admission gate for a data-plane
// connection. It mirrors server/psk.go's pskGate.Check in ORDER — decode,
// verify the binder before any identity action, require an identity,
// fail-closed on anything unexpected — but not in scope: the server's gate
// admits every client kind and both roles, this one admits exactly one thing.
// Everything else is refused, which is why it is not a second copy of the
// server's policy.
//
// It deliberately does NOT decide whether the grant is good; that is
// validateDataPlaneHello, so the credential check stays in one place with the
// store that holds it.
func checkDataPlaneHello(data, transcript, psk []byte) (*protocol.DataPlaneInfo, protocol.PskAuthStatus) {
	if len(data) == 0 || appwire.AppKind(data[0]) != appwire.AppKind_PskAuth {
		return nil, protocol.PskAuthStatus_NoIdentity
	}
	var req protocol.PskAuthRequest
	if err := req.DecodeExact(data[1:]); err != nil {
		return nil, protocol.PskAuthStatus_NoIdentity
	}

	// Binder first, before anything is read out of the identity. A runner holds
	// the connect PSK only — there is no operator secret here, and an operator
	// surface has no business reaching a runner directly, so there is no
	// second key to resolve.
	if len(psk) > 0 {
		expected, err := cli.ComputePSKBinder(psk, transcript)
		if err != nil || subtle.ConstantTimeCompare(req.Binder, expected) != 1 {
			return nil, protocol.PskAuthStatus_BadPsk
		}
	}

	if req.Role != protocol.AuthRole_Client {
		return nil, protocol.PskAuthStatus_NoIdentity
	}
	hello := req.ClientHello()
	if hello == nil || hello.Kind != protocol.ClientKind_DataPlane {
		return nil, protocol.PskAuthStatus_NoIdentity
	}
	info := hello.DataPlaneInfo()
	if info == nil {
		return nil, protocol.PskAuthStatus_NoIdentity
	}
	return info, protocol.PskAuthStatus_Ok
}

// validateDataPlaneHello is the whole authorization decision for a data-plane
// connection, as a pure function so it is testable without a socket.
//
// The runner holds no capability and no scope: it matches the grant the server
// pushed against the request that arrived. A nil session means no server
// connection has ever been established here, so no grant can have been pushed —
// answered as unknown_task rather than bad_ticket, which keeps it
// indistinguishable from a task this runner does not have.
func validateDataPlaneHello(
	sess *Session,
	info protocol.DataPlaneInfo,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	now time.Time,
) protocol.ClientHelloStatus {
	if sess == nil || sess.Grants == nil {
		return protocol.ClientHelloStatus_UnknownTask
	}
	return sess.Grants.Validate(info.GrantId, info.TaskId, kind, dir, now)
}

// dataPlaneMTUFor answers the packet size to build an accepted connection with.
//
// A forwarded connection can join two different transports, and each end would
// otherwise size its packets from its own; the server picks the smaller and
// sends it with the grant. Zero means it negotiated none, which is what a
// same-transport pair needs.
func dataPlaneMTUFor(sessionRef *atomic.Pointer[Session], cid objproto.ConnectionID) int {
	sess := sessionRef.Load()
	if sess == nil || sess.Grants == nil {
		return 0
	}
	return int(sess.Grants.MTUForSlot(cid.ID))
}

// pskStatusFor maps the store's answer onto the status that fits in a
// PskAuthResponse, which is what a data-plane connection is actually answered
// with. The two vocabularies exist because the store speaks about a grant and
// the wire speaks about a handshake; keeping the mapping in one function stops
// them drifting.
func pskStatusFor(st protocol.ClientHelloStatus) protocol.PskAuthStatus {
	switch st {
	case protocol.ClientHelloStatus_Ok:
		return protocol.PskAuthStatus_Ok
	case protocol.ClientHelloStatus_Expired:
		return protocol.PskAuthStatus_Expired
	case protocol.ClientHelloStatus_NotPermitted:
		return protocol.PskAuthStatus_NotPermitted
	default:
		// bad_ticket, unknown_task and anything new: a client is told its
		// credential was refused and not which of the two it was.
		return protocol.PskAuthStatus_BadTicket
	}
}

// sendPskAuthStatus answers a data-plane handshake.
func sendPskAuthStatus(pc *peer.Conn, st protocol.PskAuthStatus) {
	resp := protocol.PskAuthResponse{Status: st}
	payload := resp.MustAppend([]byte{byte(appwire.AppKind_PskAuth)})
	_, _, _ = pc.Connection().SendMessage(payload)
}

// handleDataPlaneConn serves one connection opened purely to carry an
// authorized request's bytes.
//
// Order, mirroring server/psk.go: gate the handshake, then check the grant,
// then read the request, then serve. The request arrives from the CLIENT on
// this connection rather than from the server on the control connection,
// because the client is the party that knows the path and the runner is the
// party that validates it — and because that generalizes to git_query and exec
// without the grant having to learn anything about them.
//
// Teardown uses pc.Close(), not pc.Connection().Close(). The file handler
// returns as soon as it has WRITTEN the last bytes, not when they have left,
// so tearing the objproto connection down underneath it drops whatever the
// send path still holds -- measured as a `file ls` that hangs forever with the
// runner logging a clean, complete serve. peer.Conn.Close sends the trsf close
// the client needs to see EOF and drains before it goes.
//
// Pitfall 5 warns against pc.Close() on a relay-setup conn because the wire
// close travels through a SetProxy entry to a peer that was not the intended
// one. Here the peer on the other side of that entry IS the client this
// connection belongs to, and it is exactly who should be told.
func handleDataPlaneConn(
	ctx context.Context,
	cfg Config,
	sessionRef *atomic.Pointer[Session],
	pc *peer.Conn,
	first firstMsgT,
) {
	defer closeWhenDrained(ctx, pc)
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	psk := cfg.PSK
	if psk == nil {
		psk = cli.GetPSK()
	}
	data := append([]byte{byte(first.kind)}, first.payload...)
	info, st := checkDataPlaneHello(data, pc.Connection().GetTranscript(), psk)
	if st != protocol.PskAuthStatus_Ok {
		log.Warn("data plane: handshake refused", "status", st)
		sendPskAuthStatus(pc, st)
		return
	}

	sess := sessionRef.Load()
	if sess == nil || sess.Grants == nil {
		// No server connection has been established here, so no grant can have
		// been pushed. Silence would read as a bare close on the client.
		log.Warn("data plane: refused, no server session on this runner",
			"session_nil", sess == nil)
		sendPskAuthStatus(pc, protocol.PskAuthStatus_BadTicket)
		return
	}
	kind, ok := sess.Grants.Kind(info.GrantId)
	if !ok {
		log.Warn("data plane: refused, no such grant")
		sendPskAuthStatus(pc, protocol.PskAuthStatus_BadTicket)
		return
	}
	log.Debug("data plane: grant redeemed", "kind", kind)

	// Arm the request handler BEFORE answering the hello. The client sends its
	// request the moment it sees the Ok, and the handler installed by the
	// accept path would otherwise swallow it into a channel nobody reads any
	// more -- a race that reads as a transfer that hangs with no error on
	// either side.
	reqCh := armDataPlaneRequest(ctx, pc)

	sendPskAuthStatus(pc, protocol.PskAuthStatus_Ok)

	// The grant can be revoked while this connection is open; that has to reach
	// the transfer, or a narrowing `caps set` would be advisory here.
	sess.Grants.OnClose(info.GrantId, func() { pc.Connection().Close() }) //nolint:errcheck

	req, err := awaitDataPlaneRequest(ctx, reqCh)
	if err != nil {
		log.Warn("data plane: no request arrived", "err", err)
		return
	}
	serveDataPlaneRequest(ctx, log, sess, pc, *info, kind, req)
}

// armDataPlaneRequest installs the handler for the one RunnerRequest the client
// sends after the handshake, and must be called BEFORE the hello is answered.
//
// It reuses the existing server->runner request envelope rather than inventing
// a client->runner one: the runner reads the same bytes either way, and the
// stream id in it names a stream on THIS connection.
func armDataPlaneRequest(ctx context.Context, pc *peer.Conn) <-chan *protocol.RunnerRequest {
	ch := make(chan *protocol.RunnerRequest, 1)
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind != appwire.AppKind_RunnerControl {
			return
		}
		req := &protocol.RunnerRequest{}
		if _, err := req.Decode(payload); err != nil {
			return
		}
		select {
		case ch <- req:
		default:
		}
	})
	pc.Start(ctx) // idempotent; the accept path started it already
	return ch
}

// awaitDataPlaneRequest blocks for the armed request.
func awaitDataPlaneRequest(ctx context.Context, ch <-chan *protocol.RunnerRequest) (*protocol.RunnerRequest, error) {
	waitCtx, cancel := context.WithTimeout(ctx, dataPlaneRequestTimeout)
	defer cancel()
	select {
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	case req := <-ch:
		return req, nil
	}
}

// dataPlaneRequestTimeout bounds how long an authorized connection may sit
// without saying what it wants. The grant's TTL bounds redeemability; this
// bounds a redeemed connection that then does nothing.
const dataPlaneRequestTimeout = 30 * time.Second

// dataPlaneDrainTimeout backstops a client that stops reading without hanging
// up. It is not the transfer budget: by the time it starts, the handler has
// already written everything.
const dataPlaneDrainTimeout = 30 * time.Second

// closeWhenDrained tears the connection down only once the client has hung up.
//
// The file handlers return when the last bytes have been WRITTEN, not when they
// have left, so closing straight away drops whatever the send path still holds.
// That was measured, not guessed: `file ls` hung forever while the runner
// logged a clean, complete serve, and a three-second delay before the close
// made the listing appear. Waiting for the peer is the version of that with no
// magic number in it -- the client closes when it has read to EOF, which is the
// only party that knows when the transfer is over.
func closeWhenDrained(ctx context.Context, pc *peer.Conn) {
	select {
	case <-pc.Done():
	case <-ctx.Done():
	case <-time.After(dataPlaneDrainTimeout):
	}
	pc.Close()
}

// serveDataPlaneRequest runs the request the grant authorized, after checking
// that the request that arrived is the one the grant names.
func serveDataPlaneRequest(
	ctx context.Context,
	log *slog.Logger,
	sess *Session,
	pc *peer.Conn,
	info protocol.DataPlaneInfo,
	kind protocol.TaskControlKind,
	req *protocol.RunnerRequest,
) {
	switch kind {
	case protocol.TaskControlKind_OpenFileTransfer:
		oft := req.OpenFileTransfer()
		if oft == nil || req.Kind != protocol.RunnerRequestType_OpenFileTransfer {
			log.Warn("data plane: grant says open_file_transfer, request does not")
			return
		}
		if st := validateDataPlaneHello(sess, info, kind, oft.Direction, time.Now()); st != protocol.ClientHelloStatus_Ok {
			log.Warn("data plane: request refused", "status", st)
			sendPskAuthStatus(pc, pskStatusFor(st))
			return
		}
		if oft.TaskId.Id != info.TaskId.Id {
			log.Warn("data plane: request names a task the grant does not")
			return
		}
		sess.handleOpenFileTransferOn(ctx, pc.Transport(), oft)
	case protocol.TaskControlKind_ListFiles:
		lf := req.ListFiles()
		if lf == nil || req.Kind != protocol.RunnerRequestType_ListFiles {
			log.Warn("data plane: grant says list_files, request does not")
			return
		}
		if st := validateDataPlaneHello(sess, info, kind, 0, time.Now()); st != protocol.ClientHelloStatus_Ok {
			log.Warn("data plane: request refused", "status", st)
			sendPskAuthStatus(pc, pskStatusFor(st))
			return
		}
		if lf.TaskId.Id != info.TaskId.Id {
			log.Warn("data plane: request names a task the grant does not")
			return
		}
		sess.handleListFilesOn(ctx, pc.Transport(), lf)
	default:
		log.Warn("data plane: grant names a request this runner does not serve", "kind", kind)
	}
}
