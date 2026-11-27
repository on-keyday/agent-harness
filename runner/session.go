package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/hostlog"
	"github.com/on-keyday/agent-harness/iplink"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/runner/streamagent"
	"github.com/on-keyday/agent-harness/termmode"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// wakeDebounceWindow is the minimum interval between successive wake
// writes for one task. Coalescing rationale lives in
// docs/superpowers/specs/2026-04-29-agent-wake-and-origin-design.md.
const wakeDebounceWindow = 1500 * time.Millisecond

// wakeMarker is the body of the synthetic prompt written to the agent's
// stdin. The leading "<harness:agentboard-wake>" tag is machine-detectable
// (hook post-processing) and is the ONLY place the word "agentboard"
// appears: the prose deliberately omits it, because "agentboard message"
// tempts the agent to GUESS a command, and the plausible guess `harness-cli
// board read` is a real but wrong tool (topic-scoped retained dump, needs a
// <topic> arg) rather than the subscribed-inbox read. Instead the prose
// names the exact read command, `harness-cli agent inbox --json`, framed as
// a fallback since the UserPromptSubmit hook usually injects the message
// already.
//
// `harness-cli skill` is the general agentboard reference (peer messaging,
// worker sessions, delegation, operator notify, discovery — not reply-only,
// hence "how to act"). BOTH pointers are guarded by an "if": an agent that
// already has the message and already knows the conventions runs neither
// command, and the marker degenerates to "new message(s) … reply only if
// warranted" — no reflexive `harness-cli skill` read on every wake. Both
// are plain binary invocations, so the marker works as a context-free
// auto-wake in ANY runtime; and it stays action-agnostic ("reply only if
// warranted") so the LLM is not forced into an unwarranted reply.
//
// IMPORTANT: wakeMarker has NO trailing newline / carriage return; ptyWake
// adds the submit keystroke, in one of two ways.
//
// The text and a trailing "\r" read together are taken as PASTED input, and
// the "\r" becomes a literal newline inside the input box instead of an Enter.
// First observed against claude code with a single combined write, and the
// first fix split the write — text, wakeSubmitDelay, lone "\r". That only moves
// the race to the reader: the delay is the WRITER's, and an agent whose event
// loop is late by more than it (host overload, a /clear being processed) still
// reads both at once. Measured: a stopped-and-continued claude leaves the
// marker in its input box every time, and the stuck markers pile up until a
// later wake happens to submit them all.
//
// The general mechanism has a name: bracketed paste (DECSET 2004,
// ESC[200~ … ESC[201~), by which a terminal marks pasted text so a TUI can
// distinguish it from keystrokes. There is no terminal emulator in this path —
// the runner writes straight to the PTY master — so the runner does what one
// would: when the agent has set the mode, ptyWake brackets the marker itself,
// and the "\r" after ESC[201~ is a keystroke however the bytes are read.
// claude, codex, agy, opencode and pi all set it, and all submit the bracketed
// marker from one read (measured 2026-10-24; see
// docs/superpowers/specs/2026-10-24-pty-wake-bracketed-paste-design.md). An
// agent that never sets it still gets the split write.
//
// Except on Windows, where the Enter always goes in a write of its own after a
// longer gap, pasted marker or not; see wakeInputFor.
//
// Bracketing is safe HERE because wakeMarker is a fixed constant with no
// ESC[201~ in it, which would end the paste early and turn the rest into
// keystrokes. That does NOT generalise to arbitrary text — `session send`
// deliberately stays a raw byte pipe and lets the caller emit ESC[200~/ESC[201~
// itself with -e.
const wakeMarker = "<harness:agentboard-wake> new message(s) — read via `harness-cli agent inbox --json` if not already in context; consult `harness-cli skill` if unsure how to act. Reply only if warranted."

// wakeSubmitDelay is the gap between the marker text write and the lone
// submit byte write, for an agent without bracketed paste. Long enough for the
// agent's input parser to flush the first chunk into its text-input state
// before the trailing keystroke arrives, short enough not to be perceptible.
// Tuned by feel; see the wakeMarker comment for why it is not a guarantee.
const wakeSubmitDelay = 100 * time.Millisecond

// windowsWakeSubmitDelay is wakeSubmitDelay on a Windows runner. It has to
// clear codex's 120ms window in which an Enter after fast-arriving text is a
// newline (see wakeInputFor). Measured 2026-11-02 against codex 0.166.0 on a
// Windows runner: a ~100ms gap left the text in the box, a ~300ms gap
// submitted it.
const windowsWakeSubmitDelay = 300 * time.Millisecond

// wakeInput is how a PTY wake is typed on one OS: whether the Enter after a
// pasted marker may share its write, and the gap before a lone Enter.
type wakeInput struct {
	enterWithPaste bool
	submitDelay    time.Duration
}

// wakeInputFor is the wakeInput for a runner on goos.
//
// On Windows the Enter never shares a write with the marker, pasted or not.
// ConPTY passes the agent's ?2004h through, so the runner sees the mode, but
// what reaches the agent's input differs per agent. codex reads key events
// rather than a paste and classifies by timing
// (codex-rs/tui/src/bottom_pane/paste_burst.rs): an Enter within
// PASTE_ENTER_SUPPRESS_WINDOW (120ms) of fast-arriving text is a newline. So
// paste-and-Enter in one write left every wake to a Windows codex in its input
// box, as did text, 100ms, Enter. claude 2.1.312 is the other way round: it
// submitted paste-and-Enter, and left text, 300ms, Enter in the box. The
// paste with its Enter 300ms later submitted on both (measured 2026-11-02,
// once each). The gap is the writer's, so an agent that reads late can still
// merge the two writes; on Windows nothing better is available.
func wakeInputFor(goos string) wakeInput {
	if goos == "windows" {
		return wakeInput{enterWithPaste: false, submitDelay: windowsWakeSubmitDelay}
	}
	return wakeInput{enterWithPaste: true, submitDelay: wakeSubmitDelay}
}

// wakeSubmitByte is the byte sequence treated as Enter when delivered as a
// standalone PTY write.
var wakeSubmitByte = []byte{'\r'}

// pastedMarker is the marker as a bracketed paste, without the Enter.
var pastedMarker = "\x1b[200~" + wakeMarker + "\x1b[201~"

// bracketedWake is the whole wake for an agent with bracketed paste on, written
// in one call: the marker as a paste, then Enter.
var bracketedWake = []byte(pastedMarker + "\r")

// wakeForm is the shape a wake took on the agent's stdin, logged with
// `wake written`. An idle agent submits a PTY wake in either PTY form, so the
// screen cannot tell them apart; the log line is the only place that can.
type wakeForm int

