package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/transport"
	"github.com/on-keyday/objtrsf/trsf"
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
	// ServerCandidates are the server addresses this runner dials, tried in
	// order until one answers. Empty in listen mode, where the server dials the
	// runner and there is no address to try. Held as text so every attempt
	// re-resolves — see the type's comment for why that is load-bearing.
	ServerCandidates ServerCandidates

	// RunnerID identifies this runner PROCESS, and must be minted ONCE by the
	// caller before PersistLoop starts — NewRunnerID does it. Minting it per
	// connection instead would silently undo the whole point: the server would
	// see a different runner on every reconnect, agent credentials would keep
	// expiring exactly as they used to, and nothing would look broken.
	//
	// A restart SHOULD change it: the value differing is what tells the server
	// the children this runner had are gone.
	RunnerID protocol.RunnerID

	AllowedRoots []string // absolute repo paths (or root prefixes) this runner serves
	MaxTasks     int      // maximum concurrent tasks (0 → defaults to 1)
	Hostname     string   // hostname reported in Hello (empty → no hostname sent)

	// Profiles is the set of agent launch profiles this runner exec's: the
	// default profile (basename becomes RunnerHello.agent_bin) plus any
	// extra profiles (advertised as RunnerHello.agent_profiles). See
	// runner/agent_profile.go and buildRunnerHello.
	Profiles ProfileSet

	Logger *slog.Logger
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

	// AgentSkillsFS, when non-nil, replaces the embedded skill tree as the
	// source WriteAgentSkills copies into each task worktree. agent-runner sets
	// it from --agentskills-dir (an os.DirFS). Since it is read per task assign
	// rather than at process start, an edited SKILL.md reaches the next task
	// without restarting the runner — which the embedded copy cannot do, a
	// running process keeping its loaded binary across `make build`.
	AgentSkillsFS fs.FS

	// PingInterval overrides peer.DialConfig.PingInterval (default 15s).
	PingInterval time.Duration

	// Tasks is this runner PROCESS's task registry, and supplying it is what
	// enables holding tasks across a deliberate server restart. Like RunnerID
	// it must be minted ONCE by the caller, before PersistLoop starts: a
	// registry made per connection would take every held child's cancel func
	// with it when the link drops, which is the exact thing a hold prevents.
	// nil is fine and means "no holds" — Session then makes its own per
	// connection and everything behaves as it did before.
	Tasks *TaskRegistry

	// ProcessCtx is the context task goroutines are rooted at when Tasks is
	// supplied. It must OUTLIVE any single connection, or a disconnect
	// cancels every held child by parentage and the hold is inert. The
	// disconnect decision becomes explicit instead: see
	// Session.cancelTasksUnlessHeld.
	ProcessCtx context.Context

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

	// Control-message handling spans two phases. During the merged handshake
	// (Connect) the server re-dispatches the embedded RunnerHello and replies
	// RunnerHelloResponse — and may AssignTask — BEFORE OnConnect installs the
	// runner-control dispatcher. The persistent handler set in Connect buffers
	// such non-PSK messages here while ctlDispatch is nil, then OnConnect sets
	// ctlDispatch and replays the buffer. Dropping these (as the old
	// "ignore non-PSK" handler did) loses the canonical RunnerID, so spawned
	// agents inherit HARNESS_RUNNER_ID=":invalid AddrPort-0".
	ctlMu       sync.Mutex
	ctlDispatch func(appwire.AppKind, []byte)
	ctlBuf      []bufferedControl
}

// bufferedControl is a runner-control message captured during the handshake
// window for replay once OnConnect installs the dispatcher.
type bufferedControl struct {
	kind    appwire.AppKind
	payload []byte
}

// bufferOrDispatch routes a runner-control message: buffered while the
// dispatcher is not yet active (the merged-handshake window), otherwise live.
// Never drops — losing RunnerHelloResponse here leaves the canonical RunnerID
// zero and spawned agents inherit HARNESS_RUNNER_ID=":invalid AddrPort-0".
func (h *RunHandle) bufferOrDispatch(kind appwire.AppKind, payload []byte) {
	h.ctlMu.Lock()
	d := h.ctlDispatch
	if d == nil {
		h.ctlBuf = append(h.ctlBuf, bufferedControl{kind: kind, payload: append([]byte(nil), payload...)})
		h.ctlMu.Unlock()
		return
	}
	h.ctlMu.Unlock()
	d(kind, payload)
}

