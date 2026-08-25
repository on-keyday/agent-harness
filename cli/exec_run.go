package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	// The child's stdin is closed when the caller's runs out — or IMMEDIATELY
	// when there is no caller stdin at all. Closing writes the 0-length Stdin
	// frame the executor reads as "close the child's stdin"; without it the
	// child holds a pipe nobody will ever write to.
	//
	// The no-stdin case is not hypothetical politeness. The TUI and the WebUI
	// pass no Stdin, and `exec <task> -- bash` from either hung forever with the
	// shell waiting on an EOF that was never coming — measured against a live
	// runner, the child still alive minutes later.
	//
	// A CURRENT runner needs none of this: stdin_enabled=0 makes it give the
	// child /dev/null, which is strictly better (no window in which stdin is
	// open and empty). This close stays as the cover for a runner that predates
	// that — the deployment order is server first, THEN runners, so a new client
	// against an old runner is a state the fleet passes through. The runner
	// drops a 0-length frame silently for exactly this reason.
	closeChildStdin := func(w io.Writer) {
		if cl, ok := w.(io.Closer); ok {
			_ = cl.Close()
		}
	}
	if opts.Stdin != nil {
		go func() {
			w := stream.Stdin()
			_, _ = io.Copy(w, opts.Stdin)
			closeChildStdin(w)
		}()
	} else {
		closeChildStdin(stream.Stdin())
	}

	// Cancellation has to reach the PUMPS. They block on the stream, not on ctx,
	// so a cancelled context alone leaves this call parked for the whole life of
	// the command — which is what "Ctrl-C does not stop it" looked like: the
	// interrupt handler printed, the process stayed, and the child ran on.
	// Returning here runs the deferred CloseBoth, which ends both pumps; the
	// caller's connection close is what then stops the child server-side.
	finished := make(chan struct{})
	go func() {
		<-done
		<-done
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		return ExecRunResult{}, ctx.Err()
	}

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

// ExecRunResult is how one exec ended.
type ExecRunResult struct {
	// ExitCode is the child's own for Kind==exited, and -1 otherwise — a death
	// by signal has no exit code, and inventing 127 would be a shell's
	// convention applied where there is no shell.
	ExitCode int32
	Kind     protocol.ExecEventKind
	Detail   string
}

// buildExecArgv wraps a command line for the wire, one element per argument.
func buildExecArgv(argv []string) protocol.ExecArgv {
	var out protocol.ExecArgv
	for _, a := range argv {
		var e protocol.ExecArg
		e.SetArg([]byte(a))
		out.Argv = append(out.Argv, e)
	}
	out.ArgvLen = uint16(len(out.Argv))
	return out
}

// ExecArgvStrings unwraps a wire argv back to a plain command line.
func ExecArgvStrings(a protocol.ExecArgv) []string {
	out := make([]string, 0, len(a.Argv))
	for i := range a.Argv {
		out = append(out, string(a.Argv[i].Arg))
	}
	return out
}

func execRunStatusError(taskID string, st protocol.ExecRunStatus) error {
	switch st {
	case protocol.ExecRunStatus_Ok:
		return nil
	case protocol.ExecRunStatus_NotFound:
		return fmt.Errorf("exec: task %q not found (pruned, wrong id, or outside your scope)", taskID)
	case protocol.ExecRunStatus_NoWorktree:
		return fmt.Errorf("exec: task %q has no worktree to run in — it ended clean and its tree was removed (a task that ended with uncommitted work keeps one)", taskID)
	case protocol.ExecRunStatus_RunnerUnreachable:
		return fmt.Errorf("exec: the runner hosting task %q is not connected", taskID)
	case protocol.ExecRunStatus_EmptyArgv:
		return errors.New("exec: empty command")
	case protocol.ExecRunStatus_Denied:
		return errors.New("exec: denied (needs the exec_run capability)")
	default:
		return fmt.Errorf("exec: %s", st.String())
	}
}

// ExecArgvString renders a command line for display. Arguments containing
// spaces are quoted so a two-word argument cannot be read as two arguments —
// a listing is what an operator reads to decide what to kill.
//
// The one implementation of that rule. Every surface that shows a command runs
// through here, so the CLI listing, the TUI result line and the WebUI row
// cannot disagree about what an argv looks like.
func ExecArgvString(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, s := range argv {
		if strings.ContainsAny(s, " \t\"") {
			s = fmt.Sprintf("%q", s)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// ExecRunArgvString is the wire-form adapter for ExecArgvString.
func ExecRunArgvString(a protocol.ExecArgv) string {
	return ExecArgvString(ExecArgvStrings(a))
}

// ExecRunInfoLines renders the listing as a human-readable table.
func ExecRunInfoLines(es []protocol.ExecRunInfo) []string {
	if len(es) == 0 {
		return []string{"no running execs"}
	}
	out := make([]string, 0, len(es)+1)
	out = append(out, fmt.Sprintf("%-6s %-12s %-8s %-24s %s", "id", "task", "age", "origin", "command"))
	for i := range es {
		e := &es[i]
		age := "-"
		if e.StartedUnixMs > 0 {
			age = time.Since(time.UnixMilli(int64(e.StartedUnixMs))).Truncate(time.Second).String()
		}
		out = append(out, fmt.Sprintf("%-6d %-12s %-8s %-24s %s",
			e.ExecId,
			hex.EncodeToString(e.TaskId.Id[:])[:12],
			age,
			e.OriginKind.String()+" "+string(e.OriginCid),
			ExecRunArgvString(e.Argv)))
	}
	return out
}

// ExecRunInfoJSONLine renders one row as JSON, carrying everything the text
// form abbreviates — the full task id and the argv as a list.
func ExecRunInfoJSONLine(e *protocol.ExecRunInfo) string {
	row := struct {
		ExecID        uint64   `json:"exec_id"`
		TaskID        string   `json:"task_id"`
		StartedUnixMs uint64   `json:"started_unix_ms"`
		Argv          []string `json:"argv"`
		OriginKind    string   `json:"origin_kind"`
		OriginCID     string   `json:"origin_cid"`
	}{
		ExecID:        e.ExecId,
		TaskID:        hex.EncodeToString(e.TaskId.Id[:]),
		StartedUnixMs: e.StartedUnixMs,
		Argv:          ExecArgvStrings(e.Argv),
		OriginKind:    e.OriginKind.String(),
		OriginCID:     string(e.OriginCid),
	}
	b, err := json.Marshal(row)
	if err != nil {
		return "{}"
	}
	return string(b)
}
