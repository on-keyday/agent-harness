package transport

import (
	"crypto/tls"
	"errors"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
)

func ClientSession(logger *slog.Logger, addr string, udpPort uint16) (objproto.ConnectionID, objproto.Session, error) {
	cid, err := objproto.ParseConnectionID(addr, objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		return objproto.ConnectionID{}, nil, err
	}
	sess := objproto.NewSession(logger, objproto.SessionModeClient)
	switch cid.Transport {
	case "ws", "wss":
		_, err = WebSocketSessionEx(sess, logger, addr, nil, sess.GetSenderChannel())
		if err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	case "udp":
		_, err = UDPSessionEx(sess, logger, udpPort, sess.GetSenderChannel())
		if err != nil {
			return objproto.ConnectionID{}, nil, err
		}
	default:
		return objproto.ConnectionID{}, nil, errors.New("unsupported transport: " + cid.Transport)
	}
	return cid, sess, nil
}

func UDPWebsocketDualStackSession(logger *slog.Logger, udpPort uint16, wsAddr string, tlsConf *tls.Config, sessMode objproto.SessionMode) (objproto.Session, error) {
	sess := objproto.NewSession(logger, sessMode)
	baseChan := sess.GetSenderChannel()
	udpChan := make(chan *objproto.PacketData, 100)
	wsChan := make(chan *objproto.PacketData, 100)

	_, err := UDPSessionEx(sess, logger, udpPort, udpChan)
	if err != nil {
		return nil, err
	}

	_, err = WebSocketSessionEx(sess, logger, wsAddr, tlsConf, wsChan)
	if err != nil {
		return nil, err
	}

	go func() {
		for pkt := range baseChan {
			switch pkt.To.Transport {
			case "udp":
				udpChan <- pkt
			case "ws", "wss":
				wsChan <- pkt
			default:
				logger.Error("unsupported transport for udp-websocket session", slog.String("transport", pkt.To.Transport))
			}
		}
	}()

	return sess, nil
}