const (
	wakeSplit      wakeForm = iota // text, wakeInput.submitDelay, lone CR
	wakePaste                      // one bracketed paste and CR
	wakeTurn                       // one event-stream user turn
	wakePasteSplit                 // bracketed paste, wakeInput.submitDelay, lone CR
)

func (f wakeForm) String() string {
	switch f {
	case wakeSplit:
		return "split"
	case wakePaste:
		return "paste"
	case wakeTurn:
		return "turn"
	case wakePasteSplit:
		return "paste-split"
	}
	return "wakeForm(" + strconv.Itoa(int(f)) + ")"
}

// Sender is the runner's outbound interface to the server. Decoupled from concrete
// trsf.Transport / objproto.Connection so tests can use a mock.
type Sender interface {
	// Send transmits a control-frame message. The bytes are already wire-prefixed.
	Send(data []byte) error
	// ID is the runner's connection ID, used by the server-side dispatch.
	ID() objproto.ConnectionID
	// SendDatagram transmits one unreliable payload, already wire-prefixed like
	// Send's. MaxDatagramSize says what currently fits in one and MOVES with
	// PLPMTUD, so a caller reads it per send.
	//
	// Separate from Send because what they ride differs in the properties that
	// matter: Send's message is unacknowledged and outside congestion control,
	// this is neither. A udp forward's replies go here; nothing else does yet.
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}

// taskEntry holds the per-task cancellation function, the repo it runs
// under, and the (debounced) stdin-wake state.
type taskEntry struct {
	cancel   context.CancelFunc
	repoPath string

	// wake delivers one wake to the running agent, in the form its stdin
	// takes: a PTY session gets the marker typed and submitted, a stream
	// session gets it as a user turn. Set from OnStdinWriter by the two
	// handleOpenExec paths; until it is set the task has no stdin to wake. It
	// reports the form it wrote, for the log.
	wake func() (wakeForm, error)

	// lastWakeAt is the most recent time WakeStdin actually wrote to
	// stdin. Wakes within wakeDebounceWindow are dropped (the agent's
	// next inbox call will pick up everything via --since-last cursor).
	lastWakeAt time.Time

	// waking is true while a wake is being written. WakeStdin runs off the
	// receive loop, so two can overlap, and lastWakeAt is only set once a
	// write finishes — the debounce cannot see one still in progress. A wake
	// that finds this set is dropped for the same reason a debounced one is.
	waking bool

	// ticket is the agentboard ticket this task's agent holds, frozen into its
	// env at spawn. Kept here because the reconnect report has to carry it: a
	// restart forgets every ticket (the board's registry is an in-memory map)
	// while the surviving agent keeps presenting this value, so a freshly
	// minted one would answer BadTicket. It arrives on OpenExecRunnerRequest,
	// the one spawn path since A3 retired the oneshot kind's AssignTask.
	ticket [16]byte

	// started / exited bracket the child's life. A registry entry exists from
	// registration, which is before the worktree is created and before
	// anything is spawned, so "there is an entry" is not "there is a child" —
	// and a hold that promised the difference would have the server writing
	// task_held for children that do not exist.
	started atomic.Bool
	exited  atomic.Bool

	// relay is the interposed stream for an interactive session, present only
	// on that path. It owns the splice between the PTY and whatever stream is
	// currently pointed at the server, which is what makes a rebind possible
	// and what keeps the PTY open across the gap.
	relay *sessionRelay
	// noPTY: the child runs on pipes (A-D33), so a cancel cannot hang it up
	// and ends it through its context at once (cancelTask).
	noPTY bool
}

// taskRootCtx is the context a spawned task descends from: the process's when
// one was supplied, otherwise the caller's (which is the connection's, i.e.
// the pre-hold behaviour).
//
// This is the single line that decides whether a disconnect can kill a child
// by parentage. With it, nothing ends a task on disconnect unless
// cancelTasksUnlessHeld says so.
func (s *Session) taskRootCtx(connCtx context.Context) context.Context {
	if s.processCtx != nil && s.reg != nil {
		return s.processCtx
	}
	return connCtx
}

// sendTaskMsg delivers a task lifecycle message through whatever connection is
// CURRENT, waiting while a hold is armed.
//
// Only the terminal messages need this, and they need it badly: TaskAccepted
// and TaskStarted are sent at spawn, long before any hold, but TaskFinished can
// be the one thing a held task still has to say — and bound to the connection
// the task STARTED on it went into a dead socket, so the server never learned
// the task had ended and the row stayed Running forever. Found on a
// re-adopted oneshot task (a kind A3 retired) that had actually run to
// completion.
//
// It reports its own failures, which is the only reason every call site may
// discard the return with `_ =`. The send happens on a task's way out, so
// there is nowhere else the loss could surface: a task that ran to completion
// ended up recorded by the server as Failed with a disconnect reason and no
// exit code of its own, and neither side said a TaskFinished had been written
// and dropped.
func (s *Session) sendTaskMsg(taskIDHex string, data []byte) error {
	err := s.reg.sendWhenConnected(data, s.Sender)
	if err != nil {
		s.logger().Warn("task message not delivered; the server ends this task on its own "+
			"(runner_disconnected, or not_held_by_runner once a hold lapses), so its recorded "+
			"reason and exit code are not the agent's",
			"task_id", taskIDHex, "err", err)
	}
	return err
}

// cancelledBeforeStart is the TaskFinished reason for a task whose CancelTask
// arrived before the task did. handleCancelTask answers such a cancel with
// cancelNotRunning; the spawn path that then refuses the late task says this.
const (
	cancelNotRunning     = "cancel: task not running on this runner"
	cancelledBeforeStart = "cancel: cancelled before it started"
)

// beginPendingTask marks a task whose OpenExec just arrived as on
// its way in, and returns the func that clears the mark. The receive loop calls
// it before handing the task to its own goroutine, which defers the result.
func (s *Session) beginPendingTask(tid protocol.TaskID) (end func()) {
	taskIDHex := hex.EncodeToString(tid.Id[:])
	s.mu.Lock()
	s.initMaps()
	reg := s.reg
	s.mu.Unlock()
	reg.beginPending(taskIDHex)
	return func() { reg.endPending(taskIDHex) }
}

