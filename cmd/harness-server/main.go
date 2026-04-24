package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

var port = flag.String("port", "8539", "port number of server")

func main() {
	// slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	sess, err := transport.WebSocketSession(slog.Default(), fmt.Sprintf("localhost:%s", *port), nil, objproto.SessionModeServer)
	if err != nil {
		slog.Error("failed to start WebSocket session", "error", err)
		return
	}
	slog.Info("WebSocket session started", "port", *port)

	go objproto.AutoGarbageCollect(sess, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pubSub := pubsub.NewPubSub(slog.Default())

	activeSessChan := sess.GetNewActiveSessionChannel()
	for session := range activeSessChan {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := trsf.NewStreams(ctx, true, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, session, slog.Default())
			subscriber := pubsub.NewSubscriber(session.ConnectionID(), p)
			defer subscriber.LeaveAll(pubSub)
			go trsf.AutoSend(ctx, p, session, func(err error) {
				if err != nil {
					slog.Error("failed to send data", "error", err)
				}
			})
			trsf.AutoReceive(ctx, p, session, func(msg *objproto.Message, err error) {
				if err != nil {
					slog.Error("failed to receive data", "error", err)
					return
				}
				slog.Info("message received", "data", string(msg.Data))
				kind := wire.ApplicationPayloadKind(msg.Data[0])
				switch kind {
				case wire.ApplicationPayloadKind_Pubsub:
					slog.Info("control message received", "data", string(msg.Data[1:]))
					response := subscriber.HandleMessage(pubSub, msg.Data[1:])
					if response != nil {
						_, _, err := session.SendMessage(response)
						if err != nil {
							slog.Error("failed to send control response", "error", err)
						}
					}
				case wire.ApplicationPayloadKind_TaskControl:

				case wire.ApplicationPayloadKind_RelayControl:

				case wire.ApplicationPayloadKind_RunnerControl:

				}
			})
		}()
	}
}
