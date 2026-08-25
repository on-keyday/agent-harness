package runner

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/trsf"
)

// execRunDir returns the directory an exec runs in, and whether it is there.
//
// Two shapes, exactly as gitSourceFor resolves them. Normally it is the task's
// own worktree. Under NoWorktree the runner never made one and every task runs
// directly in the repo, so the repo IS the task's working directory — a runner
// configured that way used to refuse every exec, because this only knew the
// first shape.
//
// isWorktreeRoot rather than a bare Stat, for the reason git_query gives: an
// orphan directory left by a crashed cleanup still exists and holds no .git,
// and a command run inside it would act on something that is not the task's
// tree while reporting success.
//
// Existence is the WHOLE gate — not the task's status. A task that ended with
// uncommitted work KEEPS its worktree (RemoveIfClean declines to remove a dirty
// one) and running `git status` or `make test` in that tree is exactly what an
// operator wants then; one that ended clean had it removed and has nowhere to
// run. git_query's fallback for that case — run in the repo against the
// retained harness/<id> branch — is deliberately NOT copied: it is safe for a
// read-only git view and wrong for an arbitrary command, which would silently
// act on a different tree than the caller named.
func (s *Session) execRunDir(repoPath, taskIDHex string) (string, bool) {
	if s.NoWorktree {
		st, err := os.Stat(repoPath)
		return repoPath, err == nil && st.IsDir()
	}
	wt := filepath.Join(repoPath, ".harness-worktrees", taskIDHex)
	return wt, isWorktreeRoot(wt)
}

// execCancels holds the cancel func of every exec this runner is running, so a
// close_exec_run request can stop one. Keyed by the server-assigned exec id.
type execCancels struct {
	mu sync.Mutex
	m  map[uint64]context.CancelFunc
}

func (c *execCancels) put(id uint64, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[uint64]context.CancelFunc{}
	}
	c.m[id] = cancel
}

func (c *execCancels) take(id uint64) context.CancelFunc {
	c.mu.Lock()
	defer c.mu.Unlock()
	cancel := c.m[id]
	delete(c.m, id)
	return cancel
}

// handleExecRun runs one argv in a task's worktree and reports the outcome.
//
// The data plane is the call the event-stream task kind already makes
// (runner/streamtask.go): ptyEnabled=false, so stdout and stderr arrive as
// separate frame types instead of interleaved on one PTY. That separation is
// the entire reason this verb exists rather than reusing `session exec`.
func (s *Session) handleExecRun(ctx context.Context, req *protocol.RunnerExecRunRequest) {
	log := s.logger()
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	repoPath := string(req.RepoPath)

	finish := func(kind protocol.ExecEventKind, code int32, detail string) {
		m := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_ExecRunFinished}
		body := protocol.ExecRunFinished{ExecId: req.ExecId, ExitCode: code, Kind: kind}
		body.Detail = []byte(detail)
		m.SetExecRunFinished(body)
		data := m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
		_ = s.Sender.Send(data)
	}

	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("exec_run: stream not visible", "stream_id", req.StreamId)
		finish(protocol.ExecEventKind_Failed, -1, "exec stream not visible")
		return
	}

	// The repo path came from the SERVER's task record, so re-validate it here
	// the way buildGitResult does: the runner decides what it will run in.
	// AllowedRoots empty means an unconfigured Session (tests).
	if len(s.AllowedRoots) > 0 && !s.repoAllowed(repoPath) {
		_ = stream.CloseBoth()
		finish(protocol.ExecEventKind_Failed, -1, "repo is not under this runner's --roots: "+repoPath)
		return
	}
	dir, ok := s.execRunDir(repoPath, taskIDHex)
	if !ok {
		_ = stream.CloseBoth()
		finish(protocol.ExecEventKind_Failed, -1, "no worktree for task "+taskIDHex)
		return
	}
	if req.Argv.ArgvLen == 0 || len(req.Argv.Argv) == 0 {
		_ = stream.CloseBoth()
		finish(protocol.ExecEventKind_Failed, -1, "empty argv")
		return
	}

	argv := make([]string, 0, len(req.Argv.Argv))
	for i := range req.Argv.Argv {
		argv = append(argv, string(req.Argv.Argv[i].Arg))
	}
	// The child sees what a task in this tree sees. Needing something else is
	// what `env VAR=x <cmd>` in the argv is for.
	env := BuildAgentEnv(AgentEnvSpec{
		ServerCID:  s.ServerCID,
		RunnerID:   s.runnerCanonicalConnID(),
		TaskID:     req.TaskId,
		RepoPath:   repoPath,
		Hostname:   s.Hostname,
		WSPath:     s.WSPath,
		AuthTicket: req.AuthTicket,
		BinDir:     s.BinDir,
		PSK:        s.PSK,
		ProxyVia:   s.ProxyVia,
	})
	env = append(env, AgentCwdEnv(dir)...)

	// A cancellable context so close_exec_run can stop the child. Closing the
	// stream would not: the frame pump reads EOF and ends its input goroutine
	// while the process runs on.
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.execCancels.put(req.ExecId, cancel)
	defer s.execCancels.take(req.ExecId)

	var exitCode int32 = -1
	kind := protocol.ExecEventKind_Failed
	var detail string
	runErr := agentexec.ExecuteCommandWithOption(execCtx, stream, log,
		argv[0], argv[1:], dir,
		false, // no PTY: separate stdout and stderr is the point
		env,
		agentexec.ExecuteOption{
			OnProcessExit: func(st *os.ProcessState, werr error) {
				// The CHILD's own code, read from ProcessState rather than
				// inferred from an error: the errgroup inside agentexec
				// surfaces whichever goroutine failed first, and a teardown
				// error routinely beats the exit status to it.
				if st != nil {
					exitCode = int32(st.ExitCode())
					kind = protocol.ExecEventKind_Exited
					return
				}
				if werr != nil {
					detail = werr.Error()
				}
			},
		})
	if kind != protocol.ExecEventKind_Exited {
		// The child never ran. Report WHY rather than inventing an exit code:
		// 127 is a shell's convention and there is no shell in this path.
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = fmt.Sprintf("exec %q did not start", argv[0])
		}
		if execCtx.Err() != nil {
			kind = protocol.ExecEventKind_Killed
			detail = "cancelled"
		}
	}
	finish(kind, exitCode, detail)
}

// handleCloseExecRun cancels a running exec. Unknown ids are ignored: the exec
// may have finished between the operator's kill and this arriving.
func (s *Session) handleCloseExecRun(req *protocol.CloseExecRunRequest) {
	if cancel := s.execCancels.take(req.ExecId); cancel != nil {
		cancel()
	}
}