// handleCancelTask ends a task the server cancelled, or — when this runner is
// not running it — answers with a TaskFinished. The server resends CancelTask
// until a TaskFinished arrives (a CancelTask is one datagram, lost silently
// over UDP), so a cancel for an unknown task means the TaskFinished never
// reached it or the task never reached us; staying silent would leave it
// resending. A task still pending (see TaskRegistry.pending) is marked, so it
// is refused at registration instead of started.
func (s *Session) handleCancelTask(tid protocol.TaskID) {
	taskIDHex := hex.EncodeToString(tid.Id[:])
	if s.reg.cancelTask(taskIDHex) {
		return
	}
	s.logger().Info("runner: cancel for unknown task; answering with TaskFinished", "task_id", taskIDHex)
	var m protocol.RunnerMessage
	m.Kind = protocol.RunnerMessageType_TaskFinished
	m.SetTaskFinished(protocol.TaskFinished{
		TaskId:       tid,
		ExitCode:     -1,
		ErrorMessage: []byte(cancelNotRunning),
	})
	_ = s.sendTaskMsg(taskIDHex, m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
}

// childLive reports that this task's child process has started and not yet
// exited — the only condition under which a hold may promise it.
func (e *taskEntry) childLive() bool {
	return e != nil && e.started.Load() && !e.exited.Load()
}

// Session manages the runner's task lifecycle. It is created once per connection
// and handles concurrent tasks through its internal maps.
type Session struct {
	AllowedRoots []string // absolute paths this runner is allowed to serve

	// transcriptEnv is the environment the agents' transcript stores are
	// resolved through (pillar B, B-D5); nil means this process's, which the
	// agents it starts inherit. A test points the stores at temp dirs.
	transcriptEnv func(string) string

	// HostLog reads this runner's process log for RunnerRequestType_HostLog (pillar C).
	HostLog *hostlog.Reader

	// execCancels holds the cancel func of every out-of-band exec running on
	// this runner (execId -> cancel), so a close_exec_run request can stop one.
	// A dedicated map because an exec belongs to no task entry: it may target a
	// TERMINAL task whose per-task state this runner has already dropped.
	execCancels execCancels

	// Profiles is the set of agent launch profiles this runner was
	// configured with: the default profile (from --agent-bin/--agent-args/
	// --agent-*-argv) plus any extra profiles from --agent-profiles. Each
	// OpenExec request carries an AgentProfile name (empty == default) that
	// handleOpenExec resolves against this set via resolveExec to pick the
	// bin + argv actually exec'd.
	Profiles ProfileSet
	Sender   Sender
	// trsfConns is every connection this runner holds, so its congestion state
	// can be reported. Nothing else in the runner keeps such a list.
	trsfConns trsfRegistry

	Streams peer.BidirectionalStreamLookup // optional; required for handleOpenExec
	Logger  *slog.Logger                   // optional; defaults to slog.Default()
	Now     func() time.Time

	// creator makes bidi streams toward the server. Set to pc.Transport() for
	// live runner connections; required by remote port-forward (one stream per
	// accepted connection). nil in tests that don't exercise remote forward.
	creator bidiStreamCreator
	// rforwards tracks active remote port-forward listeners by forwardId so a
	// ClosePortForward request can shut them down. Lazily created (see
	// rforwardListeners / port_forward.go).
	rforwards     *remoteForwardListeners
	rforwardsOnce sync.Once
	// udpForwards tracks udp -L registrations and the per-flow sockets dialled
	// under each. Separate from rforwards because it holds a different thing
	// for a different direction: those are listeners this runner opened for -R,
	// these are sockets it dialled for -L, and a udp -L has no listener at all.
	// Lazily created (see udpForwardRegistry / forward_datagram.go).
	udpForwards *udpForwards
	// ipLinks tracks this session's ip links, one tun device each. Lazily
	// created (see ipLinkRegistry / ip_link.go).
	ipLinks     *ipLinks
	ipLinksOnce sync.Once
	// openTun creates a link's device; nil means iplink.OpenTun. Set from
	// Config.TunOpener, which tests use to stand a fake in for the kernel.
	openTun iplink.Opener

	// ServerCID, Hostname, WSPath, BinDir are required for HARNESS_* env
	// injection at task spawn time. Filled from Config in connect.go.
	// BinDir is prepended to the agent's PATH so harness-cli is reachable
	// from within the task worktree (which is a different worktree than
	// the runner's binary location).
	ServerCID objproto.ConnectionID
	Hostname  string
	// MintedRunnerID is what this PROCESS generated at startup, kept only so
	// SetRunnerCanonicalID can notice the server echoing something else back.
	// The canonical value the server reports is what gets used.
	MintedRunnerID protocol.RunnerID
	WSPath         string
	BinDir         string
	// ServerPubKey is forwarded to agents as HARNESS_SERVER_PUBKEY.
	ServerPubKey ed25519.PublicKey
	// OnLeave: Config.OnLeave, called on RunnerRequest{leave}.
	OnLeave func(by string)

	// ProxyVia, when non-empty, is propagated into spawned agent env as
	// HARNESS_PROXY_VIA_RUNNER (Phase B). Set by ListenAndServe in listen mode.
	ProxyVia string

	// Endpoint is the objproto.Endpoint this session's peer.Conn lives on.
	// Required by handleEstablishRelay to install eager SetProxy entries.
	// Set by Connect (dial mode) and by handleServerConn (listen mode).
	// Written by the same goroutine that subsequently calls OnConnect, so no
	// mutex is needed; dispatchRunnerRequest cannot observe a nil Endpoint.
	Endpoint objproto.Endpoint

	// Grants holds the data-plane grants the server has pushed to this runner.
	// Built lazily via ensureGrants so a Session that never carries one does
	// not have to know about it. See runner/dataplane_grants.go.
	Grants *grantStore
	// grantSweeper guards startGrantSweeper so the ticker is started once per
	// session however many grants arrive.
	grantSweeper atomic.Bool

	// runnerCanonicalID is the RunnerID the server keys this runner as in
	// its registry / agentboard ticket store. Filled from RunnerHelloResponse
	// (server → runner) before any OpenExec. Reads/writes are guarded by
	// mu — the wire ordering guarantees Set happens before any handleOpenExec
	// reads, but the lock provides a memory barrier for the cross-goroutine
	// happens-before.
	runnerCanonicalID protocol.RunnerID

	// NoWorktree, when true, makes handleOpenExec skip the
	// worktree create / branch / cleanup steps and run the agent process
	// directly in the request's RepoPath. Settings/skills injection is also
	// skipped by default (use ForceInjectHarnessSettings to override).
	// HARNESS_* env vars are still injected. Set from runner.Config.NoWorktree.
	NoWorktree bool

	// ForceInjectHarnessSettings, when true, causes WriteAgentSettings /
	// WriteAgentSkills to run even in NoWorktree mode (target = repoPath).
	// No-op when NoWorktree=false (worktree mode always injects regardless).
	// Set from runner.Config.ForceInjectHarnessSettings.
	ForceInjectHarnessSettings bool

	// AgentSkillsFS, when non-nil, is the skill tree WriteAgentSkills
	// materialises into each task worktree instead of the embedded copy. Set
	// from runner.Config.AgentSkillsFS (agent-runner --agentskills-dir). It is
	// read on every task spawn, so editing a SKILL.md on disk reaches the next
	// task without restarting this runner.
	AgentSkillsFS fs.FS

	mu sync.Mutex
	// reg holds this runner PROCESS's live tasks. It is supplied by Config so
	// it OUTLIVES this Session: a task map owned per connection takes every
	// child's cancel func, wake writer and relay with it when the link drops,
	// which is the difference between today's behaviour and a hold. Session
	// makes its own when Config supplies none, which is what keeps tests and
	// the single-shot Run path unchanged.
	reg *TaskRegistry

	// processCtx is what a TASK's context descends from when a hold-capable
	// registry is wired. Only the two spawn paths use it: file transfers, git
	// queries and execs stay rooted at the CONNECTION, because they exist for
	// a client that is gone the moment the link is.
	processCtx context.Context
	wms        map[string]*WorktreeManager // repoPath → WorktreeManager

	// testHookOpenExec is called in handleOpenExec, after its panic guard is
	// in place, in tests to inject faults (e.g. panics). Nil in production.
	testHookOpenExec func()

	// chainedRelayPendingMu guards chainedRelayPendingCh. One-at-a-time
	// invariant: at most one RequestChainedRelay may be in flight per
	// session at any time (spec Decision 2). BeginChainedRelay returns an
	// error if another is already pending; DeliverChainedRelayResponse
	// sends to the channel and clears it.
	chainedRelayPendingMu sync.Mutex
	chainedRelayPendingCh chan protocol.ChainedRelayResponse // nil when none pending
}

// SetRunnerCanonicalID stores the RunnerID the server reports for this runner
// connection (via RunnerHelloResponse). Called from dispatchRunnerRequest
// synchronously before any OpenExec is dispatched.
//
// The response echoes back what this process minted, so a difference means the
// server keyed us as something else — every ticket we hand an agent would then
// validate against the wrong board key. Logged loudly rather than corrected:
// what the server said is what its board actually uses, so that is what gets
// stored and injected.
func (s *Session) SetRunnerCanonicalID(rid protocol.RunnerID) {
	s.mu.Lock()
	minted := s.MintedRunnerID
	s.runnerCanonicalID = rid
	s.mu.Unlock()
	if !minted.IsZero() && rid != minted {
		s.logger().Error("runner identity echo mismatch",
			"sent", minted.Hex(), "got", rid.Hex())
	}
}

// runnerCanonicalRunnerID returns the identity the server keys this runner as,
// for embedding in HARNESS_RUNNER_ID. Zero until RunnerHelloResponse has
// arrived, and left that way on purpose: an agent then fails Hello validation
// with UnknownTask, which surfaces the missing handshake instead of quietly
// sending something that looks plausible.
//
// It used to convert to objproto.ConnectionID, because the identity WAS an
// address and the env var needed a string. The identity has its own string now.
func (s *Session) runnerCanonicalRunnerID() protocol.RunnerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runnerCanonicalID
}