// activateDispatch installs the runner-control dispatcher and replays any
// messages buffered during the handshake window, in order. The replay holds
// ctlMu so a concurrently-arriving live message is ordered after the buffered
// ones and none is lost during the transition.
func (h *RunHandle) activateDispatch(d func(appwire.AppKind, []byte)) {
	h.ctlMu.Lock()
	h.ctlDispatch = d
	for _, m := range h.ctlBuf {
		d(m.kind, m.payload)
	}
	h.ctlBuf = nil
	h.ctlMu.Unlock()
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
// It walks cfg.ServerCandidates in order and returns on the first one that
// answers. PersistLoop calls this again on every reconnect, so the walk starts
// from the top each time: a runner that failed over to a second address goes
// back to the first one as soon as that address works again.
//
// Returns *cli.PSKAuthError when the server rejects the PSK so PersistLoop
// can treat it as fatal.
func Connect(ctx context.Context, cfg Config) (*RunHandle, error) {
	clearInheritedCtrlCIgnore()
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Logger.Info("runner config",
		"no_worktree", cfg.NoWorktree,
		"force_inject_harness_settings", cfg.ForceInjectHarnessSettings)

	if cfg.ServerCandidates.Len() == 0 {
		return nil, errors.New("runner: no --server-cid candidate to dial")
	}

	// One endpoint per TRANSPORT rather than per candidate. Nothing closes an
	// objproto.Endpoint — the interface has no Close, and AutoGarbageCollect /
	// AutoKeyUpdate run on tickers with no stop — so each one built here
	// outlives the attempt that built it. PersistLoop already pays that once
	// per reconnect; building one per candidate would multiply it by the length
	// of the list on a runner that reconnects all day. The pair this feature
	// exists for (a LAN address and a tailnet one, both ws) shares a single
	// endpoint, so the common case costs exactly what it costs today.
	eps := map[string]objproto.Endpoint{}

	var lastErr error
	all := cfg.ServerCandidates.All()
	for i, spec := range all {
		h, err := dialServerCandidate(ctx, cfg, eps, spec)
		if err == nil {
			return h, nil
		}
		// A credential the server refused is not an address problem. Walking on
		// would turn "wrong PSK" into a slow trip down the list and then a
		// reconnect loop, when driveAfterConn has already classified it as the
		// one failure no retry can fix.
		var fatal *cli.PSKAuthError
		if errors.As(err, &fatal) {
			return nil, err
		}
		lastErr = err
		// Logged per candidate, with its place in the list, because a single
		// returned error cannot say which addresses were tried. Silent on a
		// one-entry list: there the returned error IS the whole story, and
		// PersistLoop already reports it.
		if len(all) > 1 {
			cfg.Logger.Warn("server candidate did not answer",
				"candidate", spec, "position", fmt.Sprintf("%d/%d", i+1, len(all)), "err", err)
		}
	}
	return nil, lastErr
}

// dialServerCandidate resolves one candidate and runs the whole establish on
// it. Split out of Connect so the candidate walk above reads as the policy it
// is, and so each attempt's endpoint bookkeeping stays in one place.
func dialServerCandidate(ctx context.Context, cfg Config, eps map[string]objproto.Endpoint, spec string) (*RunHandle, error) {
	// DNS runs HERE, on every attempt. A name that resolves only on the home
	// LAN must be allowed to fail now and succeed later (and the other way
	// round) without the runner having to restart.
	cid, err := ResolveServerCandidate(spec)
	if err != nil {
		return nil, err
	}
	ep, ok := eps[cid.Transport]
	if !ok {
		ep, err = buildRunnerEndpoint(cfg, cid.Transport)
		if err != nil {
			return nil, err
		}
		go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)
		go objproto.AutoKeyUpdate(ep, 1*time.Minute, objproto.DefaultKeyUpdateInterval)
		eps[cid.Transport] = ep
	}

	// No deadline of our own: objproto bounds the ECDH handshake at 10s
	// (DoECDHHandshake's WaitWithTimeout), which is what makes the walk
	// terminate. A context deadline here could not do the job anyway — peer.Dial
	// hands its ctx to the CONNECTION, so a timeout would kill the established
	// session ten seconds in rather than cut the dial short.
	pc, err := peer.Dial(ctx, ep, cid, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: cfg.PingInterval, // zero → peer.Dial default (15s post-Task 1)
	})
	if err != nil {
		return nil, err
	}
	// driveAfterConn closes pc itself on failure, so a candidate that hands back
	// an error leaves nothing behind for the next one to trip over.
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		return nil, err
	}
	// Store ep in session so dispatchRunnerRequest can call SetProxy for
	// EstablishRelay without needing the endpoint threaded through every call.
	h.session.Endpoint = ep

	// A dial-mode runner's endpoint is EndpointModeMutual on a bound socket
	// (buildRunnerEndpoint), so objproto will complete a handshake that arrives
	// on it — but until now nothing read the accept channel, so a connection
	// the server forwards here would exist at the objproto layer and be
	// serviced by nobody. Listen mode has had this loop all along; this is the
	// same one.
	startAcceptLoop(ctx, cfg, ep, &lastListenSession, h.session)
	return h, nil
}

