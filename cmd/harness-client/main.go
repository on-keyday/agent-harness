package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

var port = flag.String("port", "8539", "port number of server")
var topic = flag.String("topic", "sample/talk", "topic to subscribe")
var nickname = flag.String("nickname", "client1", "nickname for pubsub")

func main() {
	//slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
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
	go trsf.AutoPing(context.Background(), conn, 15*time.Second)
	go trsf.AutoReceive(context.Background(), p, conn, func(msg *objproto.Message, err error) {
		if err != nil {
			slog.Error("failed to receive data", "error", err)
			return
		}
		if len(msg.Data) == 0 {
			return
		}
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_Pubsub {
			resp := &protocol.PubSubResponse{}
			resp.Decode(msg.Data[1:])
			slog.Info("control response received", "status", resp.Status, "streamID", resp.StreamId)
		}
	})

	request := pubsub.JoinTopic(*nickname, *topic)
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
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Split(bufio.ScanLines)
			for {
				var input string
				if !scanner.Scan() {
					err := scanner.Err()
					if err != nil {
						slog.Error("failed to read input", "error", err)
					}
					return
				}
				input = scanner.Text()
				if input == "" {
					continue
				}
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
				fmt.Printf("\n%s", string(data))
				if eof {
					slog.Info("stream closed by remote", "streamID", stream.ID())
					return
				}
			}
		}()
	}
}