// initMaps initialises the internal maps if they have not been set yet.
// Must be called with s.mu held.
func (s *Session) initMaps() {
	if s.reg == nil {
		s.reg = NewTaskRegistry()
	}
	if s.wms == nil {
		s.wms = make(map[string]*WorktreeManager)
	}
}

// getWorktreeManager returns the cached WorktreeManager for repoPath, creating
// one if it doesn't exist yet. Thread-safe.
func (s *Session) getWorktreeManager(repoPath string) *WorktreeManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initMaps()
	wm, ok := s.wms[repoPath]
	if !ok {
		wm = &WorktreeManager{Repo: repoPath}
		s.wms[repoPath] = wm
	}
	return wm
}

// repoAllowed reports whether repoPath equals one of AllowedRoots. Uses
// protocol.RootMatches, the predicate the server's Registry.Candidates uses,
// so a repo below a root is refused here as it is there (G9).
func (s *Session) repoAllowed(repoPath string) bool {
	for _, root := range s.AllowedRoots {
		if protocol.RootMatches(root, repoPath) {
			return true
		}
	}
	return false
}

func (s *Session) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// ServerCIDForProxyAllocate returns the ConnectionID the runner uses for its
// server peer.Conn. SetProxy's "allocate" CID for a proxied agent uses this
// transport + addr and the agent-chosen connection_id; see Phase B spec.
func (s *Session) ServerCIDForProxyAllocate() objproto.ConnectionID {
	return s.ServerCID
}

// HasTask reports whether t is currently an active task on this session.
// Used by the agent proxy handler to validate ProxyRequest.task_id.
func (s *Session) HasTask(t protocol.TaskID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.reg.get(hex.EncodeToString(t.Id[:]))
	return ok
}

// BeginChainedRelay registers a pending chained-relay request on this session
// and returns a buffered channel that will receive the ChainedRelayResponse
// when the server replies. Returns an error if another chained-relay request
// is already in flight (one-at-a-time invariant, spec Decision 2).
// The caller must either receive from the returned channel or give up; in both
// cases the channel is consumed/dropped naturally when DeliverChainedRelayResponse
// fires or the session is torn down.
func (s *Session) BeginChainedRelay() (chan protocol.ChainedRelayResponse, error) {
	s.chainedRelayPendingMu.Lock()
	defer s.chainedRelayPendingMu.Unlock()
	if s.chainedRelayPendingCh != nil {
		return nil, fmt.Errorf("chained relay already in flight")
	}
	s.chainedRelayPendingCh = make(chan protocol.ChainedRelayResponse, 1)
	return s.chainedRelayPendingCh, nil
}

// AbortChainedRelay clears the pending chained-relay slot without delivering a
// response. Called when the ceremony is abandoned (timeout or context cancel)
// so that a future BeginChainedRelay on the same session can proceed. If a
// late-arriving DeliverChainedRelayResponse fires after AbortChainedRelay, it
// sees a nil slot and returns false (warn-logged by the caller) — correct.
func (s *Session) AbortChainedRelay() {
	s.chainedRelayPendingMu.Lock()
	defer s.chainedRelayPendingMu.Unlock()
	s.chainedRelayPendingCh = nil
}

// DeliverChainedRelayResponse delivers resp to the waiting BeginChainedRelay
// caller and clears the pending slot. Returns false if no waiter is registered
// (stale or spurious message from server).
func (s *Session) DeliverChainedRelayResponse(resp protocol.ChainedRelayResponse) bool {
	s.chainedRelayPendingMu.Lock()
	defer s.chainedRelayPendingMu.Unlock()
	if s.chainedRelayPendingCh == nil {
		return false
	}
	s.chainedRelayPendingCh <- resp
	s.chainedRelayPendingCh = nil
	return true
}