// startAcceptLoop services connections that arrive at a dial-mode runner's own
// socket. sessionRef is published so the data-plane handler can find the live
// server session the grants were pushed to.
func startAcceptLoop(ctx context.Context, cfg Config, ep objproto.Endpoint, sessionRef *atomic.Pointer[Session], sess *Session) {
	sessionRef.Store(sess)
	connCh := ep.GetNewActiveConnectionChannel()
	go func() {
		defer sessionRef.CompareAndSwap(sess, nil)
		for {
			select {
			case <-ctx.Done():
				return
			case conn, ok := <-connCh:
				if !ok {
					return
				}
				pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
					Logger:       cfg.Logger,
					PingInterval: cfg.PingInterval,
					// Sized before the hello is read, because the trsf layer is
					// built here: a forwarded connection may join two different
					// transports, and the server put the size it picked next to
					// the grant, findable by this connection's slot.
					MTU: dataPlaneMTUFor(sessionRef, conn.ConnectionID()),
				})
				go handleAcceptedConn(ctx, cfg, sessionRef, ep, pc)
			}
		}
	}()
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
	// the live server endpoint. In dial mode this is the candidate that
	// ANSWERED, already resolved — which is what keeps the --server-cid
	// candidate list off every agent: harness-cli takes one address, and the
	// podman wrapper splits this value into ip/proto/port for its firewall
	// carve-out. In listen mode there is no candidate at all and
	// `pc.Connection().ConnectionID()` is the only source of the server-side
	// identity.
	serverCID := pc.Connection().ConnectionID()

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		reg:            cfg.Tasks,
		processCtx:     cfg.ProcessCtx,
		AllowedRoots:   cfg.AllowedRoots,
		Profiles:       cfg.Profiles,
		ServerCID:      serverCID,
		Hostname:       cfg.Hostname,
		MintedRunnerID: cfg.RunnerID,
		WSPath:         cli.WebSocketPath,
		BinDir:         binDir,
		PSK:            psk,
		ProxyVia:       cfg.ProxyVia,
		Sender:         sender,
		Streams:        pc.Transport(),
		creator:        pc.Transport(),
		// The uplink is the connection every task shares. Registered here
		// because this is where it becomes the Session's, and it is the end
		// that SENDS every pull -- the state nobody could see before.
		Logger:                     cfg.Logger,
		Now:                        time.Now,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
		AgentSkillsFS:              cfg.AgentSkillsFS,
		// Endpoint is set by Connect (dial mode) or handleServerConn (listen
		// mode) after driveAfterConn returns, so the ep is available.
	}
	// The registry outlives this Session, so a held task's output can find
	// whatever connection is current instead of the one it started on.
	cfg.Tasks.setSender(sender)

	// The uplink: one connection multiplexing every task on this runner, and the
	// end that SENDS every pull. Its congestion state was unobservable before
	// this -- the server's dump only ever saw the server's own side.
	session.registerTrsfConn(pc.Connection().ConnectionID().String(),
		protocol.ConnRole_Server, pc.Transport(), protocol.TaskID{})

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
		// Non-PSK (runner-control) may arrive DURING the merged handshake — the
		// server replies RunnerHelloResponse (and may AssignTask) right after
		// PskAuthResponse, before OnConnect installs the dispatcher. Buffer until
		// then (replayed by OnConnect); never drop, or the canonical RunnerID is
		// lost.
		h.bufferOrDispatch(kind, payload)
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
		// Only a NON-retryable rejection is fatal — a wrong PSK/ticket won't fix
		// itself. Everything else must be RETRYABLE so PersistLoop reconnects
		// instead of killing the runner — otherwise a routine server restart, or
		// a server that is briefly behind us on the wire schema, wipes the whole
		// fleet. Both cases are real: a transport drop mid-handshake, and a
		// NoIdentity rejection from a version-skewed server that cannot decode
		// our RunnerHello (see cli.PskRejectedError.Retryable).
		var rej *cli.PskRejectedError
		if errors.As(pskErr, &rej) && !rej.Retryable() {
			return nil, &cli.PSKAuthError{Err: pskErr}
		}
		return nil, pskErr
	}
	return h, nil
}

