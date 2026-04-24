package main

import (
	"context"
	"crypto/ecdh"
	"flag"
	"fmt"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
)

var port = flag.String("port", "8539", "port number of server")

func main() {
	sess, err := transport.WebSocketSession(slog.Default(), fmt.Sprintf("localhost:%s", *port), nil, objproto.SessionModeClient)
	if err != nil {
		slog.Error("failed to start WebSocket session", "error", err)
		return
	}
	slog.Info("WebSocket session started", "port", *port)

	conn, err := objproto.DoECDHHandshake(context.Background(), sess, objproto.MustParseConnectionID("ws:127.0.0.1:8539-1111"), ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		slog.Error("failed to perform ECDH handshake", "error", err)
		return
	}
	slog.Info("ECDH handshake completed", "remoteAddr", conn.ConnectionID())

	_, _, err = conn.SendMessage([]byte("\x00Hello, client!"))
	if err != nil {
		slog.Error("failed to send message", "error", err)
		return
	}
	slog.Info("message sent", "data", string([]byte("\x00Hello, client!")))
	msg, err := conn.ReceiveMessageContext(context.Background())
	if err != nil {
		slog.Error("failed to receive message", "error", err)
		return
	}
	slog.Info("message received", "data", string(msg.Data))

	p := trsf.NewStreams(context.Background(), false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	go trsf.AutoSend(context.Background(), p, conn, nil)
	go trsf.AutoReceive(context.Background(), p, conn, func(msg *objproto.Message, err error) {
		if err != nil {
			slog.Error("failed to receive data", "error", err)
			return
		}
	})

	bs := p.CreateBidirectionalStream()

	err = bs.AppendData(true, []byte("Hey what's up?"))
	if err != nil {
		slog.Error("failed to write to stream", "error", err)
		return
	}
	slog.Info("data written to stream", "data", "Hey what's up?")

	var buffer [4096]byte
	n, err := bs.ReadContext(context.Background(), buffer[:])
	if err != nil {
		slog.Error("failed to read from stream", "error", err)
		return
	}
	slog.Info("data read from stream", "data", string(buffer[:n]))
}
