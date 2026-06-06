package runner

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// wakeDebounceWindow is the minimum interval between successive wake
// writes for one task. Coalescing rationale lives in
// docs/superpowers/specs/2026-04-29-agent-wake-and-origin-design.md.
const wakeDebounceWindow = 1500 * time.Millisecond

// wakeMarker is the body of the synthetic prompt written to the agent's
// stdin. The leading "<harness:agentboard-wake>" tag is machine-detectable
// for future hook post-processing; the rest is action-agnostic so the LLM
// is not forced into a reply when the message does not warrant one.
//
// IMPORTANT: wakeMarker has NO trailing newline / carriage return. The
// submit keystroke is sent as a separate write after wakeSubmitDelay (see
// WakeStdin). claude code's UI is built with Ink (a React-based terminal
// renderer); empirically, when the runner writes the marker text and a
// trailing "\r" or "\n" in a single syscall, Ink treats the whole chunk
// as paste content and the trailing byte becomes a literal newline inside
// the input box rather than firing the Enter / submit handler. The
// human still has to press Enter manually. Splitting the write — text,
// short pause, lone "\r" — makes the second write parse as a real
// keystroke and the prompt is submitted automatically.
const wakeMarker = "<harness:agentboard-wake> new agentboard message(s) — review and act as appropriate"

// wakeSubmitDelay is the gap between the marker text write and the lone
// submit byte write. Long enough for Ink's input parser to flush the
// first chunk into its text-input state before the trailing keystroke
// arrives, short enough not to be perceptible. Tuned by feel; see
// the wakeMarker comment for why a single combined write fails.
const wakeSubmitDelay = 100 * time.Millisecond

// wakeSubmitByte is the byte sequence treated as Enter by Ink-based TUIs
// when delivered as a standalone PTY write.
var wakeSubmitByte = []byte{'\r'}

// Sender is the runner's outbound interface to the server. Decoupled from concrete
// trsf.Transport / objproto.Connection so tests can use a mock.
type Sender interface {
	// Send transmits a control-frame message. The bytes are already wire-prefixed.
	Send(data []byte) error
	// ID is the runner's connection ID, used by the server-side dispatch.
	ID() objproto.ConnectionID
	// Publish writes a chunk of data to the given pubsub topic.
	Publish(topic string, data []byte) error
}

// taskEntry holds the per-task cancellation function, the repo it runs
// under, and the (debounced) stdin-wake state.
type taskEntry struct {
	cancel     context.CancelFunc
	repoPath   string
	detachable bool

	// wakeWrite is the closure handed to OnStdinWriter — writes bytes
	// directly to the running claude's stdin pipe. nil until the agent
	// process is spawned.
	wakeWrite func([]byte) (int, error)

	// lastWakeAt is the most recent time WakeStdin actually wrote to
	// stdin. Wakes within wakeDebounceWindow are dropped (the agent's
	// next inbox call will pick up everything via --since-last cursor).
	lastWakeAt time.Time
}