// resolveExec resolves profile against ps (empty name == the default
// profile) and builds the argv for either a `session new --oneshot`
// invocation (oneshot=true — templated with prompt via buildOneshotArgs) or an
// interactive/PTY invocation (oneshot=false — via buildInteractiveArgs, the
// prompt filling the template's {prompt}, pillar A spec A-D3). resume selects the resume-conversation argv templates.
// extra is the caller-supplied per-task extra args; it is merged after the
// profile's own AgentArgs baseline (profile args first, per-task extras
// last, so a per-task flag wins on conflict).
//
// Returns an error (never a silent fallback to the default profile) if the
// profile name is unknown or its argv templates are invalid — callers must
// fail the task rather than guess a substitute binary.
func resolveExec(ps ProfileSet, profile string, oneshot, resume bool, extra []string, prompt string) (bin string, argv []string, err error) {
	p, err := ps.Resolve(profile)
	if err != nil {
		return "", nil, err
	}
	args := mergeExtraArgs(p.AgentArgs, extra)
	if oneshot {
		argv, err = buildOneshotArgs(p.OneshotArgv, p.ResumeOneshotArgv, args, prompt, resume)
	} else {
		argv, err = buildInteractiveArgs(args, p.InteractiveArgv, p.ResumeInteractiveArgv, resume, prompt)
	}
	if err != nil {
		return "", nil, err
	}
	return p.Bin, argv, nil
}

