package main

import (
	"context"
	"crypto/ecdh"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

var port = flag.String("port", "8539", "port number of server")
var topic = flag.String("topic", "sample/talk", "topic to subscribe")

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	flag.Parse()
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

	p := trsf.NewStreams(context.Background(), false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	go trsf.AutoSend(context.Background(), p, conn, nil)
	// go trsf.AutoPing(context.Background(), conn, 15*time.Second)
	go trsf.AutoReceive(context.Background(), p, conn, func(msg *objproto.Message, err error) {
		if err != nil {
			slog.Error("failed to receive data", "error", err)
			return
		}
		if len(msg.Data) == 0 {
			return
		}
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_Control {
			resp := &protocol.PubSubResponse{}
			resp.Decode(msg.Data[1:])
			slog.Info("control response received", "status", resp.Status, "streamID", resp.StreamId)
		}
	})

	request := pubsub.JoinTopic(*topic)
	_, _, err = conn.SendMessage(request)
	if err != nil {
		slog.Error("failed to send join topic request", "error", err)
		return
	}

	for {
		stream, err := p.AcceptBidirectionalStream(context.Background())
		if err != nil {
			slog.Error("failed to accept bidirectional stream", "error", err)
			return
		}
		slog.Info("bidirectional stream accepted", "streamID", stream.ID())
		go func() {
			// stdin reader
			for {
				var input string
				fmt.Print("Enter message to publish (or 'exit' to quit): ")
				_, err := fmt.Scanln(&input)
				if err != nil {
					slog.Error("failed to read input", "error", err)
					continue
				}
				if input == "exit" {
					slog.Info("exiting...")
					return
				}
				err = stream.AppendData(false, []byte(input))
				if err != nil {
					slog.Error("failed to write to stream", "error", err)
					return
				}
			}
		}()
		go func() {
			for {
				data, eof, err := stream.ReadDirect(trsf.InitialFlowWindow)
				if err != nil {
					slog.Error("failed to read from stream", "error", err)
					return
				}
				slog.Info("data received from stream", "length", len(data), "eof", eof)
				if len(data) != 0 {
					slog.Info("message from topic", "topic", "sample/talk", "message", string(data))
				}
				if eof {
					slog.Info("stream closed by remote", "streamID", stream.ID())
					return
				}
			}
		}()
	}
}
