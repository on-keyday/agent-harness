//go:build !js

package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// ErrPinnedNotFound is wrapped into the error returned for
// OpenInteractiveStatus_PinnedNotFound so callers can retry with a broader
// selector (e.g. Any) via errors.Is, instead of string-matching the message.
var ErrPinnedNotFound = errors.New("pinned runner not found")

// OpenInteractive asks the server to allocate an interactive PTY claude
// session in repoPath on an idle runner, splices the per-side streams, and
// returns a CommandExecutionStream wired to the client end. taskIDHex is
// the new task's id (so the caller can show it in tasks list / cancel it).
//
// On success, the caller owns the returned stream and is responsible for
// calling RemoteShell (or Stdin/Stdout/Stderr individually) and Close.
// The runner's exec.ExecuteCommand drives PTY lifecycle on the other side.
func (c *Client) OpenInteractive(ctx context.Context, repoPath string, opts SessionOpts) (*agentexec.CommandExecutionStream, string, error) {
	return c.openInteractive(ctx, repoPath, opts, nil)
}

// openInteractive is the single OpenInteractive request builder. x11 is nil for
// non-X11 sessions; when set, x11_enabled + the X11Forward block (display +
// cookie) are sent.
func (c *Client) openInteractive(ctx context.Context, repoPath string, opts SessionOpts, x11 *X11Request) (*agentexec.CommandExecutionStream, string, error) {
	oir, err := c.roundTripOpen(ctx, repoPath, opts, x11)
	if err != nil {
		return nil, "", err
	}

	taskIDHex := hex.EncodeToString(oir.TaskId.Id[:])

	sid := oir.StreamId()
	if !oir.Attached() || sid == nil {
		return nil, taskIDHex, fmt.Errorf("OpenInteractive: server opened the session detached; no stream")
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(*sid))
	if st == nil {
		return nil, taskIDHex, fmt.Errorf("exec stream %d not visible after OpenInteractive", *sid)
	}
	stream := agentexec.NewCommandExecutionStream(st)
	// Size the PTY here, in the one place every native attached open funnels
	// through. A detached open (OpenDetached) has no stream and sends its size
	// in the request instead.
	if err := applyInitialWindowSize(stream, opts); err != nil {
		return nil, taskIDHex, fmt.Errorf("OpenInteractive: initial window size: %w", err)
	}
	return stream, taskIDHex, nil
}

// OpenDetached opens a session without attaching to it and returns the task
// id. The server creates no stream toward this client, so the caller never
// holds the session's control seat — which is what lets a caller without
// exec_control start one. opts.InitialRows / InitialCols travel in the request.
func (c *Client) OpenDetached(ctx context.Context, repoPath string, opts SessionOpts) (string, error) {
	opts.Detached = true
	oir, err := c.roundTripOpen(ctx, repoPath, opts, nil)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(oir.TaskId.Id[:]), nil
}

// roundTripOpen sends one OpenInteractive request and turns every non-ok answer
// into an error, an ambiguous one into an *AmbiguousRunnerError. Shared by the
// attached and the detached open so the two cannot map statuses differently.
func (c *Client) roundTripOpen(ctx context.Context, repoPath string, opts SessionOpts, x11 *X11Request) (*protocol.OpenInteractiveResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := buildOpenInteractiveRequest(repoPath, opts)
	resumeTaskID := opts.ResumeTaskID
	if resumeTaskID != "" {
		tid, err := parseTaskIDHex(resumeTaskID)
		if err != nil {
			return nil, fmt.Errorf("OpenInteractive: parse resume id: %w", err)
		}
		oi.ResumeTaskId = tid
	}
	if x11 != nil {
		oi.SetX11Enabled(true) // set the discriminator BEFORE the embedded block
		f := protocol.X11Forward{Display: uint16(x11.Display)}
		f.SetCookie(x11.Cookie)
		oi.SetX11(f)
	}
	req.SetOpenInteractive(oi)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenInteractive {
		return nil, fmt.Errorf("expected OpenInteractive response, got kind=%v", resp.Kind)
	}
	oir := resp.OpenInteractive()
	if oir == nil {
		return nil, fmt.Errorf("OpenInteractive response variant missing")
	}
	if oir.Status == protocol.OpenInteractiveStatus_AmbiguousRunner {
		cands, err := readCandidates(ctx, c.Transport(), oir.CandidatesStreamId())
		if err != nil {
			return nil, err
		}
		return nil, &AmbiguousRunnerError{Candidates: cands}
	}
	if err := openInteractiveStatusError(repoPath, opts.AgentProfile, oir.Status); err != nil {
		return nil, err
	}
	return oir, nil
}

