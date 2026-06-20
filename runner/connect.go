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

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/objtrsf/objproto"
)

// agentBinBase is the basename of the agent binary the runner runs, for peer
// identification over the wire. Empty stays empty (callers treat "" as unknown).
func agentBinBase(claudeBin string) string {
	if claudeBin == "" {
		return ""
	}
	return filepath.Base(claudeBin)
}

// skillsInjected reports whether the runner injects .claude/{settings.json,skills}
// for its tasks. Mirrors the guard in runner/session.go (!NoWorktree ||
// ForceInjectHarnessSettings).
func skillsInjected(noWorktree, forceInject bool) bool {
	return !noWorktree || forceInject
}

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

	// ProxyVia, when non-empty, is propagated into spawned agent env as
	// HARNESS_PROXY_VIA_RUNNER (Phase B). ListenAndServe sets this from its
	// listen addr; dial mode leaves it empty.
	ProxyVia string
}

// RunHandle wraps the live connection + session so Connect and OnConnect can
// be called by separate callers (e.g. cli.PersistLoop).
type RunHandle struct {
	pc      *peer.Conn
	session *Session
	sender  *peerSender
	cfg     Config

	pskRespCh chan protocol.PskAuthResponse
	closeOnce sync.Once
}

func (h *RunHandle) Done() <-chan struct{} { return h.pc.Done() }
func (h *RunHandle) Close() {
	h.closeOnce.Do(func() {
		// Release this connection's remote port-forward listeners before
		// dropping the peer conn. PersistLoop calls Close() on every
		// disconnect and then builds a fresh Session on reconnect, so skipping
		// this leaks the bound listener ports across reconnects.
		if h.session != nil && h.session.rforwards != nil {
			h.session.rforwards.closeAll()
		}
		h.pc.Close()
	})
}

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

	ep, err := buildRunnerEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: cfg.PingInterval, // zero → peer.Dial default (15s post-Task 1)
	})
	if err != nil {
		return nil, err
	}
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		return nil, err
	}
	// Store ep in session so dispatchRunnerRequest can call SetProxy for
	// EstablishRelay without needing the endpoint threaded through every call.
	h.session.Endpoint = ep
	return h, nil
}

// driveAfterConn is the half of Connect that runs after the peer.Conn is
// established (regardless of who dialed). PSK send, session build, and
// handle wrap-up. Returns the RunHandle ready for OnConnect.
func driveAfterConn(ctx context.Context, cfg Config, pc *peer.Conn) (*RunHandle, error) {
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

	// Use the actual peer.Conn's ConnectionID as the server CID so that
	// HARNESS_SERVER_CID injected into spawned agent processes points to
	// the live server endpoint. In dial mode this equals cfg.ServerCID
	// (peer.Dial uses that CID verbatim, including any random-ID
	// resolution done by cliopts.ResolveServerCID). In listen mode
	// cfg.ServerCID is the zero ConnectionID and `pc.Connection().ConnectionID()`
	// is the only source of the server-side identity.
	serverCID := pc.Connection().ConnectionID()

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		AllowedRoots:               cfg.AllowedRoots,
		ClaudeBin:                  cfg.ClaudeBin,
		ExtraClaudeArgs:            cfg.ExtraClaudeArgs,
		ServerCID:                  serverCID,
		Hostname:                   cfg.Hostname,
		WSPath:                     cli.WebSocketPath,
		BinDir:                     binDir,
		PSK:                        psk,
		ProxyVia:                   cfg.ProxyVia,
		Sender:                     sender,
		Streams:                    pc.Transport(),
		creator:                    pc.Transport(),
		Logger:                     cfg.Logger,
		Now:                        time.Now,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
		// Endpoint is set by Connect (dial mode) or handleServerConn (listen
		// mode) after driveAfterConn returns, so the ep is available.
	}

	h := &RunHandle{
		pc:        pc,
		session:   session,
		sender:    sender,
		cfg:       cfg,
		pskRespCh: make(chan protocol.PskAuthResponse, 1),
	}

	// During PSK phase, only route PskAuth responses (brgen-decoded); runner
	// control messages arrive only after the server has accepted the connection.
	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		if kind == appwire.AppKind_PskAuth && len(payload) > 0 {
			var resp protocol.PskAuthResponse
			if _, err := resp.Decode(payload); err == nil {
				select {
				case h.pskRespCh <- resp:
				default:
				}
			}
			return
		}
		// pre-OnConnect: ignore non-PSK payloads.
	})
	pc.Start(ctx)

	// Build and send the merged PSK+identity request (role=runner).
	// The RunnerHello is embedded here so the server's gate can register the
	// runner in one round-trip and reply with both PskAuthResponse{ok} AND
	// RunnerHelloResponse (carrying YourRunnerId). Do NOT use SendMergedHandshake
	// — that is role=client only.
	pskCtx, pskCancel := context.WithCancel(ctx)
	go func() {
		defer pskCancel()
		select {
		case <-pc.Done():
		case <-pskCtx.Done():
		}
	}()
	pskErr := sendRunnerMergedHandshake(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, pc.Connection().GetTranscript(), cfg, h.pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, &cli.PSKAuthError{Err: pskErr}
	}
	return h, nil
}

