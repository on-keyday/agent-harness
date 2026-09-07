package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// dataPlaneTarget is what the server hands back when it routes a request end to
// end instead of splicing it. A zero GrantID means it did not, and the caller
// takes the stream the server allocated on its own connection.
type dataPlaneTarget struct {
	GrantID [16]uint8
	TaskID  protocol.TaskID
	SlotID  uint16
	// MTU is the packet size the server picked for this connection, or 0 to
	// keep the one this end's transport implies. The server computes it
	// because it is the only party that sees both transports; neither end
	// restates the rule.
	MTU uint16
	// RunnerCID is WHERE TO DIAL. Zero means the server's own address at
	// SlotID, which it forwards; non-zero is the runner's address, reached
	// directly because the server has already had it punch a path open toward
	// this client. The client does not choose between them -- the server
	// answers with one or the other, because only the server knows whether the
	// punch was asked for.
	RunnerCID protocol.RunnerID
}

// use reports whether the server routed this request end to end.
func (t dataPlaneTarget) use() bool {
	return t.GrantID != ([16]uint8{}) && t.SlotID != 0
}

// Errors a data-plane refusal turns into, so a caller can tell a withdrawn
// authority from a stale one and from a plain network failure.
var (
	ErrDataPlaneRefused = errors.New("file: the runner refused this transfer's credential")
	ErrDataPlaneExpired = errors.New("file: this transfer's authorization expired before it was used")
	ErrDataPlaneRevoked = errors.New("file: this transfer's authorization no longer covers it (capabilities narrowed?)")
)

// dataPlaneHandshakeTimeout bounds the WHOLE data-plane setup: the dial's key
// exchange and the runner's answer to the hello, together. Both can hang on a
// path that is not open -- the forwarded route if the server's proxy entry is
// gone, the direct route if the punch did not reach -- and neither has any
// other deadline over it.
const dataPlaneHandshakeTimeout = 10 * time.Second

// dialDataPlane opens the connection that carries one request's bytes.
//
// The far end, and the peer the AEAD is with, is the runner either way. What
// the server's answer decides is how the packets get there: to the server's own
// address at the allocated slot, which it forwards without decrypting, or
// straight to the runner's address once it has been punched toward this client.
// The second crosses one hop instead of two, which is worth about 2.6x at 20ms
// RTT and more with loss.
func (c *Client) dialDataPlane(ctx context.Context, t dataPlaneTarget) (*peer.Conn, context.CancelFunc, error) {
	ep := c.conn.Endpoint()
	if ep == nil {
		return nil, nil, errors.New("file: no endpoint to open a data plane on (accepted conn?)")
	}
	// Where to dial, per the server's answer. The endpoint is the same either
	// way and that is load-bearing for BOTH routes: the relay matches its
	// forwarding entry on the address this socket sends from, and the direct
	// route was punched toward that same address. A second socket would be
	// recognised by neither.
	serverCID := c.conn.Connection().ConnectionID()
	slotCID := objproto.NewConnectionID(serverCID.Transport, serverCID.Addr, t.SlotID)
	if t.RunnerCID.TransportLen != 0 {
		rc := protocol.RunnerIDToConnID(t.RunnerCID)
		slotCID = objproto.NewConnectionID(rc.Transport, rc.Addr, t.SlotID)
	}

	// Both ends here are peer.Conn, which defaults to the client half of the
	// stream-id space, so neither could create a stream the other would accept.
	// The client takes the server half: the runner's side is an ACCEPTED conn
	// whose kind is not known until its first payload is read, by which time
	// its trsf already exists, and flipping every accepted conn would put the
	// server-dialed ones on the same half as the real server.
	// The dial needs a bound, and the bound may NOT be a deadline on the
	// connection's own context: peer.Dial hands that ctx to WrapAcceptedConn,
	// which derives the STREAM lifetime from it, and Start runs AutoReceive on
	// it. A context.WithTimeout here therefore killed the transfer ten seconds
	// in -- "stream write: context canceled" on every forwarded and direct
	// push. That was this function's own regression, fixed by bounding the
	// WAIT instead of the connection.
	//
	// So: dial on a cancellable child of the caller's context, race it against
	// the clock, and hand the cancel to the caller so it fires when the
	// connection closes rather than when the handshake finishes.
	//
	// It has to be bounded at all because a direct dial the punch did not open
	// otherwise parks forever -- observed on the live fleet at six minutes with
	// no CPU, on a pair that had completed in 520ms minutes earlier. With no
	// fallback by design, a prompt failure is the whole of what the caller gets.
	//
	// The bound is no longer one shot, though: objproto retransmits the
	// handshake from inside its own wait, first after 333ms and doubling, so
	// ten seconds is about five attempts. That matters here because losing the
	// race with the punch's first probe used to cost the whole transfer -- the
	// path opens milliseconds later and stays open for the grant's lifetime.
	dialCtx, cancelDial := context.WithCancel(ctx)
	type dialed struct {
		pc  *peer.Conn
		err error
	}
	ch := make(chan dialed, 1) // buffered: the goroutine must never block on an abandoned dial
	go func() {
		pc, err := peer.Dial(dialCtx, ep, slotCID, peer.DialConfig{
			Logger:                        slog.Default(),
			CreatesServerInitiatedStreams: true,
			MTU:                           int(t.MTU),
		})
		ch <- dialed{pc, err}
	}()

	var pc *peer.Conn
	select {
	case d := <-ch:
		if d.err != nil {
			cancelDial()
			return nil, nil, fmt.Errorf("file: dial data plane: %w", d.err)
		}
		pc = d.pc
	case <-time.After(dataPlaneHandshakeTimeout):
		cancelDial() // stops the dial in flight; the goroutine's send still fits the buffer
		return nil, nil, fmt.Errorf("file: dial data plane: no answer within %v", dataPlaneHandshakeTimeout)
	case <-ctx.Done():
		cancelDial()
		return nil, nil, fmt.Errorf("file: dial data plane: %w", ctx.Err())
	}

	respCh := make(chan protocol.PskAuthResponse, 1)
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind != appwire.AppKind_PskAuth || len(payload) == 0 {
			return
		}
		var resp protocol.PskAuthResponse
		if _, err := resp.Decode(payload); err != nil {
			return
		}
		select {
		case respCh <- resp:
		default:
		}
	})
	pc.Start(ctx)

	// The connect PSK, never the operator one: the runner holds only the
	// former, and an operator secret has no business leaving for a runner.
	if err := sendDataPlaneHello(pc, GetPSK(), t); err != nil {
		pc.Connection().Close() //nolint:errcheck
		cancelDial()
		return nil, nil, err
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, dataPlaneHandshakeTimeout)
	defer cancelWait()
	select {
	case <-waitCtx.Done():
		pc.Connection().Close() //nolint:errcheck
		cancelDial()
		return nil, nil, fmt.Errorf("file: data plane handshake: %w", waitCtx.Err())
	case resp := <-respCh:
		if err := dataPlaneStatusError(resp.Status); err != nil {
			pc.Connection().Close() //nolint:errcheck
			cancelDial()
			return nil, nil, err
		}
	}
	return pc, cancelDial, nil
}