// buildRunnerHello constructs the RunnerHello from the given Config, identical
// to the message previously sent in OnConnect. It is now embedded in the merged
// PskAuthRequest so the server can register the runner in one round-trip.
func buildRunnerHello(cfg Config) protocol.RunnerHello {
	hh := protocol.RunnerHello{Version: 1, RunnerId: cfg.RunnerID}
	// What this process is still holding from a previous server's shutdown.
	// Empty for every ordinary reconnect. It rides the hello so the server can
	// register and re-adopt in one step, rather than holding a window open
	// before it may fail held tasks.
	hh.Held = cfg.Tasks.heldReport()
	// The harness had no idea what OS a runner ran on, and the ssh gateway was
	// guessing (`sh -c` for every command, on every platform). Reported once at
	// hello time; it cannot change without a reconnect.
	hh.SetGoos([]byte(runtime.GOOS))
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
	// agent_bin advertises the default profile's basename (empty ProfileSet →
	// Resolve("") errors and the zero-value AgentProfile{} yields "", the same
	// as an unset --agent-bin previously did).
	defaultProfile, _ := cfg.Profiles.Resolve("")
	hh.SetAgentBin([]byte(agentBinBase(defaultProfile.Bin)))
	names := cfg.Profiles.Names()
	profileNames := make([]protocol.AgentProfileName, 0, len(names))
	for _, n := range names {
		var pn protocol.AgentProfileName
		pn.SetName([]byte(n))
		profileNames = append(profileNames, pn)
	}
	hh.SetAgentProfiles(profileNames)
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
		if resp.Status == protocol.PskAuthStatus_Ok {
			return nil
		}
		// Explicit server rejection. Connect wraps it as *cli.PSKAuthError (fatal)
		// ONLY when !Retryable() — so Code MUST be set: a zero Code reads as
		// PskAuthStatus_Ok, makes Retryable() false, and silently restores the
		// fatal-on-wire-skew behaviour that wiped the fleet on 2026-07-16.
		// (The runner sends a RunnerHello, so it has its own handshake here and
		// does NOT go through cli.SendMergedHandshake — this is a third creation
		// site, easy to miss when grepping only cli/.)
		return cli.NewPskRejectedError(resp.Status)
	case <-ctx.Done():
		// Transport drop / cancellation mid-handshake — RETRYABLE: a server
		// restart that interrupts the handshake must trigger reconnect, not exit.
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

	// Activate the runner-control dispatcher and replay any control messages
	// buffered during the handshake window (RunnerHelloResponse / early
	// AssignTask). The persistent handler installed in Connect keeps routing
	// PskAuth and, once ctlDispatch is set, routes runner-control here.
	h.activateDispatch(func(kind appwire.AppKind, payload []byte) {
		dispatchRunnerRequest(runCtx, session, cfg.Logger, kind, payload)
	})

	// Block until either the connection dies or the run is cancelled, then
	// make the ONE decision this design turns from a side effect into a
	// statement: with a hold armed the children stay, otherwise they go.
	//
	// Task contexts used to descend from runCtx, so a dropped link cancelled
	// them by parentage. They now descend from the process, which means
	// nothing happens here unless it is written here.
	select {
	case <-pc.Done():
	case <-runCtx.Done():
	}
	// Retire this connection's sender FIRST. The registry outlives the
	// Session, so without this it keeps handing tasks a sender pointing at a
	// socket nobody reads — and a task reporting its finish on the way out
	// (cancelTasksUnlessHeld is about to cause exactly that) writes into it and
	// is told nothing went wrong, because over UDP nothing does for another
	// minute. Both modes end here: listen mode's handleServerConn calls
	// OnConnect too.
	session.retireSender()
	session.cancelTasksUnlessHeld()
	return nil
}

