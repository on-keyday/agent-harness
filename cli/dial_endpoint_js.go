//go:build js

package cli

import (
	"fmt"
	"log/slog"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/transport"
)

// BuildClientEndpoint constructs a Client-mode objproto.Endpoint for the
// transport selected by peerCID.Transport. The wasm build supports
// WebSocket only — UDP is not reachable from the browser.
func BuildClientEndpoint(peerCID objproto.ConnectionID) (objproto.Endpoint, error) {
	switch peerCID.Transport {
	case "ws", "wss":
		ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
			Logger: slog.Default(),
			Path:   WebSocketPath,
			Mode:   objproto.EndpointModeClient,
		})
		if err != nil {
			return nil, fmt.Errorf("ws endpoint: %w", err)
		}
		return ep, nil
	case "udp":
		return nil, fmt.Errorf("udp transport is not supported in the WASM build")
	default:
		return nil, fmt.Errorf("unsupported transport %q in CID", peerCID.Transport)
	}
}
