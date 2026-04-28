package runner

import (
	"context"
	"encoding/hex"
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

// taskEntry holds the per-task cancellation function and the repo it runs under.
type taskEntry struct {
	cancel   context.CancelFunc
	repoPath string
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

	// ServerCID, Hostname, WSPath are required for HARNESS_* env injection at
	// task spawn time. Filled from Config in connect.go.
	ServerCID objproto.ConnectionID
	Hostname  string
	WSPath    string

	mu    sync.Mutex
	tasks map[string]*taskEntry       // taskID (hex) → cancel + repo
	wms   map[string]*WorktreeManager // repoPath → WorktreeManager

	// testHookHandleAssign is called at the start of handleAssign in tests to
	// inject faults (e.g. panics). It is nil in production.
	testHookHandleAssign func()
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
func (s *Session) handleAssign(ctx context.Context, req *protocol.AssignTask) {
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	topic := topics.TaskLog(taskIDHex)

	// Validate repo path from the request.
	repoPath := string(req.RepoPath)

	// Step 1: Send TaskAccepted — signals we are committed even if worktree fails.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
		m.SetTaskAccepted(protocol.TaskAccepted{TaskId: req.TaskId})
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	finishWithError := func(code int32, reason string) {
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{
			TaskId:   req.TaskId,
			ExitCode: code,
			DiffInfo: []byte(reason),
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

	// If the request didn't provide a RepoPath, fall back to AllowedRoots[0]
	// (for backward compatibility with tests that don't set req.RepoPath).
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

	// Step 2: Create worktree.
	wm := s.getWorktreeManager(repoPath)
	dir, err := wm.Create(taskIDHex)
	if err != nil {
		finishWithError(-1, "worktree_error: "+err.Error())
		return
	}

	// Step 3: Send TaskStarted.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskStarted}
		ts := protocol.TaskStarted{TaskId: req.TaskId}
		ts.SetWorktreeDir([]byte(dir))
		m.SetTaskStarted(ts)
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	if err := WriteAgentSettings(dir); err != nil {
		s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
	}

	// Build HARNESS_* env vars for the subprocess.
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:  s.ServerCID,
		RunnerID:   s.Sender.ID(),
		TaskID:     req.TaskId,
		RepoPath:   repoPath,
		Hostname:   s.Hostname,
		WSPath:     s.WSPath,
		AuthTicket: req.AuthTicket,
	})

	// Step 4: Execute the process, publishing log lines to the task log topic.
	proc := &Process{
		ClaudeBin: s.ClaudeBin,
		CWD:       dir,
		Timeout:   s.Timeout,
		ExtraArgs: s.ExtraClaudeArgs,
		Env:       env,
	}
	logSink := func(data []byte) {
		_ = s.Sender.Publish(topic, data)
	}
	exit, runErr := proc.Run(taskCtx, string(req.Prompt), logSink)

	// Step 5: Send TaskFinished.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{
			TaskId:   req.TaskId,
			ExitCode: int32(exit),
		}
		if runErr != nil {
			tf.ExitCode = -1
			tf.DiffInfo = []byte("process_error: " + runErr.Error())
		}
		m.SetTaskFinished(tf)
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Step 6: Clean up the worktree directory. The branch ref harness/<id>
	// is intentionally retained so any commits claude made in the worktree
	// are still reachable via `git checkout harness/<id>`. Failure to remove
	// is logged but does not affect task lifecycle (TaskFinished already
	// sent); a subsequent runner start can scan stale worktrees if needed.
	if err := wm.Remove(taskIDHex); err != nil {
		s.logger().Warn("worktree remove failed", "task_id", taskIDHex, "err", err)
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

	finishWithError := func(code int32, reason string) {
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		tf := protocol.TaskFinished{TaskId: oer.TaskId, ExitCode: code}
		tf.DiffInfo = []byte(reason)
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
	s.tasks[taskIDHex] = &taskEntry{cancel: cancel, repoPath: repoPath}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.tasks, taskIDHex)
		s.mu.Unlock()
	}()

	// Step 2: worktree.
	wm := s.getWorktreeManager(repoPath)
	dir, err := wm.Create(taskIDHex)
	if err != nil {
		_ = stream.CloseBoth()
		finishWithError(-1, "worktree_error: "+err.Error())
		return
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
	if err := WriteAgentSettings(dir); err != nil {
		log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
	}

	// Build HARNESS_* env vars for the subprocess.
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:  s.ServerCID,
		RunnerID:   s.Sender.ID(),
		TaskID:     oer.TaskId,
		RepoPath:   repoPath,
		Hostname:   s.Hostname,
		WSPath:     s.WSPath,
		AuthTicket: oer.AuthTicket,
	})

	// Step 4: spawn claude under PTY, hand the stream to exec.
	// ExecuteCommand defers stream.CloseBoth() so we don't double-close here.
	runErr := agentexec.ExecuteCommand(taskCtx, stream, log, s.ClaudeBin, s.ExtraClaudeArgs, dir, true, env)

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
			tf.DiffInfo = []byte("interactive_error: " + runErr.Error())
		}
		m.SetTaskFinished(tf)
		_ = s.Sender.Send(m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}))
	}

	// Step 6: Clean up the worktree directory. See handleAssign for the
	// rationale on why the branch ref is retained.
	if err := wm.Remove(taskIDHex); err != nil {
		log.Warn("worktree remove failed", "task_id", taskIDHex, "err", err)
	}
}