// sendDataPlaneHello writes the one message that proves fleet membership and
// presents the grant. It rides the existing PSK handshake rather than a new
// first message, so the binder already binds it to THIS connection's
// transcript.
func sendDataPlaneHello(pc *peer.Conn, psk []byte, t dataPlaneTarget) error {
	var req protocol.PskAuthRequest
	if len(psk) > 0 {
		binder, err := ComputePSKBinder(psk, pc.Connection().GetTranscript())
		if err != nil {
			return fmt.Errorf("file: compute binder: %w", err)
		}
		req.Binder = binder
		req.BinderLen = uint16(len(binder))
	}
	req.Role = protocol.AuthRole_Client
	var hello protocol.ClientHello
	hello.Kind = protocol.ClientKind_DataPlane
	hello.SetDataPlaneInfo(protocol.DataPlaneInfo{GrantId: t.GrantID, TaskId: t.TaskID})
	req.SetClientHello(hello)

	payload, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		return fmt.Errorf("file: encode data plane hello: %w", err)
	}
	if _, _, err := pc.Connection().SendMessage(payload); err != nil {
		return fmt.Errorf("file: send data plane hello: %w", err)
	}
	return nil
}

// dataPlaneStatusError turns the runner's answer into an error a person can act
// on. A revoked or expired grant is not a network fault and must not be
// reported as one.
func dataPlaneStatusError(st protocol.PskAuthStatus) error {
	switch st {
	case protocol.PskAuthStatus_Ok:
		return nil
	case protocol.PskAuthStatus_Expired:
		return ErrDataPlaneExpired
	case protocol.PskAuthStatus_NotPermitted:
		return ErrDataPlaneRevoked
	case protocol.PskAuthStatus_BadPsk:
		return fmt.Errorf("%w: the runner did not accept this fleet's PSK", ErrDataPlaneRefused)
	default:
		return fmt.Errorf("%w (status=%v)", ErrDataPlaneRefused, st)
	}
}

// openDataPlaneStream completes the ceremony: dial, hello, open the stream that
// will carry the bytes, and tell the runner which request it is and which
// stream carries it. Returns the stream and the closer for the connection
// underneath it.
func (c *Client) openDataPlaneStream(
	ctx context.Context,
	t dataPlaneTarget,
	build func(streamID uint64) protocol.RunnerRequest,
) (trsf.BidirectionalStream, func(), error) {
	pc, cancelDial, err := c.dialDataPlane(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	// pc.Close, not pc.Connection().Close: it sends the trsf close, which is
	// how the runner learns at once that the transfer is over. Without it the
	// runner waits out its drain timeout before reporting the end, and the
	// server holds the forwarding entry that whole time. The peer on the far
	// side of this connection IS the runner, so telling it is the point --
	// Pitfall 5's warning is about a close reaching a peer that was not meant
	// to hear it.
	closer := func() { pc.Close(); cancelDial() }

	st := pc.Transport().CreateBidirectionalStream()
	if st == nil {
		closer()
		return nil, nil, errors.New("file: could not open a stream on the data plane")
	}
	req := build(uint64(st.ID()))
	payload, err := req.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		closer()
		return nil, nil, fmt.Errorf("file: encode data plane request: %w", err)
	}
	if _, _, err := pc.Connection().SendMessage(payload); err != nil {
		closer()
		return nil, nil, fmt.Errorf("file: send data plane request: %w", err)
	}
	return st, closer, nil
}