// handleOpenExec is the runner-side counterpart of the server's
// handleOpenInteractive. The server has already created a bidi stream
// toward us and stored its id in oer.StreamId; we look it up and feed it
// to exec.ExecuteCommand for an interactive PTY claude session in a fresh
// worktree — or, with no_pty (A-D33), for the same child on pipes.
//
// Lifecycle messages:
//   - TaskAccepted (we are committed even if worktree creation later fails)
//   - TaskStarted with worktree dir (after a successful worktree create)
//   - TaskFinished with claude's exit code (after exec.ExecuteCommand returns)
//
// On EOF / detach from the TUI side, exec.ExecuteCommand's SIGHUP→SIGTERM→
// SIGKILL ladder reaps claude; this method returns and TaskFinished follows.
func (s *Session) handleOpenExec(ctx context.Context, oer *protocol.OpenExecRunnerRequest) {
	taskIDHex := hex.EncodeToString(oer.TaskId.Id[:])
	log := s.logger()

	repoPath := string(oer.RepoPath)

	// Step 1: TaskAccepted.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
		m.SetTaskAccepted(protocol.TaskAccepted{TaskId: oer.TaskId})
		_ = s.Sender.Send(m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
	}

	log.Info("handleOpenExec", "task_id", taskIDHex, "repo", repoPath)

	finishWithError := func(code int32, reason string) {
		log.Error("handleOpenExec: "+reason, "task_id", taskIDHex, "repo", repoPath)
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{TaskId: oer.TaskId, ExitCode: code}
		tf.ErrorMessage = []byte(reason)
		m.SetTaskFinished(tf)
		_ = s.sendTaskMsg(taskIDHex, m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
	}

	if s.Streams == nil {
		log.Error("runner: handleOpenExec missing Streams lookup")
		finishWithError(-1, "runner: missing stream lookup")
		return
	}

	// Acquire the server-allocated bidi stream BEFORE any gate / setup checks.
	// The server side has already created its end and started splicing it to
	// the TUI; any error path that returns without closing this stream would
	// leave the server's splice goroutine — and the TUI it serves — blocked
	// forever on ReadDirect.
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(oer.StreamId))
	if stream == nil {
		finishWithError(-1, "runner: server-allocated exec stream not visible")
		return
	}

	// refuse ends the open before any agent runs: an End frame first, so a
	// client attached to the stream is told why rather than seeing it close,
	// then the close and the task's failure with the same reason.
	refuse := func(reason string) {
		writeEndFrame(stream, -1, reason)
		_ = stream.CloseBoth()
		finishWithError(-1, reason)
	}

	// A panic fails this task alone: it runs on its own goroutine, and an
	// unrecovered panic there would end the runner and every task on it.
	defer func() {
		if r := recover(); r != nil {
			s.logger().Error("handleOpenExec panic", "task_id", taskIDHex, "panic", r)
			refuse(fmt.Sprintf("runner_panic: %v", r))
		}
	}()
	if s.testHookOpenExec != nil {
		s.testHookOpenExec()
	}

	// The session's initial prompt, on its own stream (A-D17), read before any
	// gate can refuse the open so a refusal does not leave it buffered. A PTY
	// open hands it to the agent's argv; a stream open to the adapter's first
	// turn.
	var prompt string
	if oer.PromptPresent() {
		var id uint64
		if p := oer.PromptStreamId(); p != nil {
			id = *p
		}
		p, err := waitForPromptBody(ctx, s.Streams, trsf.StreamID(id))
		if err != nil {
			refuse("prompt: " + err.Error())
			return
		}
		prompt = p
	}

	// Gate: check repo is one of the allowed roots.
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repoPath) {
		refuse("repo_not_allowed: " + repoPath)
		return
	}

	// Gate: resolve the requested agent profile + build the interactive argv
	// up front (see resolveExec). An unknown profile fails the task clearly
	// — no silent fallback to the default profile — and, same as the gate
	// above, the server-allocated stream must be closed here or the
	// server's splice goroutine blocks on ReadDirect forever. A stream open
	// gives its prompt to the adapter instead, so it is not in this argv.
	argvPrompt := prompt
	if oer.EventStream() {
		argvPrompt = ""
	}
	// --oneshot (A3, A-D29): the profile's oneshot argv in the PTY, which ends
	// when the agent answers. It needs the prompt it exists to run, and it is
	// a terminal session, not an event stream.
	oneshot := oer.Oneshot()
	if oneshot && oer.EventStream() {
		refuse("oneshot_unsupported: --oneshot runs in a PTY, not as an event stream")
		return
	}
	if oneshot && prompt == "" {
		refuse("prompt_required: --oneshot runs the profile's oneshot argv, which needs a prompt")
		return
	}
	agentBin, agentArgv, err := resolveExec(s.Profiles, string(oer.AgentProfile), oneshot, oer.ResumeConversation(), oer.ExtraArgs.AsStrings(), argvPrompt)
	if errors.Is(err, errPromptUnsupported) {
		name := string(oer.AgentProfile)
		if name == "" {
			name = "(default)"
		}
		refuse("prompt_unsupported: profile " + name + " has no {prompt} in its interactive template")
		return
	}
	if err != nil {
		refuse("agent_profile: " + err.Error())
		return
	}

	// Gate the KIND beside the profile gate, and for the same reason. A profile
	// with no stream adapter cannot serve an event-stream task, and falling
	// back to a PTY would hand a client a terminal where it expects events —
	// the two are not substitutes. Refused here, with the stream closed, rather
	// than failing obscurely once a client is already reading it.
	var streamProfile AgentProfile
	if oer.EventStream() {
		p, perr := s.Profiles.Resolve(string(oer.AgentProfile))
		if perr != nil {
			refuse("agent_profile: " + perr.Error())
			return
		}
		if !p.ServesStream() {
			refuse("agent_profile " + p.Name + ": no stream adapter configured; " +
				"this profile cannot serve an event-stream task")
			return
		}
		streamProfile = p
	}

	// Fallback if RepoPath not set.
	if repoPath == "" && len(s.AllowedRoots) > 0 {
		repoPath = s.AllowedRoots[0]
	}

	// Register per-task cancellable context.
	//
	// ctx is the PROCESS's context when a hold-capable registry is wired, not
	// the connection's, so a disconnect no longer cancels this by parentage.
	// What ends a task on a disconnect is now an explicit decision:
	// Session.cancelTasksUnlessHeld.
	taskCtx, cancel := context.WithCancel(s.taskRootCtx(ctx))
	entry := &taskEntry{cancel: cancel, repoPath: repoPath, ticket: oer.AuthTicket, noPTY: oer.NoPty()}
	s.mu.Lock()
	s.initMaps()
	registered := s.reg.putUnlessCancelled(taskIDHex, entry)
	s.mu.Unlock()
	if !registered {
		cancel()
		refuse(cancelledBeforeStart)
		return
	}
	defer func() {
		cancel()
		s.mu.Lock()
		s.reg.remove(taskIDHex)
		s.mu.Unlock()
	}()

	// Step 2: worktree (skipped in NoWorktree mode — agent runs in repoPath directly).
	var dir string
	var wm *WorktreeManager
	if s.NoWorktree {
		dir = repoPath
		log.Info("no-worktree mode: using repo path as cwd", "task_id", taskIDHex, "repo", repoPath)
	} else {
		wm = s.getWorktreeManager(repoPath)
		d, err := wm.Create(taskIDHex)
		if err != nil {
			refuse("worktree_error: " + err.Error())
			return
		}
		dir = d
	}

	// Step 3: TaskStarted.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskStarted}
		ts := protocol.TaskStarted{TaskId: oer.TaskId}
		ts.SetWorktreeDir([]byte(dir))
		m.SetTaskStarted(ts)
		_ = s.Sender.Send(m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
	}

	// Write Claude Code and Codex hook settings into the worktree.
	// Non-fatal: task continues even if settings file can't be written.
	// Skipped in NoWorktree mode by default; ForceInjectHarnessSettings overrides.
	if !s.NoWorktree || s.ForceInjectHarnessSettings {
		if err := WriteAgentSettings(dir); err != nil {
			log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir, s.AgentSkillsFS); err != nil {
			log.Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}

	// Build HARNESS_* env vars for the subprocess.
	var x11Display int
	var x11AuthFile string
	var x11Enabled bool
	if oer.X11Enabled() {
		if f := oer.X11(); f != nil {
			x11Display = int(f.Display)
			x11Enabled = true
			if len(f.Cookie) > 0 {
				p, err := writeXauthFile(taskIDHex, x11Display, f.Cookie)
				if err != nil {
					log.Warn("x11 xauth setup failed; forwarding without authentication", "task_id", taskIDHex, "err", err)
				} else {
					x11AuthFile = p
					defer cleanupXauthFile(p)
				}
			} else {
				log.Info("x11 forwarding without authentication (no cookie)", "task_id", taskIDHex, "display", x11Display)
			}
		} else {
			log.Warn("x11 enabled but X11Forward block absent; continuing without DISPLAY", "task_id", taskIDHex)
		}
	}
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:    s.ServerCID,
		RunnerID:     s.runnerCanonicalRunnerID(),
		TaskID:       oer.TaskId,
		RepoPath:     repoPath,
		Hostname:     s.Hostname,
		WSPath:       s.WSPath,
		AuthTicket:   oer.AuthTicket,
		BinDir:       s.BinDir,
		ServerPubKey: s.ServerPubKey,
		ProxyVia:     s.ProxyVia,
		X11Enabled:   x11Enabled,
		X11Display:   x11Display,
		X11AuthFile:  x11AuthFile,
	})

	// Step 4: spawn claude under PTY, hand the stream to exec.
	// ExecuteCommandWithOption defers stream.CloseBoth() so we don't double-close here.
	// agentBin/agentArgv were already resolved from the requested profile by
	// the gate above. Args order: profile baseline
	// first, per-task extras appended.
	// PWD is derived from `dir` — the same value passed as the working
	// directory on the next line — so the two cannot drift; see AgentCwdEnv
	// for why the chdir alone leaves some agents pointed at the runner's
	// directory instead of the task's.
	env = append(env, AgentCwdEnv(dir)...)
	// No PTY (A-D33): the child runs on pipes, so nothing is a terminal for a
	// pager to wait on, and a oneshot reads /dev/null. The pager env is for a
	// oneshot that does have one.
	noPTY := oer.NoPty()
	if oneshot && !noPTY {
		env = append(env, OneshotPagerEnv()...)
	}

	// The event-stream kind: same stream, same lifecycle, a different thing on
	// the far end of it. StreamTask runs the adapter through agentexec with
	// ptyEnabled=false, so the frames carry neutral NDJSON rather than terminal
	// bytes, and taps them for the pending table and the agent's exit.
	if oer.EventStream() {
		st := &StreamTask{
			AdapterPath: streamProfile.StreamAdapter,
			AgentArgv:   append([]string{agentBin}, agentArgv...),
			Dir:         dir,
			Env:         env,
			// The initial prompt, if any, is the adapter's first user turn;
			// without one the session starts idle and a client sends it.
			Prompt:             prompt,
			ResumeConversation: oer.ResumeConversation(),
			Logger:             log,
			OnStdinWriter: func(write func([]byte) (int, error)) {
				s.setWake(taskIDHex, streamWake(write))
			},
			// No task log (retired for every kind in A3; this one dropped
			// its own first). It rendered every event into one until
			// 2026-09-30, on the premise that the ring evicts and so the
			// stream alone loses what happened while nobody watched. That
			// premise missed claude's own session transcript on the runner
			// host, which holds the whole conversation (and which a resume
			// now replays). The log was a lossy copy that no workflow relied
			// on, paid for in writes on the server's disk. Removed by operator
			// decision; the agent's stderr, the one thing only the log kept,
			// is shown by both chats instead.
		}
		runErr := st.Run(taskCtx, stream)

		// TaskFinished. The AGENT's own exit is what the adapter reported
		// in-band; runErr is the adapter's. They differ: the adapter exits 1
		// when its agent fails, and the agent's real code only exists in the
		// neutral `exit` message.
		{
			m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
			tf := protocol.TaskFinished{TaskId: oer.TaskId}
			ex := st.Exit()
			switch {
			case taskCtx.Err() != nil:
				// We killed it — the ordinary end for this kind, since nothing
				// else ends it. Not a crash; see the wiring log.
				log.Info("event-stream task ended by cancellation", "task_id", taskIDHex)
			case ex != nil && ex.Err != "":
				tf.ExitCode = -1
				tf.ErrorMessage = []byte("stream_adapter: " + ex.Err)
			case ex != nil && ex.Code != 0:
				tf.ExitCode = int32(ex.Code)
				tf.ErrorMessage = []byte("agent exited " + strconv.Itoa(ex.Code))
			case ex == nil && runErr != nil:
				// The adapter died without reporting an exit at all.
				tf.ExitCode = -1
				tf.ErrorMessage = []byte("stream_adapter: " + runErr.Error())
			}
			m.SetTaskFinished(tf)
			_ = s.sendTaskMsg(taskIDHex, m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
		}

		if !s.NoWorktree {
			switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
			case r.StatusErr != nil:
				log.Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
			case !r.Removed:
				log.Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
			}
		}
		return
	}

	// exitCode is the CHILD's own, read from its ProcessState rather than
	// inferred from an error: the errgroup inside agentexec returns whichever
	// goroutine failed first, and on Linux the pty's EIO at teardown beats the
	// exit status to it. Sniffing the error type reported a clean session as
	// "read /dev/ptmx: input/output error"; ProcessState cannot lose that race.
	var exitCode atomic.Int32
	exitCode.Store(0)
	// Interpose the relay between agentexec and the server's stream.
	//
	// agentexec takes its stream once, blocks for the whole session and defers
	// CloseBoth, so a rebind is impossible unless the stream it holds is one we
	// own. Handing it the relay also puts three other obligations in the same
	// object rather than scattered through this function: stop draining while
	// held, never close the PTY master, and keep the EOF that drives its
	// reaper ladder from firing because a server went away.
	relay := newSessionRelay(stream, s.reg.holdArmed).withLog(log, taskIDHex)
	entry.relay = relay
	defer relay.close()

	// The agent's terminal modes, read off everything it writes from its first
	// byte. Kept here rather than asked of the server: the server's copy starts
	// over when it restarts, and an agent that set bracketed paste once at
	// startup would then be typed at the wrong way.
	modes := termmode.New()

	opt := agentexec.ExecuteOption{
		Audit: ptyModeTap{modes: modes},
		OnStdinWriter: func(write func([]byte) (int, error)) {
			// A --oneshot child is `claude -p` or a command, not a prompt
			// reading turns: a wake would only type the marker and an Enter
			// into whatever it is running. Nor does a child on pipes get one
			// (A-D33): with no terminal it is a program reading its input
			// stream, and the marker is input to it — measured with bash,
			// which on a pipe is non-interactive, reads the marker as a
			// command, and exits 2 on its syntax error, ending the session.
			if !oneshot && !noPTY {
				s.setWake(taskIDHex, ptyWake(write, modes, wakeInputFor(runtime.GOOS)))
			}
			// The child's stdin is wired, so there IS a child now. This is
			// the earliest observable "started" on this path, and a hold may
			// only promise tasks that have one.
			entry.started.Store(true)
		},
		OnProcessExit: func(st *os.ProcessState, werr error) {
			entry.exited.Store(true)
			// How the child ended, always: for a held task this is the only
			// place the answer exists, and "it died during the hold" is
			// otherwise indistinguishable from "the relay ended the session".
			// taskCtx.Err() separates the endings: non-nil means the context
			// cancel ran — a cancel's backstop after ptyCancelGrace, or a hold
			// path — and on a PTY child that is go-pty's default Cancel, a
			// SIGKILL of the child pid alone (pillar A spec, F5); nil means
			// agentexec's stdin-EOF ladder (SIGHUP → SIGTERM → SIGKILL, ~2s),
			// which a cancel now starts through relay.endInput, or the
			// child's own exit did it.
			log.Info("agent child exited", "task_id", taskIDHex,
				"state", fmt.Sprint(st), "wait_err", werr,
				"held", s.reg.holdArmed(), "task_ctx_err", taskCtx.Err())
			if st != nil {
				exitCode.Store(int32(st.ExitCode()))
			}
		},
	}
	if noPTY {
		// Pipes, not a terminal (A-D33). No mode tracker: nothing reads
		// terminal modes off a pipe. A cancel ends a pipe child the way it
		// ends the stream kind's (A-D16): TERM to its process group, KILL
		// after a grace — the hangup ladder needs a terminal to hang up.
		opt.Audit = nil
		opt.KillProcessTree = runtime.GOOS != "windows"
		if oneshot {
			// /dev/null, not a pipe nobody writes: EOF on the first read,
			// with no window in which stdin is open and empty. objtrsf
			// refuses OnStdinWriter alongside it — there is no pipe — so the
			// child is marked started here, before the call that starts it;
			// a Start that fails returns at once and is marked exited below.
			opt.StdinDevNull = true
			opt.OnStdinWriter = nil
			entry.started.Store(true)
		}
	}
	runErr := agentexec.ExecuteCommandWithOption(taskCtx, relay, log, agentBin, agentArgv, dir, !noPTY, env, opt)
	entry.exited.Store(true)

	log.Info("ExecuteCommandWithOption RETURNED — the session is over",
		"task_id", taskIDHex, "err", runErr)
	if runErr != nil {
		log.Error("ExecuteCommand error", "task_id", taskIDHex, "error", runErr)
	}

	// Step 5: TaskFinished (interactiveFinished says how it is classified).
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		// The runner ended it if the context was cancelled (the cancel's
		// backstop, a shutdown) or a cancel ended the relay's input and the
		// child answered the hangup (relay.endInput).
		ended := taskCtx.Err() != nil || relay.wasInputEnded()
		if ended {
			log.Info("interactive task ended by cancellation", "task_id", taskIDHex)
		}
		tf := interactiveFinished(oer.TaskId, runErr, exitCode.Load(), ended)
		m.SetTaskFinished(tf)
		_ = s.sendTaskMsg(taskIDHex, m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
	}

	// Step 6: Conditionally clean up the worktree directory. The branch ref
	// harness/<id> is retained so any commits the agent made stay reachable.
	// The directory is removed only when `git status --porcelain` (excluding
	// harness-injected paths) is empty, so uncommitted work survives to a
	// resume; the long tail is `harness-cli prune-local`'s. Errors are logged
	// and never bubble up: TaskFinished is already sent.
	if !s.NoWorktree {
		switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
		case r.StatusErr != nil:
			log.Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
		case !r.Removed:
			log.Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
		}
	}
}