// Run is the single-shot entry point: sequential Connect → OnConnect. The
// integration suite is what uses it — agent-runner does NOT. Its listen mode
// calls ListenAndServe and its dial mode hands Connect/OnConnect to
// cli.PersistLoop, so anything that must happen once per runner process has to
// be attached to those two, not to this.
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
		te, ok := session.reg.get(taskIDHex)
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
		// The ACCEPTED ids. Anything this runner is holding and the server did
		// not name is over: it refused the task, or never had it. Absence is
		// the signal on purpose — a list the server forgot to fill kills
		// children, which is loud and recoverable, where a refused-list it
		// forgot to fill would strand them silently. It is also the only path
		// that reaches a task cancelled while it was held, since CancelTask
		// can never be delivered to one.
		if session.reg.holdArmed() {
			// Declined means the server did not look at the report at all —
			// it is shutting down and its answer says nothing about what to
			// keep. Treating that empty list as a refusal would kill the
			// children this hold exists to preserve, which is what happened
			// on a dummy instance before this branch existed: the runner
			// reconnected to the DYING server inside its first backoff.
			if rhr.Declined() {
				session.logger().Info("hold: the server declined to reconcile (it is going down); keeping the children and waiting for the next one")
			} else {
				accepted := make(map[string]bool, len(rhr.Accepted))
				for _, t := range rhr.Accepted {
					accepted[hex.EncodeToString(t.Id[:])] = true
				}
				session.reg.killHeldExcept(accepted, session.logger())
			}
		}
	case protocol.RunnerRequestType_HoldTasks:
		ht := req.HoldTasks()
		if ht == nil {
			return
		}
		session.handleHoldTasks(ht)
	case protocol.RunnerRequestType_RebindSession:
		rb := req.RebindSession()
		if rb == nil {
			return
		}
		session.handleRebindSession(rb)
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
	case protocol.RunnerRequestType_GitQuery:
		gq := req.GitQuery()
		if gq == nil {
			return
		}
		go session.handleGitQuery(ctx, gq)
	case protocol.RunnerRequestType_OpenExecRun:
		er := req.OpenExecRun()
		if er == nil {
			return
		}
		go session.handleExecRun(ctx, er)
	case protocol.RunnerRequestType_CloseExecRun:
		ce := req.CloseExecRun()
		if ce == nil {
			return
		}
		session.handleCloseExecRun(ce)
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
	case protocol.RunnerRequestType_TrsfState:
		ts := req.TrsfState()
		if ts == nil {
			return
		}
		// Read-only and synchronous: a map walk plus one reply.
		handleTrsfState(session, *ts, session.sendRunnerMessage)

	case protocol.RunnerRequestType_AuthorizeDataPlane:
		ad := req.AuthorizeDataPlane()
		if ad == nil {
			return
		}
		// Synchronous, like EstablishRelay above: a map insert plus one reply,
		// and the server is blocked on that reply.
		handleAuthorizeDataPlane(ctx, log, session, session.Endpoint, *ad, func(resp protocol.AuthorizeDataPlaneResponse) error {
			var rm protocol.RunnerMessage
			rm.Kind = protocol.RunnerMessageType_AuthorizeDataPlaneResponse
			rm.SetAuthorizeDataPlaneResponse(resp)
			payload := rm.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
			return session.Sender.Send(payload)
		})
	case protocol.RunnerRequestType_RevokeDataPlane:
		rd := req.RevokeDataPlane()
		if rd == nil {
			return
		}
		handleRevokeDataPlane(session, *rd, func(resp protocol.RevokeDataPlaneResponse) error {
			var rm protocol.RunnerMessage
			rm.Kind = protocol.RunnerMessageType_RevokeDataPlaneResponse
			rm.SetRevokeDataPlaneResponse(resp)
			payload := rm.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
			return session.Sender.Send(payload)
		})
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
// The scheme comes from the candidate being dialed, not from Config: a
// candidate list may mix ws and udp, and each needs its own endpoint.
func buildRunnerEndpoint(cfg Config, scheme string) (objproto.Endpoint, error) {
	switch scheme {
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
		return nil, fmt.Errorf("unsupported transport %q in --server-cid", scheme)
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
