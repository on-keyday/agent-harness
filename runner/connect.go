package runner

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	AllowedRoots    []string              // absolute repo paths (or root prefixes) this runner serves
	MaxTasks        int                   // maximum concurrent tasks (0 → defaults to 1)
	Hostname        string                // hostname reported in Hello (empty → no hostname sent)
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

	// Resolve the runner binary's directory so we can prepend it to the
	// agent's PATH. Errors are non-fatal: the agent simply won't have
	// harness-cli on its PATH (legacy behaviour).
	var binDir string
	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)
	} else {
		cfg.Logger.Warn("os.Executable failed; agent PATH will not include runner bin dir", "err", err)
	}

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		AllowedRoots:    cfg.AllowedRoots,
		ClaudeBin:       cfg.ClaudeBin,
		ExtraClaudeArgs: cfg.ExtraClaudeArgs,
		ServerCID:       cfg.ServerCID,
		Hostname:        cfg.Hostname,
		WSPath:          cli.WebSocketPath,
		BinDir:          binDir,
		Sender:          sender,
		Streams:         pc.Transport(),
		Logger:          cfg.Logger,
		Now:             time.Now,
	}

	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		dispatchRunnerRequest(ctx, session, cfg.Logger, kind, payload)
	})
	pc.Start(ctx)

	// Build and send Hello — the server's registry uses this to bind
	// ConnectionID → allowed_roots / max_tasks / hostname.
	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	h := protocol.RunnerHello{Version: 1}

	maxTasks := cfg.MaxTasks
	if maxTasks < 1 {
		maxTasks = 1
	}
	h.MaxTasks = uint16(maxTasks)

	if cfg.Hostname != "" {
		h.SetHostname([]byte(cfg.Hostname))
	}

	roots := make([]protocol.AllowedRoot, 0, len(cfg.AllowedRoots))
	for _, r := range cfg.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(r))
		roots = append(roots, ar)
	}
	h.SetAllowedRoots(roots)

	hello.SetHello(h)
	helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err := sender.Send(helloBytes); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}

	return pc.Wait(ctx)
}

// dispatchRunnerRequest decodes an inbound control payload and dispatches it to
// the appropriate session handler. Extracted from the OnControl closure so that
// tests can call it directly without a live peer connection.
func dispatchRunnerRequest(ctx context.Context, session *Session, log *slog.Logger, kind wire.ApplicationPayloadKind, payload []byte) {
	if kind != wire.ApplicationPayloadKind_RunnerControl {
		return // server side never sends TaskControl/Pubsub-other to runners
	}
	req := &protocol.RunnerRequest{}
	if _, derr := req.Decode(payload); derr != nil {
		log.Error("runner_request decode", "err", derr)
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
		ct := req.CancelTask()
		if ct == nil {
			return
		}
		taskIDHex := hex.EncodeToString(ct.TaskId.Id[:])
		session.mu.Lock()
		te, ok := session.tasks[taskIDHex]
		session.mu.Unlock()
		if ok {
			te.cancel()
		} else {
			log.Info("runner: cancel for unknown task", "task_id", taskIDHex)
		}
	case protocol.RunnerRequestType_OpenExec:
		oer := req.OpenExec()
		if oer == nil {
			return
		}
		go session.handleOpenExec(ctx, oer)
	}
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