// WakeStdin delivers a wake to the given task's agent, debounced per task to
// one delivery per wakeDebounceWindow and one in flight at a time. Safe to
// call from any goroutine, and called from its own: the write can block for as
// long as the agent leaves its input unread. No-op
// if the task is unknown, or if it has no wake installed — both handleOpenExec
// paths install one (PTY and event-stream) once the child's stdin exists, and
// since A3 they are the only spawn paths, so the second case is a task whose
// child's stdin is not wired yet, or a --oneshot child, which never gets one.
//
// Coalescing rationale: subsequent agentboard messages within the window
// are not lost — they are read by the agent's next `agent inbox
// --since-last`, which uses a persisted cursor.
func (s *Session) WakeStdin(taskIDHex string) {
	s.mu.Lock()
	e, ok := s.reg.get(taskIDHex)
	if !ok || e == nil {
		s.mu.Unlock()
		s.logger().Info("wake skipped", "task_id", taskIDHex, "reason", "task not known to this runner")
		return
	}
	if e.wake == nil {
		s.mu.Unlock()
		// Registered, but no wake: the child's stdin does not exist yet,
		// or it is a --oneshot child or one with no PTY. Worth a line anyway — from
		// the outside this is identical to a wake that was written and ignored,
		// and telling the two apart is the whole reason these logs exist.
		s.logger().Info("wake skipped", "task_id", taskIDHex, "reason", "task has no stdin writer (not wired yet, a --oneshot, or no PTY)")
		return
	}
	if e.waking {
		s.mu.Unlock()
		s.logger().Info("wake skipped", "task_id", taskIDHex, "reason", "in flight")
		return
	}
	now := s.Now()
	if !e.lastWakeAt.IsZero() && now.Sub(e.lastWakeAt) < wakeDebounceWindow {
		since := now.Sub(e.lastWakeAt)
		s.mu.Unlock()
		s.logger().Info("wake skipped", "task_id", taskIDHex, "reason", "debounced", "since_last", since)
		return
	}
	wake := e.wake
	e.waking = true
	s.mu.Unlock()

	form, err := wake()

	s.mu.Lock()
	e.waking = false
	if err == nil {
		if e2, ok := s.reg.get(taskIDHex); ok && e2 != nil {
			e2.lastWakeAt = now
		}
	}
	s.mu.Unlock()
	if err != nil {
		s.logger().Warn("wake write failed", "task_id", taskIDHex, "err", err)
		return
	}
	// The wake reached the agent's stdin. This says the harness did its half —
	// it does NOT say a turn started, which is the agent's decision and not
	// observable from here. That boundary is exactly what the log is for: with
	// this line present and no turn, the next investigation starts at the
	// agent instead of re-reading this file.
	s.logger().Info("wake written", "task_id", taskIDHex, "form", form)
}

