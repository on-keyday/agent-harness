//go:build !js

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// ExecRunOpts carries the caller's three ends.
//
// A nil Stdin closes the child's stdin immediately, which is what a
// non-interactive caller wants: a child that reads stdin then gets EOF rather
// than hanging forever on a pipe nobody will write to.
type ExecRunOpts struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ExecRun runs argv in the task's worktree as its own process and blocks until
// it ends, copying stdout and stderr to opts as they arrive — SEPARATELY, which
// is the whole point of this verb versus `session exec`.
//
// The outcome arrives on a second stream rather than in the frame protocol:
// that protocol has no exit shape, and it is pre-trsf legacy where sideband
// belongs in its own trsf stream.
func (c *Client) ExecRun(ctx context.Context, taskIDHex string, argv []string, opts ExecRunOpts) (ExecRunResult, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return ExecRunResult{}, fmt.Errorf("exec: parse task id: %w", err)
	}
	if len(argv) == 0 {
		return ExecRunResult{}, errors.New("exec: empty command")
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenExecRun}
	body := protocol.ExecRunRequest{TaskId: tid, Argv: buildExecArgv(argv)}
	body.SetStdinEnabled(opts.Stdin != nil)
	req.SetOpenExecRun(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return ExecRunResult{}, err
	}
	r := resp.OpenExecRun()
	if r == nil {
		return ExecRunResult{}, fmt.Errorf("exec: expected OpenExecRun response, got kind=%v", resp.Kind)
	}
	if err := execRunStatusError(taskIDHex, r.Status); err != nil {
		return ExecRunResult{}, err
	}

	data := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.DataStreamId))
	if data == nil {
		return ExecRunResult{}, fmt.Errorf("exec: data stream %d not visible", r.DataStreamId)
	}
	defer data.CloseBoth()

	stream := agentexec.NewCommandExecutionStream(data)
	// Both output pumps must be drained: an undrained demux side backpressures
	// the stream, and the child would stall part-way through its output.
	done := make(chan struct{}, 2)
	go func() { defer func() { done <- struct{}{} }(); copyIfSet(opts.Stdout, stream.Stdout()) }()
	go func() { defer func() { done <- struct{}{} }(); copyIfSet(opts.Stderr, stream.Stderr()) }()
	if opts.Stdin != nil {
		// Close when the caller's stdin runs out: that writes the 0-length
		// Stdin frame the executor reads as "close the child's stdin". Without
		// it a filter like `cat` reads forever and the exec never ends — the
		// command looks hung when it is only waiting for an EOF nobody sends.
		go func() {
			w := stream.Stdin()
			_, _ = io.Copy(w, opts.Stdin)
			if cl, ok := w.(io.Closer); ok {
				_ = cl.Close()
			}
		}()
	}

	<-done
	<-done

	// The outcome stream is looked up HERE, not before the pumps: it carries
	// nothing until the exec ends, and a stream with no bytes on it yet is not
	// resolvable, so waiting for it up front would time out on every exec that
	// outlives the lookup deadline — which is all of them.
	//
	// A RECEIVE stream, read the way ExecRunListWith reads its rows. The server
	// makes it unidirectional on purpose; see handleOpenExecRun.
	ctrl := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(r.ControlStreamId))
	if ctrl == nil {
		return ExecRunResult{}, fmt.Errorf("exec: outcome stream %d never arrived", r.ControlStreamId)
	}

	// It carries exactly one ExecEvent and then EOFs; the EOF is what says the
	// outcome is complete.
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return ExecRunResult{}, err
		}
		chunk, eof, rerr := ctrl.ReadDirect(64 * 1024)
		if rerr != nil {
			return ExecRunResult{}, fmt.Errorf("exec: read outcome: %w", rerr)
		}
		raw = append(raw, chunk...)
		if eof {
			break
		}
	}

	var ev protocol.ExecEvent
	if derr := ev.DecodeExactCopy(raw); derr != nil {
		return ExecRunResult{}, fmt.Errorf("exec: decode outcome: %w", derr)
	}
	return ExecRunResult{ExitCode: ev.ExitCode, Kind: ev.Kind, Detail: string(ev.Detail)}, nil
}

// copyIfSet drains src even when the caller wants nothing written, because an
// undrained side backpressures the whole stream.
func copyIfSet(dst io.Writer, src io.Reader) {
	if dst == nil {
		dst = io.Discard
	}
	_, _ = io.Copy(dst, src)
}

// ExecRunListWith reports the running execs this caller can see.
func (c *Client) ExecRunListWith(ctx context.Context, taskFilter string) ([]protocol.ExecRunInfo, error) {
	var q protocol.ExecRunListRequest
	if taskFilter != "" {
		tid, err := parseTaskIDHex(taskFilter)
		if err != nil {
			return nil, fmt.Errorf("exec ls: parse task id: %w", err)
		}
		q.TaskFilter = tid
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ExecRunList}
	req.SetExecRunList(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.ExecRunList()
	if lr == nil {
		return nil, fmt.Errorf("exec ls: expected ExecRunList response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, errors.New("exec ls: server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("exec ls: stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, rerr := st.ReadDirect(64 * 1024)
		if rerr != nil {
			return nil, fmt.Errorf("exec ls: read: %w", rerr)
		}
		raw = append(raw, data...)
		if eof {
			break
		}
	}
	var body protocol.ExecRunListBody
	if derr := body.DecodeExactCopy(raw); derr != nil {
		return nil, fmt.Errorf("exec ls: decode: %w", derr)
	}
	return body.Execs, nil
}

// ExecRunList is the short-lived-CLI form of ExecRunListWith.
func ExecRunList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.ExecRunInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ExecRunListWith(ctx, taskFilter)
}

// ExecRunKillWith stops one running exec by id.
func (c *Client) ExecRunKillWith(ctx context.Context, id uint64) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ExecRunKill}
	req.SetExecRunKill(protocol.ExecRunKillRequest{ExecId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.ExecRunKill()
	if kr == nil {
		return fmt.Errorf("exec kill: expected ExecRunKill response, got kind=%v", resp.Kind)
	}
	switch kr.Status {
	case protocol.ExecRunStatus_Ok:
		return nil
	case protocol.ExecRunStatus_NotFound:
		// Covers "no such exec" AND "outside your scope", deliberately: an
		// invisible id must not become an existence oracle.
		return fmt.Errorf("exec kill: no such exec %d", id)
	default:
		return fmt.Errorf("exec kill: %s", kr.Status.String())
	}
}

// ExecRunKill is the short-lived-CLI form of ExecRunKillWith.
func ExecRunKill(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.ExecRunKillWith(ctx, id)
}
