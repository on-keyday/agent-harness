package transport

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/on-keyday/agent-harness/objproto"
)

// ClientEndpoint is a convenience constructor that resolves a server CID
// and wires up the appropriate transport (ws/wss/udp) for a Client-mode
// objproto Endpoint. Currently caller-zero in this repo; preserved as
// template alongside UDPWebsocketDualStackEndpoint for future use.
func ClientEndpoint(logger *slog.Logger, addr string, udpPort uint16) (objproto.ConnectionID, objproto.Endpoint, error) {
	cid, err := objproto.ParseConnectionID(addr, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		return objproto.ConnectionID{}, nil, err
	}
	sess := objproto.NewEndpoint(logger, objproto.EndpointModeClient)
	switch cid.Transport {
	case "ws", "wss":
		err = WebSocketEndpointEx(sess, nil, WebSocketConfig{
			Logger: logger,
			Mode:   objproto.EndpointModeClient,
		})
		if err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	case "udp":
		_, err = UDPEndpointEx(sess, logger, udpPort, sess.GetSenderChannel())
		if err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	default:
		return objproto.ConnectionID{}, nil, errors.New("unsupported transport: " + cid.Transport)
	}
	return cid, sess, nil
}

// UDPWebsocketDualStackConfig configures a UDP+WebSocket dual stack
// Endpoint that shares a single objproto RawEndpoint across both
// transports. This is template code: there are no callers in this repo,
// but the wiring pattern (one rawSess fed by two transports, sender
// channel split by pkt.To.Transport) is preserved as a reference for
// future UDP-on-harness work. If this code has bit-rotted by the time you
// need it, prefer fixing it over deleting it.
//
// WS.Mode selects the WS leg behaviour (Client / Server / Mutual). Mux
// must be non-nil for Server / Mutual; nil for Client.
type UDPWebsocketDualStackConfig struct {
	Logger  *slog.Logger
	UDPPort uint16
	Mux     *http.ServeMux  // required when WS.Mode is Server or Mutual; nil for Client
	WS      WebSocketConfig // Mode / Path / TLS / Logger drive the WS leg
}

type UDPWebsocketDualStack struct {
	Endpoint objproto.Endpoint
}

func UDPWebsocketDualStackEndpoint(cfg UDPWebsocketDualStackConfig) (UDPWebsocketDualStack, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, cfg.WS.Mode)
	udpChan := make(chan *objproto.PacketData, 100)
	wsChan := make(chan *objproto.PacketData, 100)

	if _, err := UDPEndpointEx(rawSess, cfg.Logger, cfg.UDPPort, udpChan); err != nil {
		return UDPWebsocketDualStack{}, err
	}

	if err := WebSocketEndpointEx(rawSess, cfg.Mux, cfg.WS); err != nil {
		return UDPWebsocketDualStack{}, err
	}

	// BROKEN AS-IS: this loop and WebSocketEndpointEx's internal sender loop
	// both range over rawSess.GetSenderChannel(), so packets are split
	// non-deterministically between the two readers. Calling
	// UDPWebsocketDualStackEndpoint today will lose roughly half the WS-bound
	// traffic. UDPEndpointEx accepts an explicit sendTo channel arg
	// (UDPEndpointEx(sess, logger, port, sendTo)); WebSocketEndpointEx
	// dropped the equivalent sendTo override during the WebSocketConfig
	// refactor and now always reads rawSess.GetSenderChannel() directly.
	// Preserved per plan as a template for future re-wiring (likely fix:
	// re-introduce a sendTo override on WebSocketEndpointEx mirroring
	// UDPEndpointEx, then route through this fan-out). If you need
	// dualstack today, fix the asymmetry first — do not delete this code.
	go func() {
		for pkt := range rawSess.GetSenderChannel() {
			switch pkt.To.Transport {
			case "udp":
				udpChan <- pkt
			case "ws", "wss":
				wsChan <- pkt
			default:
				cfg.Logger.Error("unsupported transport for udp-websocket session", slog.String("transport", pkt.To.Transport))
			}
		}
	}()

	return UDPWebsocketDualStack{Endpoint: rawSess}, nil
}