// Session manages the runner's task lifecycle. It is created once per connection
// and handles concurrent tasks through its internal maps.
type Session struct {
	AllowedRoots    []string // absolute paths this runner is allowed to serve
	ClaudeBin       string
	ExtraClaudeArgs []string // forwarded to Process.ExtraArgs (e.g. --dangerously-skip-permissions)
	Timeout         time.Duration
	Sender          Sender
	Streams         peer.BidirectionalStreamLookup // optional; required for handleOpenExec
	Logger          *slog.Logger                   // optional; defaults to slog.Default()
	Now             func() time.Time

	// creator makes bidi streams toward the server. Set to pc.Transport() for
	// live runner connections; required by remote port-forward (one stream per
	// accepted connection). nil in tests that don't exercise remote forward.
	creator bidiStreamCreator
	// rforwards tracks active remote port-forward listeners by forwardId so a
	// ClosePortForward request can shut them down. Lazily created (see
	// remoteForwardListeners / port_forward.go).
	rforwards *remoteForwardListeners

	// ServerCID, Hostname, WSPath, BinDir are required for HARNESS_* env
	// injection at task spawn time. Filled from Config in connect.go.
	// BinDir is prepended to the agent's PATH so harness-cli is reachable
	// from within the task worktree (which is a different worktree than
	// the runner's binary location).
	ServerCID objproto.ConnectionID
	Hostname  string
	WSPath    string
	BinDir    string
	// PSK, when non-nil, is forwarded to the agent subprocess via
	// HARNESS_PSK so harness-cli invocations from inside the agent can
	// authenticate against the PSK-protected server.
	PSK []byte

	// ProxyVia, when non-empty, is propagated into spawned agent env as
	// HARNESS_PROXY_VIA_RUNNER (Phase B). Set by ListenAndServe in listen mode.
	ProxyVia string

	// Endpoint is the objproto.Endpoint this session's peer.Conn lives on.
	// Required by handleEstablishRelay to install eager SetProxy entries.
	// Set by Connect (dial mode) and by handleServerConn (listen mode).
	// Written by the same goroutine that subsequently calls OnConnect, so no
	// mutex is needed; dispatchRunnerRequest cannot observe a nil Endpoint.
	Endpoint objproto.Endpoint

	// runnerCanonicalID is the RunnerID the server keys this runner as in
	// its registry / agentboard ticket store. Filled from RunnerHelloResponse
	// (server → runner) before any AssignTask. Reads/writes are guarded by
	// mu — the wire ordering guarantees Set happens before any handleAssign
	// reads, but the lock provides a memory barrier for the cross-goroutine
	// happens-before.
	runnerCanonicalID protocol.RunnerID

	// NoWorktree, when true, makes handleAssign / handleOpenExec skip the
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

	mu    sync.Mutex
	tasks map[string]*taskEntry       // taskID (hex) → cancel + repo
	wms   map[string]*WorktreeManager // repoPath → WorktreeManager

	// testHookHandleAssign is called at the start of handleAssign in tests to
	// inject faults (e.g. panics). It is nil in production.
	testHookHandleAssign func()

	// chainedRelayPendingMu guards chainedRelayPendingCh. One-at-a-time
	// invariant: at most one RequestChainedRelay may be in flight per
	// session at any time (spec Decision 2). BeginChainedRelay returns an
	// error if another is already pending; DeliverChainedRelayResponse
	// sends to the channel and clears it.
	chainedRelayPendingMu sync.Mutex
	chainedRelayPendingCh chan protocol.ChainedRelayResponse // nil when none pending
}

// SetRunnerCanonicalID stores the RunnerID the server reports for this
// runner connection (via RunnerHelloResponse). Called from
// dispatchRunnerRequest synchronously before any AssignTask is dispatched.
func (s *Session) SetRunnerCanonicalID(rid protocol.RunnerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runnerCanonicalID = rid
}

// runnerCanonicalConnID returns the runner's canonical RunnerID, converted to
// objproto.ConnectionID format for embedding in HARNESS_RUNNER_ID. Returns the
// zero ConnectionID (which stringifies as ":invalid AddrPort-0") if the server
// has not yet sent RunnerHelloResponse — agent Hello validation will then fail
// with UnknownTask, surfacing the missing handshake clearly rather than
// silently sending the server's own CID.
func (s *Session) runnerCanonicalConnID() objproto.ConnectionID {
	s.mu.Lock()
	rid := s.runnerCanonicalID
	s.mu.Unlock()
	return protocol.RunnerIDToConnID(rid)
}

