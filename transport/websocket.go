package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"sync"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/objproto/packet"
	"golang.org/x/net/websocket"
)

// WebSocketConn is a connection that uses a WebSocket for communication.
type WebSocketConn struct {
	conn       *websocket.Conn
	remoteAddr netip.AddrPort
	cancel     context.CancelFunc
}

// newWebSocketConn creates a new WebSocketConn.
func newWebSocketConn(conn *websocket.Conn, remoteAddr netip.AddrPort, cancel context.CancelFunc) *WebSocketConn {
	return &WebSocketConn{
		conn:       conn,
		remoteAddr: remoteAddr,
		cancel:     cancel,
	}
}

// Send writes a message to the WebSocket connection.
func (c *WebSocketConn) Send(p []byte) error {
	return websocket.Message.Send(c.conn, p)
}

// Receive reads a message from the WebSocket connection.
func (c *WebSocketConn) Receive() ([]byte, error) {
	var p []byte
	err := websocket.Message.Receive(c.conn, &p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Close closes the WebSocket connection.
func (c *WebSocketConn) Close() error {
	c.cancel()
	return c.conn.Close()
}

// RemoteAddr returns the remote network address.
func (c *WebSocketConn) RemoteAddr() string {
	return c.remoteAddr.String()
}

type connectionMap struct {
	connMapLock sync.RWMutex
	connMap     map[netip.AddrPort]*WebSocketConn
}

func (m *connectionMap) Get(addr netip.AddrPort) (*WebSocketConn, bool) {
	m.connMapLock.RLock()
	defer m.connMapLock.RUnlock()
	conn, ok := m.connMap[addr]
	return conn, ok
}

func (m *connectionMap) Set(addr netip.AddrPort, conn *WebSocketConn) {
	m.connMapLock.Lock()
	defer m.connMapLock.Unlock()
	m.connMap[addr] = conn
}

func (m *connectionMap) Delete(addr netip.AddrPort) {
	m.connMapLock.Lock()
	defer m.connMapLock.Unlock()
	delete(m.connMap, addr)
}

func handleRawSession(transportName string, connChan chan *WebSocketConn, senderChannel <-chan *objproto.PacketData, rawSess objproto.RawSession, connMap *connectionMap, logger *slog.Logger, tlsConf *tls.Config) {
	go func() {
		for conn := range connChan {
			go func(c *WebSocketConn) {
				for {
					recv, err := c.Receive()
					if err != nil {
						// map
						connMap.Delete(c.remoteAddr)
						if errors.Is(err, io.EOF) {
							logger.Info("websocket connection closed by remote", slog.String("address", c.remoteAddr.String()))
						} else {
							logger.Error("failed to receive websocket message", slog.String("address", c.remoteAddr.String()), slog.String("error", err.Error()))
						}
						return
					}
					rawSess.Receive(transportName, c.remoteAddr, recv)
				}
			}(conn)
		}
	}()

	go func() {
		for pkt := range senderChannel {
			conn, ok := connMap.Get(pkt.To.Addr)
			if !ok {
				if pkt.Kind == packet.PacketKind_Handshake { // initial, create connection
					go func() {
						wsScheme := "ws"
						httpScheme := "http"
						if tlsConf != nil {
							wsScheme = "wss"
							httpScheme = "https"
						}
						conf := &websocket.Config{
							Location: &url.URL{
								Scheme: wsScheme,
								Host:   pkt.To.Addr.String(),
							},
							Origin: &url.URL{
								Scheme: httpScheme,
								Host:   pkt.To.Addr.String(),
							},
							TlsConfig: tlsConf,
							Version:   websocket.ProtocolVersionHybi13,
						}
						ws, err := websocket.DialConfig(conf)
						if err != nil {
							logger.Error("failed to dial websocket", slog.String("address", pkt.To.Addr.String()), slog.String("error", err.Error()))
							rawSess.CannotSend(pkt)
							return
						}
						conn := newWebSocketConn(ws, pkt.To.Addr, func() {})
						connMap.Set(pkt.To.Addr, conn)
						err = conn.Send(pkt.Data)
						if err != nil {
							logger.Error("failed to send websocket handshake message", slog.String("to", pkt.To.String()), slog.String("error", err.Error()))
							rawSess.CannotSend(pkt)
							connMap.Delete(pkt.To.Addr)
							return
						}
						connChan <- conn
					}()
					continue
				}
				logger.Error("no websocket connection for address", slog.String("address", pkt.To.String()))
				rawSess.CannotSend(pkt)
				continue
			}
			err := conn.Send(pkt.Data)
			if err != nil {
				logger.Error("failed to send websocket message", slog.String("to", pkt.To.String()), slog.String("error", err.Error()))
				rawSess.CannotSend(pkt)
				connMap.Delete(pkt.To.Addr)
			}
		}
	}()
}

func WebSocketSession(logger *slog.Logger, addr string, tlsConf *tls.Config, sessMode objproto.SessionMode) (objproto.Session, error) {
	rawSess := objproto.NewSession(logger, sessMode)
	return WebSocketSessionEx(rawSess, logger, addr, tlsConf, rawSess.GetSenderChannel())
}

func WebSocketSessionEx(rawSess objproto.RawSession, logger *slog.Logger, addr string, tlsConf *tls.Config, sendTo <-chan *objproto.PacketData) (objproto.Session, error) {
	connChan := make(chan *WebSocketConn, 10)
	connMap := &connectionMap{
		connMap: make(map[netip.AddrPort]*WebSocketConn),
	}

	// mutual or server mode listens for incoming connections
	if rawSess.SessionMode() != objproto.SessionModeClient {
		handler := &websocket.Server{
			Config: websocket.Config{
				TlsConfig: tlsConf,
			},
			Handshake: func(c *websocket.Config, r *http.Request) error {
				var err error
				c.Origin, err = websocket.Origin(c, r)
				if err == nil && c.Origin == nil {
					return fmt.Errorf("null origin")
				}
				return err
			},
			Handler: func(ws *websocket.Conn) {
				ctx, cancel := context.WithCancel(ws.Request().Context())
				remoteAddr, err := netip.ParseAddrPort(ws.Request().RemoteAddr)
				if err != nil {
					logger.Error("invalid remote address", slog.String("address", ws.Request().RemoteAddr))
					ws.Close()
					cancel()
					return
				}
				conn := newWebSocketConn(ws, remoteAddr, cancel)
				connMap.Set(remoteAddr, conn)
				connChan <- conn
				<-ctx.Done() // Wait for the context to be done before closing the connection
			},
		}

		httpServer := &http.Server{Addr: addr}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		httpServer.Handler = mux

		go func() {
			if tlsConf != nil {
				httpServer.TLSConfig = tlsConf
				if err := httpServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTPS server failed", "error", err)
				}
			} else {
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTP server failed", "error", err)
				}
			}
		}()
	}

	transportName := "ws"
	if tlsConf != nil {
		transportName = "wss"
	}

	handleRawSession(transportName, connChan, sendTo, rawSess, connMap, logger, tlsConf)

	return rawSess, nil
}
