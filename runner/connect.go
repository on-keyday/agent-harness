package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Config holds the configuration for the runner connection.
type Config struct {
	ServerCID       objproto.ConnectionID // server peer ConnectionID (parsed from --server-cid)
	RepoPath        string                // absolute path of the repo this runner serves
	ClaudeBin       string                // path to the claude binary
	ExtraClaudeArgs []string              // forwarded to every claude invocation (before -p)
	Logger          *slog.Logger
}

// Run dials the server, registers via Hello, processes RunnerRequests until
// ctx is done (or the underlying connection closes), and returns the first
// fatal error. The receive loop runs in a goroutine inside peer.Conn; this
// function blocks on pc.Wait so the runner main can simply defer cleanup.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: cfg.Logger,
		Path:   cli.WebSocketPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		return fmt.Errorf("ws endpoint: %w", err)
	}
	pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pc.Close()

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		RepoPath:        cfg.RepoPath,
		ClaudeBin:       cfg.ClaudeBin,
		ExtraClaudeArgs: cfg.ExtraClaudeArgs,
		Sender:          sender,
		Streams:         pc.Transport(),
		Logger:          cfg.Logger,
		Now:             time.Now,
	}

	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind != wire.ApplicationPayloadKind_RunnerControl {
			return // server side never sends TaskControl/Pubsub-other to runners
		}
		req := &protocol.RunnerRequest{}
		if _, derr := req.Decode(payload); derr != nil {
			cfg.Logger.Error("runner_request decode", "err", derr)
			return
		}
		switch req.Kind {
		case protocol.RunnerRequestType_AssignTask:
			at := req.AssignTask()
			if at == nil {
				return
			}
			// Spawn the task handler so the receive loop stays responsive.
			go session.handleAssign(ctx, at)
		case protocol.RunnerRequestType_CancelTask:
			// v1 does not implement runner-side cancel; log and ignore.
			cfg.Logger.Info("runner: cancel not implemented", "kind", req.Kind)
		case protocol.RunnerRequestType_OpenExec:
			oer := req.OpenExec()
			if oer == nil {
				return
			}
			go session.handleOpenExec(ctx, oer)
		}
	})
	pc.Start(ctx)

	// Send Hello — the server's registry uses this to bind ConnectionID → repo.
	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	h := protocol.RunnerHello{Version: 1}
	h.SetRepoPath([]byte(cfg.RepoPath))
	hello.SetHello(h)
	helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err := sender.Send(helloBytes); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}

	return pc.Wait(ctx)
}

// peerSender adapts *peer.Conn to the runner.Sender interface so existing
// session code (and its tests via mockSender) doesn't have to know about
// peer at all. Send writes raw control bytes through the objproto connection;
// Publish goes through peer.Conn.Publish (per-topic singleflight + cached
// stream — replaces the old connSender, which lived alongside this file).
type peerSender struct {
	pc  *peer.Conn
	ctx context.Context
}

func (s *peerSender) Send(data []byte) error {
	_, _, err := s.pc.Connection().SendMessage(data)
	return err
}

func (s *peerSender) ID() objproto.ConnectionID {
	return s.pc.Connection().ConnectionID()
}

func (s *peerSender) Publish(topic string, data []byte) error {
	return s.pc.Publish(s.ctx, "runner", topic, data)
}