// initMaps initialises the internal maps if they have not been set yet.
// Must be called with s.mu held.
func (s *Session) initMaps() {
	if s.tasks == nil {
		s.tasks = make(map[string]*taskEntry)
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

// repoAllowed reports whether repoPath is under at least one of AllowedRoots.
// Uses protocol.IsUnderRoot for consistent semantics with the server side.
func (s *Session) repoAllowed(repoPath string) bool {
	for _, root := range s.AllowedRoots {
		if protocol.IsUnderRoot(root, repoPath) {
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
	_, ok := s.tasks[hex.EncodeToString(t.Id[:])]
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

// handleAssign performs the full lifecycle for one assigned task:
//  1. TaskAccepted control message
//  2. Worktree creation (failure → TaskFinished with error info)
//  3. TaskStarted control message
//  4. Process exec, with each stdout/stderr line published to task.<id>.log
//  5. TaskFinished control message
//
// Errors during steps 2 or 4 are conveyed via the TaskFinished message's exit code and
// DiffInfo (textual error). The runner does NOT retry; the server can re-dispatch.
//
// A per-task context derived from ctx is used so CancelTask can cancel individual
// tasks without affecting others. Panics are recovered and reported as TaskFinished.
// handleAssign drives one assigned task through its full lifecycle.
//
// taskID is the wire-level TaskID (carried inline in the AssignTask
// envelope so the runner can correlate immediately, even before the body
// stream is drained). body holds the rest of the assignment payload —
// auth_ticket, repo_path, prompt, extra_args — read from a server-
// initiated trsf send-stream by dispatchRunnerRequest before this
// function is invoked.
func (s *Session) handleAssign(ctx context.Context, taskID protocol.TaskID, body *protocol.AssignTaskBody) {
	taskIDHex := hex.EncodeToString(taskID.Id[:])
	topic := topics.TaskLog(taskIDHex)

	// Validate repo path from the body.
	repoPath := string(body.RepoPath)

	// Step 1: Send TaskAccepted — signals we are committed even if worktree fails.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
		m.SetTaskAccepted(protocol.TaskAccepted{TaskId: taskID})
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	finishWithError := func(code int32, reason string) {
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{
			TaskId:       taskID,
			ExitCode:     code,
			ErrorMessage: []byte(reason),
		}
		m.SetTaskFinished(tf)
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Panic recovery: report as TaskFinished so the server doesn't wait forever.
	// Must be deferred BEFORE the test hook so that hook-injected panics are caught.
	defer func() {
		if r := recover(); r != nil {
			s.logger().Error("handleAssign panic", "task_id", taskIDHex, "panic", r)
			finishWithError(-1, "runner_panic")
		}
	}()

	// testHookHandleAssign runs after TaskAccepted and the recover defer are in place,
	// so that hook-injected panics are caught and result in a TaskFinished message.
	if s.testHookHandleAssign != nil {
		s.testHookHandleAssign()
	}

	// Gate: check repo is under an allowed root (skip check if AllowedRoots is empty
	// so existing tests that don't set AllowedRoots continue to work).
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repoPath) {
		finishWithError(-1, "repo_not_allowed: "+repoPath)
		return
	}

	// If the body didn't provide a RepoPath, fall back to AllowedRoots[0]
	// (for backward compatibility with tests that don't set body.RepoPath).
	if repoPath == "" && len(s.AllowedRoots) > 0 {
		repoPath = s.AllowedRoots[0]
	}

	// Register per-task cancellable context.
	taskCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.initMaps()
	s.tasks[taskIDHex] = &taskEntry{cancel: cancel, repoPath: repoPath}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.tasks, taskIDHex)
		s.mu.Unlock()
	}()

	// Step 2: Create worktree (skipped in NoWorktree mode — agent runs in repoPath directly).
	var dir string
	var wm *WorktreeManager
	if s.NoWorktree {
		dir = repoPath
		s.logger().Info("no-worktree mode: using repo path as cwd", "task_id", taskIDHex, "repo", repoPath)
	} else {
		wm = s.getWorktreeManager(repoPath)
		d, err := wm.Create(taskIDHex)
		if err != nil {
			finishWithError(-1, "worktree_error: "+err.Error())
			return
		}
		dir = d
	}

	// Step 3: Send TaskStarted.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskStarted}
		ts := protocol.TaskStarted{TaskId: taskID}
		ts.SetWorktreeDir([]byte(dir))
		m.SetTaskStarted(ts)
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	// In NoWorktree mode this is skipped by default to avoid polluting the
	// user's repo; ForceInjectHarnessSettings re-enables it.
	if !s.NoWorktree || s.ForceInjectHarnessSettings {
		if err := WriteAgentSettings(dir); err != nil {
			s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir); err != nil {
			s.logger().Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}

	// Build HARNESS_* env vars for the subprocess. RunnerID is the canonical
	// value the server keys this runner as (from RunnerHelloResponse), NOT
	// s.Sender.ID() — the latter is the peer's symmetric ConnectionID, which
	// is the server's own CID from the runner's vantage.
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:  s.ServerCID,
		RunnerID:   s.runnerCanonicalConnID(),
		TaskID:     taskID,
		RepoPath:   repoPath,
		Hostname:   s.Hostname,
		WSPath:     s.WSPath,
		AuthTicket: body.AuthTicket,
		BinDir:     s.BinDir,
		PSK:        s.PSK,
		ProxyVia:   s.ProxyVia,
	})

	// Step 4: Execute the process, publishing log lines to the task log topic.
	// Args order: runner-global baseline first, per-task extras appended last
	// so that a per-task --resume / --add-dir / etc. wins on conflict (claude
	// flags are largely last-wins).
	proc := &Process{
		ClaudeBin: s.ClaudeBin,
		CWD:       dir,
		Timeout:   s.Timeout,
		ExtraArgs: mergeExtraArgs(s.ExtraClaudeArgs, body.ExtraArgs.AsStrings()),
		Env:       env,
		OnStdinWriter: func(write func([]byte) (int, error)) {
			s.mu.Lock()
			if e := s.tasks[taskIDHex]; e != nil {
				e.wakeWrite = write
			}
			s.mu.Unlock()
		},
	}
	logSink := func(data []byte) {
		_ = s.Sender.Publish(topic, data)
	}
	exit, runErr := proc.Run(taskCtx, string(body.Prompt), logSink)

	// Step 5: Send TaskFinished.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{
			TaskId:   taskID,
			ExitCode: int32(exit),
		}
		if runErr != nil {
			tf.ExitCode = -1
			tf.ErrorMessage = []byte("process_error: " + runErr.Error())
		}
		m.SetTaskFinished(tf)
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Step 6: Conditionally clean up the worktree directory. The branch ref
	// harness/<id> is intentionally retained so any commits claude made are
	// reachable via `git checkout harness/<id>`. The directory itself is
	// removed only when `git status --porcelain` (excluding harness-injected
	// paths) is empty — uncommitted in-flight work is preserved across the
	// resume boundary, and the long-tail cleanup is left to `harness-cli
	// prune-local`. Status / remove errors are logged but never bubble up:
	// TaskFinished is already sent and the lifecycle must complete.
	if !s.NoWorktree {
		switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
		case r.StatusErr != nil:
			s.logger().Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
		case !r.Removed:
			s.logger().Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
		}
	}
}

