package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

var port = flag.String("port", "8539", "port number of server")

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	sess, err := transport.WebSocketSession(slog.Default(), fmt.Sprintf("localhost:%s", *port), nil, objproto.SessionModeServer)
	if err != nil {
		slog.Error("failed to start WebSocket session", "error", err)
		return
	}
	slog.Info("WebSocket session started", "port", *port)

	go objproto.AutoGarbageCollect(sess, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	activeSessChan := sess.GetNewActiveSessionChannel()
	for session := range activeSessChan {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := trsf.NewStreams(ctx, true, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, session, slog.Default())
			go trsf.AutoSend(ctx, p, session, func(err error) {
				if err != nil {
					slog.Error("failed to send data", "error", err)
				}
			})
			go func() {
				for {
					stream, err := p.AcceptBidirectionalStream(ctx)
					if err != nil {
						slog.Error("failed to accept bidirectional stream", "error", err)
						return
					}
					slog.Info("accepted new bidirectional stream", "stream_id", stream.ID())
					go func() {
						for {
							var buffer [4096]byte
							n, err := stream.ReadContext(ctx, buffer[:])
							if err != nil {
								slog.Error("failed to read from stream", "error", err)
								return
							}
							slog.Info("received data from stream", "length", n)
							_, err = stream.WriteContext(ctx, buffer[:n])
							if err != nil {
								slog.Error("failed to write to stream", "error", err)
								return
							}
							slog.Info("echoed data back to stream", "length", n)
						}
					}()
				}
			}()
			for {
				msg, err := session.ReceiveMessage()
				if err != nil {
					slog.Error("failed to receive message", "error", err)
					return
				}
				if len(msg.Data) == 0 {
					slog.Info("received empty message, closing session")
					return
				}
				if wire.IsStreamRelated(wire.ApplicationPayloadKind(msg.Data[0])) {
					p.Send(msg)
					continue
				}
				_, _, err = session.SendMessage(msg.Data)
				if err != nil {
					slog.Error("failed to send message", "error", err)
					return
				}
				slog.Info("sent message", "data", string(msg.Data))
			}
		}()
	}
}
