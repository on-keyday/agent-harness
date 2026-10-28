//go:build !js

package cli

import (
	"fmt"
	"log/slog"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
)

// BuildClientEndpoint constructs a Client-mode objproto.Endpoint for the
// transport selected by peerCID.Transport. Native build supports WebSocket,
// UDP and vsock; the WASM counterpart in dial_endpoint_js.go is
// WebSocket-only because raw UDP is not available in the browser env.
//
//   - "ws" / "wss" → transport.WebSocketEndpoint
//   - "udp"        → transport.UDPEndpoint with OS-assigned local port
//   - "vsock"      → a dial-only transport.VsockLeg (Linux only)
//   - other        → error
func BuildClientEndpoint(peerCID objproto.ConnectionID) (objproto.Endpoint, error) {
	switch peerCID.Transport {
	case "ws", "wss":
		ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
			Logger: slog.Default(),
			Path:   WebSocketPath,
			TLS:    ClientTLSConfig(),
			Mode:   objproto.EndpointModeClient,
		})
		if err != nil {
			return nil, fmt.Errorf("ws endpoint: %w", err)
		}
		return ep, nil
	case "udp":
		ep, err := transport.UDPEndpoint(slog.Default(), 0, objproto.EndpointModeClient)
		if err != nil {
			return nil, fmt.Errorf("udp endpoint: %w", err)
		}
		return ep, nil
	case objproto.TransportVsock:
		ep, err := transport.MultiLegEndpoint(slog.Default(), objproto.EndpointModeClient,
			transport.VsockLeg(transport.VsockConfig{Logger: slog.Default()}))
		if err != nil {
			return nil, fmt.Errorf("vsock endpoint: %w", err)
		}
		return ep, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q in CID", peerCID.Transport)
	}
}