// handleOpenExec is the runner-side counterpart of the server's
// handleOpenInteractive. The server has already created a bidi stream
// toward us and stored its id in oer.StreamId; we look it up and feed it
// to exec.ExecuteCommand for an interactive PTY claude session in a fresh
// worktree.
//
// Lifecycle messages mirror handleAssign:
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
		_ = s.Sender.Send(m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
	}

	log.Info("handleOpenExec", "task_id", taskIDHex, "repo", repoPath, "detachable", oer.Detachable())

	finishWithError := func(code int32, reason string) {
		log.Error("handleOpenExec: "+reason, "task_id", taskIDHex, "repo", repoPath)
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{TaskId: oer.TaskId, ExitCode: code}
		tf.ErrorMessage = []byte(reason)
		m.SetTaskFinished(tf)
		_ = s.Sender.Send(m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
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

	// Gate: check repo is under an allowed root.
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repoPath) {
		_ = stream.CloseBoth()
		finishWithError(-1, "repo_not_allowed: "+repoPath)
		return
	}

	// Fallback if RepoPath not set.
	if repoPath == "" && len(s.AllowedRoots) > 0 {
		repoPath = s.AllowedRoots[0]
	}

	// Register per-task cancellable context.
	taskCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.initMaps()
	s.tasks[taskIDHex] = &taskEntry{cancel: cancel, repoPath: repoPath, detachable: oer.Detachable()}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.tasks, taskIDHex)
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
			_ = stream.CloseBoth()
			finishWithError(-1, "worktree_error: "+err.Error())
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
		_ = s.Sender.Send(m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
	}

	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	// Skipped in NoWorktree mode by default; ForceInjectHarnessSettings overrides.
	if !s.NoWorktree || s.ForceInjectHarnessSettings {
		if err := WriteAgentSettings(dir); err != nil {
			log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir); err != nil {
			log.Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}

	// Build HARNESS_* env vars for the subprocess. See handleAssign comment.
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:  s.ServerCID,
		RunnerID:   s.runnerCanonicalConnID(),
		TaskID:     oer.TaskId,
		RepoPath:   repoPath,
		Hostname:   s.Hostname,
		WSPath:     s.WSPath,
		AuthTicket: oer.AuthTicket,
		BinDir:     s.BinDir,
		PSK:        s.PSK,
		ProxyVia:   s.ProxyVia,
	})

	// Step 4: spawn claude under PTY, hand the stream to exec.
	// ExecuteCommandWithOption defers stream.CloseBoth() so we don't double-close here.
	// Args order matches handleAssign: runner-global baseline first, per-task extras appended.
	mergedArgs := mergeExtraArgs(s.ExtraClaudeArgs, oer.ExtraArgs.AsStrings())
	runErr := agentexec.ExecuteCommandWithOption(taskCtx, stream, log, s.ClaudeBin, mergedArgs, dir, true, env, agentexec.ExecuteOption{
		OnStdinWriter: func(write func([]byte) (int, error)) {
			s.mu.Lock()
			if e := s.tasks[taskIDHex]; e != nil {
				e.wakeWrite = write
			}
			s.mu.Unlock()
		},
	})

	if runErr != nil {
		log.Error("ExecuteCommand error", "task_id", taskIDHex, "error", runErr)
	}

	// Step 5: TaskFinished.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{TaskId: oer.TaskId}
		if runErr != nil {
			// Could be claude's non-zero exit (passed up by errgroup as an
			// *exec.ExitError) or a stream/setup error. Bucket as Failed.
			tf.ExitCode = -1
			tf.ErrorMessage = []byte("interactive_error: " + runErr.Error())
		}
		m.SetTaskFinished(tf)
		_ = s.Sender.Send(m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
	}

	// Step 6: Conditionally clean up the worktree directory. See handleAssign
	// for the rationale on why the branch ref is retained and why the dir is
	// kept when `git status --porcelain` shows non-injected uncommitted work.
	if !s.NoWorktree {
		switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
		case r.StatusErr != nil:
			log.Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
		case !r.Removed:
			log.Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
		}
	}
}

