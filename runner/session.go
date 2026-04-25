package runner

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
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
	RepoPath  string
	ClaudeBin string
	Timeout   time.Duration
	Sender    Sender
	Now       func() time.Time

	wm *WorktreeManager // set on first use
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
	proc := &Process{
		ClaudeBin: s.ClaudeBin,
		CWD:       dir,
		Timeout:   s.Timeout,
	}
	// Serialize concurrent log-sink calls so that the first Publish (which blocks
	// to establish the pubsub stream) completes before a second concurrent call can
	// also attempt stream setup, which would deadlock in connSender.Publish.
	var logMu sync.Mutex
	logSink := func(data []byte) {
		logMu.Lock()
		defer logMu.Unlock()
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
