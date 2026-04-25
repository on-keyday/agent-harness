package runner

import (
	"context"
	"encoding/hex"
	"log/slog"
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

type Session struct {
	RepoPath        string
	ClaudeBin       string
	ExtraClaudeArgs []string // forwarded to Process.ExtraArgs (e.g. --dangerously-skip-permissions)
	Timeout         time.Duration
	Sender          Sender
	Streams         peer.BidirectionalStreamLookup // optional; required for handleOpenExec
	Logger          *slog.Logger                   // optional; defaults to slog.Default()
	Now             func() time.Time

	wm *WorktreeManager // set on first use
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
func (s *Session) handleAssign(ctx context.Context, req *protocol.AssignTask) {
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	topic := topics.TaskLog(taskIDHex)

	// Step 1: Send TaskAccepted — signals we are committed even if worktree fails.
	{
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskAccepted}
		m.SetTaskAccepted(protocol.TaskAccepted{TaskId: req.TaskId})
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	// Step 2: Create worktree.
	if s.wm == nil {
		s.wm = &WorktreeManager{Repo: s.RepoPath}
	}
	dir, err := s.wm.Create(taskIDHex)
	if err != nil {
		// Worktree creation failure → TaskFinished with error.
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
		m.SetTaskFinished(protocol.TaskFinished{
			TaskId:   req.TaskId,
			ExitCode: -1,
			DiffInfo: []byte("worktree_error: " + err.Error()),
		})
		data := m.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
		_ = s.Sender.Send(data)
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

	// Step 4: Execute the process, publishing log lines to the task log topic.
	// peer.Conn.Publish (under the Sender adapter) handles concurrent first-
	// callers via per-topic leader/follower, so no caller-side serialization
	// is needed here.
	proc := &Process{
		ClaudeBin: s.ClaudeBin,
		CWD:       dir,
		Timeout:   s.Timeout,
		ExtraArgs: s.ExtraClaudeArgs,
	}
	logSink := func(data []byte) {
		_ = s.Sender.Publish(topic, data)
	}
	exit, runErr := proc.Run(ctx, string(req.Prompt), logSink)

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

	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(oer.StreamId))
	if stream == nil {
		finishWithError(-1, "runner: server-allocated exec stream not visible")
		return
	}

	// Step 2: worktree.
	if s.wm == nil {
		s.wm = &WorktreeManager{Repo: s.RepoPath}
	}
	dir, err := s.wm.Create(taskIDHex)
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

	// Step 4: spawn claude under PTY, hand the stream to exec.
	// ExecuteCommand defers stream.CloseBoth() so we don't double-close here.
	runErr := agentexec.ExecuteCommand(ctx, stream, log, s.ClaudeBin, s.ExtraClaudeArgs, dir, true)

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
}