// buildRunnerHello constructs the RunnerHello from the given Config, identical
// to the message previously sent in OnConnect. It is now embedded in the merged
// PskAuthRequest so the server can register the runner in one round-trip.
func buildRunnerHello(cfg Config) protocol.RunnerHello {
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
	hh.SetAgentBin([]byte(agentBinBase(cfg.ClaudeBin)))
	hh.SetSkillsInjected(skillsInjected(cfg.NoWorktree, cfg.ForceInjectHarnessSettings))
	return hh
}

// sendRunnerMergedHandshake builds a PskAuthRequest{binder (or empty when psk==nil),
// role=runner, runner_hello=<RunnerHello from cfg>}, sends [0x45]+PskAuthRequest via
// sendFn, then blocks until a PskAuthResponse arrives on respCh or ctx is cancelled.
//
// This is the runner-side counterpart to cli.SendMergedHandshake (which is
// role=client). The binder computation (HMAC-SHA512 over the objproto transcript)
// is identical to cli.ComputePSKBinder — only the role and identity union differ.
func sendRunnerMergedHandshake(ctx context.Context, sendFn func([]byte) error, psk, transcript []byte, cfg Config, respCh <-chan protocol.PskAuthResponse) error {
	req := protocol.PskAuthRequest{Role: protocol.AuthRole_Runner}

	if len(psk) > 0 {
		binder, err := cli.ComputePSKBinder(psk, transcript)
		if err != nil {
			return fmt.Errorf("psk: binder: %w", err)
		}
		if !req.SetBinder(binder) {
			return fmt.Errorf("psk: SetBinder failed (len=%d)", len(binder))
		}
	} else {
		req.SetBinder(nil) // binder_len = 0
	}

	rh := buildRunnerHello(cfg)
	if !req.SetRunnerHello(rh) {
		return fmt.Errorf("psk: SetRunnerHello failed")
	}

	data, err := req.Append([]byte{byte(appwire.AppKind_PskAuth)})
	if err != nil {
		return fmt.Errorf("psk: encode: %w", err)
	}
	if err := sendFn(data); err != nil {
		return fmt.Errorf("psk: send: %w", err)
	}

	select {
	case resp := <-respCh:
		switch resp.Status {
		case protocol.PskAuthStatus_Ok:
			return nil
		case protocol.PskAuthStatus_BadPsk:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		case protocol.PskAuthStatus_BadTicket:
			return fmt.Errorf("psk: server rejected agent ticket: %v", resp.Status)
		case protocol.PskAuthStatus_NoIdentity:
			return fmt.Errorf("psk: server rejected (no identity): %v", resp.Status)
		default:
			return fmt.Errorf("psk: server rejected: %v", resp.Status)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// OnConnect performs the post-PSK lifecycle: install the runner-control
// dispatcher rooted at runCtx, and block until the peer connection terminates
// or runCtx is cancelled.
//
// The RunnerHello is now embedded in the merged PskAuthRequest sent during
// driveAfterConn (Connect). The server's gate re-dispatches it to the runner
// handler, which registers the runner and replies with RunnerHelloResponse.
// No separate Hello send is needed here.
func OnConnect(runCtx context.Context, h *RunHandle) error {
	pc := h.pc
	session := h.session
	cfg := h.cfg

	pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
		dispatchRunnerRequest(runCtx, session, cfg.Logger, kind, payload)
	})

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
func dispatchRunnerRequest(ctx context.Context, session *Session, log *slog.Logger, kind appwire.AppKind, payload []byte) {
	if kind != appwire.AppKind_RunnerControl {
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
		// The body (auth_ticket / repo_path / prompt / extra_args) is on
		// a server-initiated send-stream — fetch+decode here so handleAssign
		// receives a fully-resolved request.
		go func() {
			body, err := waitForAssignTaskBody(ctx, session.Streams, trsf.StreamID(at.StreamId))
			if err != nil {
				log.Error("AssignTask body fetch failed",
					"task_id", hex.EncodeToString(at.TaskId.Id[:]),
					"stream_id", at.StreamId,
					"err", err)
				return
			}
			session.handleAssign(ctx, at.TaskId, body)
		}()
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
	case protocol.RunnerRequestType_OpenPortForward:
		pf := req.OpenPortForward()
		if pf == nil {
			return
		}
		go session.handleOpenPortForward(ctx, pf)
	case protocol.RunnerRequestType_ClosePortForward:
		cpf := req.ClosePortForward()
		if cpf == nil {
			return
		}
		session.rforwardListeners().close(cpf.ForwardId)
	case protocol.RunnerRequestType_EstablishRelay:
		er := req.EstablishRelay()
		if er == nil {
			return
		}
		st := &relayHandlerState{serverCID: session.ServerCID}
		handleEstablishRelay(ctx, log, st, session.Endpoint, *er, func(resp protocol.EstablishRelayResponse) error {
			var rm protocol.RunnerMessage
			rm.Kind = protocol.RunnerMessageType_EstablishRelayResponse
			rm.SetEstablishRelayResponse(resp)
			payload := rm.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
			return session.Sender.Send(payload)
		})
	case protocol.RunnerRequestType_ChainedRelayResponse:
		rcr := req.ChainedRelayResponse()
		if rcr == nil {
			log.Error("dispatch: ChainedRelayResponse nil")
			return
		}
		if !session.DeliverChainedRelayResponse(*rcr) {
			log.Warn("dispatch: ChainedRelayResponse without waiter", "status", rcr.Status)
		}
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

// buildRunnerEndpoint constructs an objproto.Endpoint for dial-mode runner.
//
// Mode is Mutual at the objproto layer: the runner dials the server outbound,
// but once the WS / UDP socket is established, incoming Handshake packets
// (from the server at a fresh connection_id) are accepted instead of dropped.
// This lets a dial-mode runner serve as a Phase C relay proxy when the server
// uses it as a --via target. The WS transport stays dial-only (nil mux, no
// HTTP listener registered) — this is the new "Mutual + nil mux" configuration
// that the transport layer accepts.
//
// UDP transport was already symmetric (binds a socket regardless of mode), so
// the mode bump there only affects objproto-level handshake acceptance.
func buildRunnerEndpoint(cfg Config) (objproto.Endpoint, error) {
	switch cfg.ServerCID.Transport {
	case "ws", "wss":
		ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
			Logger: cfg.Logger,
			Path:   cli.WebSocketPath,
			Mode:   objproto.EndpointModeMutual,
		})
		if err != nil {
			return nil, fmt.Errorf("ws endpoint: %w", err)
		}
		return ep, nil
	case "udp":
		ep, err := transport.UDPEndpoint(cfg.Logger, 0, objproto.EndpointModeMutual)
		if err != nil {
			return nil, fmt.Errorf("udp endpoint: %w", err)
		}
		return ep, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q in --server-cid", cfg.ServerCID.Transport)
	}
}

// waitForAssignTaskBody resolves the server-initiated send-stream
// referenced by AssignTask.StreamId, reads the full body to EOF, and
// decodes it as a protocol.AssignTaskBody. Mirrors cli/get_log.go's
// waitForReceiveStream pattern: the trsf stream-creation frame may not
// have arrived by the time the AssignTask envelope is dispatched, so
// we poll Transport.GetReceiveStream briefly before reading.
func waitForAssignTaskBody(ctx context.Context, p peer.BidirectionalStreamLookup, id trsf.StreamID) (*protocol.AssignTaskBody, error) {
	if id == 0 {
		return nil, fmt.Errorf("AssignTask stream_id is 0 (server failed to allocate)")
	}
	st := p.GetReceiveStream(id)
	if st == nil {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
	wait:
		for st == nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("AssignTask stream %d not visible after 2s", id)
			case <-tick.C:
				st = p.GetReceiveStream(id)
				if st != nil {
					break wait
				}
			}
		}
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, err := st.ReadDirect(64 * 1024)
		if err != nil {
			return nil, fmt.Errorf("AssignTask stream %d read: %w", id, err)
		}
		if len(data) > 0 {
			raw = append(raw, data...)
		}
		if eof {
			break
		}
	}
	body := &protocol.AssignTaskBody{}
	if err := body.DecodeExact(raw); err != nil {
		return nil, fmt.Errorf("decode AssignTaskBody (%d bytes): %w", len(raw), err)
	}
	return body, nil
}