// openInteractiveStatusError converts a non-Ok OpenInteractiveStatus into a
// Go error. Returns nil for OpenInteractiveStatus_Ok.
func openInteractiveStatusError(repo, agentProfile string, status protocol.OpenInteractiveStatus) error {
	switch status {
	case protocol.OpenInteractiveStatus_Ok:
		return nil
	case protocol.OpenInteractiveStatus_NoRunnerForRepo:
		return fmt.Errorf("interactive no_runner_for_repo: no idle runner for repo %q", repo)
	case protocol.OpenInteractiveStatus_ProfileUnavailable:
		if agentProfile != "" {
			return fmt.Errorf("interactive profile_unavailable: agent profile %q is advertised by no runner serving repo %q", agentProfile, repo)
		}
		return fmt.Errorf("interactive profile_unavailable: the resumed task's agent profile is advertised by no runner serving repo %q (pick a different runner/agent)", repo)
	case protocol.OpenInteractiveStatus_RunnerBusy:
		return fmt.Errorf("interactive runner_busy: runner is at capacity")
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return fmt.Errorf("interactive ambiguous_runner: multiple runners match; pin one with --runner/--host/--ip")
	case protocol.OpenInteractiveStatus_PinnedNotFound:
		return fmt.Errorf("interactive pinned_not_found: the specified runner was not found: %w", ErrPinnedNotFound)
	case protocol.OpenInteractiveStatus_ResumeNotFound:
		return fmt.Errorf("interactive resume_not_found: the specified resume task id is unknown (was it pruned, or is the kind a mismatch?)")
	case protocol.OpenInteractiveStatus_ResumeNotTerminal:
		return fmt.Errorf("interactive resume_not_terminal: the resume target is still queued/running (or another resume is already in flight)")
	case protocol.OpenInteractiveStatus_ResumeExactMismatch:
		return fmt.Errorf("interactive resume_exact_mismatch: an exact resume takes the task's recorded settings and kind; drop the setting flags (or --stream to match the task) or drop --exact")
	case protocol.OpenInteractiveStatus_ScopeNotPermitted:
		return fmt.Errorf("interactive scope_not_permitted: a scope id is outside your own scope, or the scope is inconsistent (overlapping or empty overrides, or one naming spawn — narrow resume / resume_exact instead)")
	case protocol.OpenInteractiveStatus_DetachRequired:
		return fmt.Errorf("interactive detach_required: you may open this task but not hold its seat (it is outside your exec_control scope); add -d")
	case protocol.OpenInteractiveStatus_InternalError:
		return fmt.Errorf("interactive internal_error")
	default:
		return fmt.Errorf("interactive error (status=%d)", status)
	}
}

// Interactive splices stdin/stdout/SIGWINCH between the local terminal and
// the remote PTY for an interactive claude session in repo. Method form:
// callable on an existing *Client without re-dialing. The caller's terminal
// must be a real tty (RemoteShell flips it into raw mode). Returns the new
// task's hex id even on error so the caller can surface it for cleanup.
func (c *Client) Interactive(ctx context.Context, repo string, opts SessionOpts) (string, error) {
	stream, taskIDHex, err := c.OpenInteractive(ctx, repo, opts)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()

	// stderr because stdout is owned by the remote PTY's output once
	// RemoteShell starts. Printing before MakeRaw keeps the message in
	// cooked mode so the trailing newline behaves.
	fmt.Fprintf(os.Stderr, "harness-cli: attached to task %s (Ctrl+] to detach client; Ctrl+D / `exit` ends the session)\n", taskIDHex)

	// RemoteShell returns with the local terminal already back in cooked mode
	// and its screen and input modes reset — it does that on the way out
	// whether it is returning cleanly or not (objtrsf exec.WriteTerminalReset).
	err = stream.RemoteShell()
	if err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}

// Interactive (package-level) is a thin wrapper that opens a fresh Client per
// call: dial, open the interactive session, splice, then close. Suitable for
// the harness-cli `interactive` subcommand. Long-lived consumers should hold
// a *Client and call (*Client).Interactive instead.
func Interactive(ctx context.Context, peerCID objproto.ConnectionID, repo string) (string, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Interactive(ctx, repo, SessionOpts{})
}