// WakeStdin writes the wake marker to the agent's stdin pipe for the given
// task, debounced per task to one write per wakeDebounceWindow. Safe to
// call from any goroutine. No-op if the task is unknown or its agent
// process has not yet wired its stdin via OnStdinWriter.
//
// Coalescing rationale: subsequent agentboard messages within the window
// are not lost — they are read by the agent's next `agent inbox
// --since-last`, which uses a persisted cursor.
func (s *Session) WakeStdin(taskIDHex string) {
	s.mu.Lock()
	e, ok := s.tasks[taskIDHex]
	if !ok || e == nil || e.wakeWrite == nil {
		s.mu.Unlock()
		return
	}
	now := s.Now()
	if !e.lastWakeAt.IsZero() && now.Sub(e.lastWakeAt) < wakeDebounceWindow {
		s.mu.Unlock()
		return
	}
	write := e.wakeWrite
	s.mu.Unlock()

	// Split write — text first, brief pause, lone Enter byte. See the
	// wakeMarker / wakeSubmitDelay comments for why a single combined
	// write fails against Ink-based TUIs.
	if _, err := write([]byte(wakeMarker)); err != nil {
		s.logger().Warn("wake stdin write (text) failed", "task_id", taskIDHex, "err", err)
		return
	}
	time.Sleep(wakeSubmitDelay)
	if _, err := write(wakeSubmitByte); err != nil {
		s.logger().Warn("wake stdin write (submit) failed", "task_id", taskIDHex, "err", err)
		return
	}

	s.mu.Lock()
	if e2, ok := s.tasks[taskIDHex]; ok && e2 != nil {
		e2.lastWakeAt = now
	}
	s.mu.Unlock()
}