// setWake installs the task's wake delivery once its agent's stdin exists.
func (s *Session) setWake(taskIDHex string, wake func() (wakeForm, error)) {
	s.mu.Lock()
	if e, ok := s.reg.get(taskIDHex); ok && e != nil {
		e.wake = wake
	}
	s.mu.Unlock()
}

// ptyWake types the marker into a PTY session and submits it. modes is the
// agent's own, fed from its output by ptyModeTap and read at each wake, since
// an agent can turn bracketed paste on after it starts. With bracketed paste on
// the marker goes as a paste — with the Enter in the same write when
// in.enterWithPaste, else followed by a pause of in.submitDelay and a lone
// Enter. Without it, as text, the pause, then a lone Enter. See the wakeMarker
// comment for why.
func ptyWake(write func([]byte) (int, error), modes *termmode.Tracker, in wakeInput) func() (wakeForm, error) {
	return func() (wakeForm, error) {
		form, text := wakeSplit, []byte(wakeMarker)
		if on, _ := modes.Mode(termmode.BracketedPaste); on {
			if in.enterWithPaste {
				if _, err := write(bracketedWake); err != nil {
					return wakePaste, fmt.Errorf("paste: %w", err)
				}
				return wakePaste, nil
			}
			form, text = wakePasteSplit, []byte(pastedMarker)
		}
		if _, err := write(text); err != nil {
			return form, fmt.Errorf("text: %w", err)
		}
		time.Sleep(in.submitDelay)
		if _, err := write(wakeSubmitByte); err != nil {
			return form, fmt.Errorf("submit: %w", err)
		}
		return form, nil
	}
}

// ptyModeTap feeds a PTY session's output to its mode tracker as it passes. An
// agentexec.Auditor, like streamTap: Stdout is what the agent wrote, before it
// was framed. A PTY has one output, but Stderr is fed too rather than assumed
// empty. Feed keeps no reference to the buffer, which the caller reuses.
type ptyModeTap struct{ modes *termmode.Tracker }

func (t ptyModeTap) Start(string, []string, bool) {}
func (t ptyModeTap) Stdin([]byte)                 {}
func (t ptyModeTap) Stdout(p []byte)              { t.modes.Feed(p) }
func (t ptyModeTap) Stderr(p []byte)              { t.modes.Feed(p) }
func (t ptyModeTap) Exit(error)                   {}

// streamWake hands the marker to an event-stream session as a user turn — the
// neutral protocol's own message, which the adapter turns into its vendor's.
// None of ptyWake's keystroke problem exists here: the line is a message, not
// typing, and it goes in one write. That one write is also what keeps it from
// interleaving with a client's line: the stdin it writes is agentexec's
// io.Pipe, which serialises concurrent Writes whole, and a client's turn
// arrives as one Stdin frame written in one call.
func streamWake(write func([]byte) (int, error)) func() (wakeForm, error) {
	w := streamagent.NewWriter(writeFunc(write))
	return func() (wakeForm, error) {
		return wakeTurn, w.User(streamagent.UserTurn{Text: wakeMarker})
	}
}

// writeFunc adapts agentexec's stdin write callback to an io.Writer.
type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

// interactiveFinished is the TaskFinished an interactive session reports.
// objtrsf returns the child's outcome, which splits three cases that used to be
// one (until then every interactive task reported exit 0 and read Succeeded
// whatever the agent did). ended says the runner itself ended the session — a
// cancel, or its own shutdown: the child's death is then the ordinary end of
// these sessions rather than a crash, whatever code it died with, because
// TaskStore.Finish deliberately overwrites Cancelled with what is reported here
// and anything else would relabel every cancelled task Failed.
func interactiveFinished(id protocol.TaskID, runErr error, code int32, ended bool) protocol.TaskFinished {
	tf := protocol.TaskFinished{TaskId: id}
	switch {
	case runErr == nil && code == 0:
		// Clean exit.
	case ended:
	case code != 0:
		// The agent exited on its own, non-zero. Report the REAL code
		// rather than a flat -1.
		tf.ExitCode = code
		tf.ErrorMessage = []byte("agent exited " + strconv.Itoa(int(code)))
	default:
		// A stream or setup failure, not the agent's own exit.
		tf.ExitCode = -1
		tf.ErrorMessage = []byte("interactive_error: " + runErr.Error())
	}
	return tf
}
