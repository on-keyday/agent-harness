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

// dataPlaneHandshakeTimeout bounds the runner's answer to the hello.
const dataPlaneHandshakeTimeout = 10 * time.Second

// dialDataPlane opens the connection that carries one request's bytes.
//
// It dials the SERVER's address at the slot the server allocated, and the
// server forwards those packets to the runner without decrypting them — so the
// far end of this connection, and the peer the AEAD is with, is the runner.
// The endpoint must be the one the control connection was dialed on: the
// server matched its forwarding entry on the address that connection's packets
// come from, and a second socket would not be recognised.
func (c *Client) dialDataPlane(ctx context.Context, t dataPlaneTarget) (*peer.Conn, error) {
	ep := c.conn.Endpoint()
	if ep == nil {
		return nil, errors.New("file: no endpoint to open a data plane on (accepted conn?)")
	}
	serverCID := c.conn.Connection().ConnectionID()
	slotCID := objproto.NewConnectionID(serverCID.Transport, serverCID.Addr, t.SlotID)

	// Both ends here are peer.Conn, which defaults to the client half of the
	// stream-id space, so neither could create a stream the other would accept.
	// The client takes the server half: the runner's side is an ACCEPTED conn
	// whose kind is not known until its first payload is read, by which time
	// its trsf already exists, and flipping every accepted conn would put the
	// server-dialed ones on the same half as the real server.
	pc, err := peer.Dial(ctx, ep, slotCID, peer.DialConfig{
		Logger:                        slog.Default(),
		CreatesServerInitiatedStreams: true,
		MTU:                           int(t.MTU),
	})
	if err != nil {
		return nil, fmt.Errorf("file: dial data plane: %w", err)
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
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, dataPlaneHandshakeTimeout)
	defer cancel()
	select {
	case <-waitCtx.Done():
		pc.Connection().Close() //nolint:errcheck
		return nil, fmt.Errorf("file: data plane handshake: %w", waitCtx.Err())
	case resp := <-respCh:
		if err := dataPlaneStatusError(resp.Status); err != nil {
			pc.Connection().Close() //nolint:errcheck
			return nil, err
		}
	}
	return pc, nil
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
	pc, err := c.dialDataPlane(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { pc.Connection().Close() } //nolint:errcheck

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
