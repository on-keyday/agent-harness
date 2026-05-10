package runner

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	// PSK, when non-nil, overrides the HARNESS_PSK / HARNESS_PSK_FILE env vars.
	PSK []byte

	// NoWorktree disables the per-task git worktree creation. Tasks run with
	// cwd = AssignTask.RepoPath. Settings/skills injection and worktree
	// cleanup are skipped by default. See spec
	// docs/superpowers/specs/2026-05-08-runner-no-worktree-mode-design.md.
	NoWorktree bool

	// ForceInjectHarnessSettings is only meaningful with NoWorktree=true:
	// it re-enables WriteAgentSettings / WriteAgentSkills (target = RepoPath).
	// Worktree cleanup remains disabled in NoWorktree mode regardless.
	ForceInjectHarnessSettings bool

	// PingInterval overrides peer.DialConfig.PingInterval (default 15s).
	PingInterval time.Duration
}

// RunHandle wraps the live connection + session so Connect and OnConnect can
// be called by separate callers (e.g. cli.PersistLoop).
type RunHandle struct {
	pc      *peer.Conn
	session *Session
	sender  *peerSender
	cfg     Config

	pskRespCh chan wire.PskAuthStatus
	closeOnce sync.Once
}

func (h *RunHandle) Done() <-chan struct{} { return h.pc.Done() }
func (h *RunHandle) Close()                { h.closeOnce.Do(func() { h.pc.Close() }) }

// Connect performs the WS dial, ECDH handshake, PSK exchange, and session
// scaffolding. The caller drives the rest of the lifecycle via OnConnect.
//
// Returns *cli.PSKAuthError when the server rejects the PSK so PersistLoop
// can treat it as fatal.
func Connect(ctx context.Context, cfg Config) (*RunHandle, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Logger.Info("runner config",
		"no_worktree", cfg.NoWorktree,
		"force_inject_harness_settings", cfg.ForceInjectHarnessSettings)

	ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
		Logger: cfg.Logger,
		Path:   cli.WebSocketPath,
		Mode:   objproto.EndpointModeClient,
	})
	if err != nil {
		return nil, fmt.Errorf("ws endpoint: %w", err)
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: cfg.PingInterval, // zero → peer.Dial default (15s post-Task 1)
	})
	if err != nil {
		return nil, err
	}

	// Resolve the runner binary's directory so we can prepend it to the
	// agent's PATH. Errors are non-fatal: the agent simply won't have
	// harness-cli on its PATH (legacy behaviour).
	var binDir string
	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)
	} else {
		cfg.Logger.Warn("os.Executable failed; agent PATH will not include runner bin dir", "err", err)
	}

	psk := cfg.PSK
	if psk == nil {
		psk = cli.GetPSK()
	}

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		AllowedRoots:               cfg.AllowedRoots,
		ClaudeBin:                  cfg.ClaudeBin,
		ExtraClaudeArgs:            cfg.ExtraClaudeArgs,
		ServerCID:                  cfg.ServerCID,
		Hostname:                   cfg.Hostname,
		WSPath:                     cli.WebSocketPath,
		BinDir:                     binDir,
		PSK:                        psk,
		Sender:                     sender,
		Streams:                    pc.Transport(),
		Logger:                     cfg.Logger,
		Now:                        time.Now,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
	}

	h := &RunHandle{
		pc:        pc,
		session:   session,
		sender:    sender,
		cfg:       cfg,
		pskRespCh: make(chan wire.PskAuthStatus, 1),
	}

	// During PSK phase, only route PskAuth responses; runner control messages
	// arrive only after the server has accepted the connection.
	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind == wire.ApplicationPayloadKind_PskAuth && len(payload) > 0 {
			select {
			case h.pskRespCh <- wire.PskAuthStatus(payload[0]):
			default:
			}
			return
		}
		// pre-OnConnect: ignore non-PSK payloads.
	})
	pc.Start(ctx)

	pskCtx, pskCancel := context.WithCancel(ctx)
	go func() {
		defer pskCancel()
		select {
		case <-pc.Done():
		case <-pskCtx.Done():
		}
	}()
	pskErr := cli.SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, h.pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, &cli.PSKAuthError{Err: pskErr}
	}
	return h, nil
}

// OnConnect performs the post-PSK lifecycle: install the runner-control
// dispatcher rooted at runCtx, send Hello, and block until the peer
// connection terminates or runCtx is cancelled.
func OnConnect(runCtx context.Context, h *RunHandle) error {
	pc := h.pc
	session := h.session
	cfg := h.cfg

	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		dispatchRunnerRequest(runCtx, session, cfg.Logger, kind, payload)
	})

	// Build and send Hello — the server's registry uses this to bind
	// ConnectionID → allowed_roots / max_tasks / hostname.
	hello := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
	hh := protocol.RunnerHello{Version: 1}

	maxTasks := cfg.MaxTasks
	if maxTasks < 1 {
		maxTasks = 1
	}
	hh.MaxTasks = uint16(maxTasks)
	if cfg.Hostname != "" {
		hh.SetHostname([]byte(cfg.Hostname))
	}
	roots := make([]protocol.AllowedRoot, 0, len(cfg.AllowedRoots))
	for _, r := range cfg.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(r))
		roots = append(roots, ar)
	}
	hh.SetAllowedRoots(roots)
	hello.SetHello(hh)
	helloBytes := hello.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err := h.sender.Send(helloBytes); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}

	// Block until either the connection dies or the run is cancelled.
	select {
	case <-pc.Done():
		return nil
	case <-runCtx.Done():
		return nil
	}
}

// Run is the legacy single-shot entry point used by tests and by the shim in
// agent-runner main when persist=false. Sequential Connect → OnConnect.
func Run(ctx context.Context, cfg Config) error {
	h, err := Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()
	return OnConnect(ctx, h)
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
	case protocol.RunnerRequestType_RunnerHelloResponse:
		// Stored synchronously: peer.Conn delivers messages serially, so
		// by the time the next AssignTask is dispatched, this field is set.
		rhr := req.RunnerHelloResponse()
		if rhr == nil {
			return
		}
		session.SetRunnerCanonicalID(rhr.YourRunnerId)
	case protocol.RunnerRequestType_TaskWake:
		tw := req.TaskWake()
		if tw == nil {
			break
		}
		taskIDHex := hex.EncodeToString(tw.TaskId.Id[:])
		session.WakeStdin(taskIDHex)
	case protocol.RunnerRequestType_OpenFileTransfer:
		oft := req.OpenFileTransfer()
		if oft == nil {
			return
		}
		go session.handleOpenFileTransfer(ctx, oft)
	case protocol.RunnerRequestType_ListFiles:
		lf := req.ListFiles()
		if lf == nil {
			return
		}
		go session.handleListFiles(ctx, lf)
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

