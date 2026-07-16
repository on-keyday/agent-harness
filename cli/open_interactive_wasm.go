//go:build js

package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// ErrPinnedNotFound is wrapped into the error returned for
// OpenInteractiveStatus_PinnedNotFound so callers can retry with a broader
// selector (e.g. Any) via errors.Is, instead of string-matching the message.
var ErrPinnedNotFound = errors.New("pinned runner not found")

// InteractiveSession holds the state of an active wasm-side interactive PTY
// session: the bidirectional stream with the runner, the recv goroutine
// cancel hook, and a closed flag guarded by mu.
//
// Browser UX is intentionally singleton — only one xterm is mounted, so we
// keep at most one InteractiveSession at a time (see activeInteractiveSession
// and the detach-old-on-new pattern in Interactive).
type InteractiveSession struct {
	stream    trsf.BidirectionalStream
	taskIDHex string
	ctx       context.Context
	cancel    context.CancelFunc
	// gen stamps which generation of the active session this is. recvPump
	// writes to the shared xterm only while gen == interactiveGen — see the
	// single-writer guard in recvPump.
	gen uint64
	// done is closed when recvPump exits. installAndPumpSession waits on it
	// so a superseded session's goroutine has stopped before the next one
	// starts painting the terminal.
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

// activeInteractiveSession is the singleton current session. Browser UX only
// allows one interactive task at a time; if a second Interactive call lands
// while a session exists, the old one is detached first.
//
// interactiveGen is bumped on every change of the active session (install of a
// new one, or detach to none). A recv goroutine paints the shared browser
// xterm only while its session.gen still equals interactiveGen. This is the
// single-writer invariant that the native TUI/CLI get for free — each runs one
// RemoteShell at a time against the real terminal (tui/interactive.go uses
// tea.Exec → RemoteShell, suspending the rest of the UI) — but the browser,
// with one long-lived xterm fed by per-session goroutines, must enforce
// explicitly. Without it, a superseded session's residual frames interleave
// with the new session's replay and desync the xterm parser, which is the
// browser-only reattach corruption that forced a page reload.
var (
	activeInteractiveSession *InteractiveSession
	activeInteractiveMu      sync.Mutex
	interactiveGen           atomic.Uint64
)

// detachDrainTimeout bounds how long installAndPumpSession waits for a
// superseded session's recv goroutine to stop before installing the new one.
// On a healthy transport CloseBoth unblocks the read well under this; the
// timeout only guards a wedged/dead transport (e.g. after a WS reconnect),
// where the old session has no inbound bytes to leak anyway.
const detachDrainTimeout = time.Second

// Interactive (wasm) opens an interactive PTY session against an idle runner
// for repo and wires the bidirectional stream's bytes to the browser xterm.
// Unlike the native variant it does not exec a local PTY; it just pumps
// frame-encapsulated bytes between the runner and the JS-side xterm.
//
// Method form only: the wasm caller (cmd/harness-webui-wasm) holds a single
// *Client for the lifetime of the page and reuses it across submit/list/
// cancel/watch/interactive. The session's bidirectional stream lives on top
// of that connection until DetachInteractive (or page unload) tears it down.
//
// The signature mirrors native (`(ctx, repo) (taskIDHex, error)`); the
// server allocates the TaskID from OpenInteractiveRequest{RepoPath}, so
// JS supplies the repo and gets the taskID back, not the other way around.
//
// On success this returns immediately after the recv goroutine is started.
// The active session is stored in activeInteractiveSession; subsequent calls
// to SendInteractive / ResizeInteractive / DetachInteractive operate on it.
func (c *Client) Interactive(ctx context.Context, repo string) (string, error) {
	return c.InteractiveWithSelectorAndArgs(ctx, repo, protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, "")
}

// InteractiveWithSelector is the same as Interactive but accepts an explicit
// runner selector. extraArgs default to none.
func (c *Client) InteractiveWithSelector(ctx context.Context, repo string, sel protocol.RunnerSelector) (string, error) {
	return c.InteractiveWithSelectorAndArgs(ctx, repo, sel, nil, "")
}

// InteractiveWithSelectorAndArgs is the full-featured form: selector pinning,
// per-task extraArgs, and an optional resumeTaskID (hex; "" = new task).
// Every session is detachable.
//
// RequestedCaps defaults to Capability_All (inherit everything the spawner
// holds). Callers that need a narrower grant should use
// InteractiveWithSelectorArgsAndCaps instead.
func (c *Client) InteractiveWithSelectorAndArgs(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string) (string, error) {
	return c.InteractiveWithSelectorArgsAndCaps(ctx, repo, sel, extraArgs, resumeTaskID, protocol.Capability_All, false, false, "")
}

// InteractiveWithSelectorArgsAndCaps is identical to
// InteractiveWithSelectorAndArgs but lets the caller specify an explicit
// capability mask for the spawned task. Pass protocol.Capability_All for the
// inherit-all behaviour.
// resumeCapsOverride, when true, instructs the server to apply caps as an
// override on resume (re-grant) rather than inheriting the original task's
// capability mask. Has no effect on new tasks (non-resume).
// resumeConversation, when true, asks the runner to resume the agent's own
// conversation state in addition to the harness task/worktree.
// agentProfile, when non-empty, selects a named agent profile (e.g. "codex")
// for the spawned task instead of the runner's default. "" means default.
func (c *Client) InteractiveWithSelectorArgsAndCaps(ctx context.Context, repo string, sel protocol.RunnerSelector, extraArgs []string, resumeTaskID string, caps protocol.Capability, resumeCapsOverride bool, resumeConversation bool, agentProfile string) (string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repo))
	oi.Selector = sel
	oi.ExtraArgs = protocol.ClaudeArgsFromStrings(extraArgs)
	oi.RequestedCaps = caps
	oi.SetResumeCapsOverride(resumeCapsOverride)
	oi.SetResumeConversation(resumeConversation)
	oi.SetAgentProfile([]byte(agentProfile))
	if resumeTaskID != "" {
		tid, err := parseTaskIDHex(resumeTaskID)
		if err != nil {
			return "", fmt.Errorf("Interactive: parse resume id: %w", err)
		}
		oi.ResumeTaskId = tid
	}
	req.SetOpenInteractive(oi)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return "", fmt.Errorf("OpenInteractive RPC: %w", err)
	}
	if resp.Kind != protocol.TaskControlKind_OpenInteractive {
		return "", fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	oiResp := resp.OpenInteractive()
	if oiResp == nil {
		return "", errors.New("empty OpenInteractive response")
	}
	switch oiResp.Status {
	case protocol.OpenInteractiveStatus_Ok:
	case protocol.OpenInteractiveStatus_NoRunnerForRepo:
		return "", fmt.Errorf("no idle runner for repo %q", repo)
	case protocol.OpenInteractiveStatus_ProfileUnavailable:
		if agentProfile != "" {
			return "", fmt.Errorf("profile_unavailable: agent profile %q is advertised by no runner serving repo %q", agentProfile, repo)
		}
		return "", fmt.Errorf("profile_unavailable: the resumed task's agent profile is advertised by no runner serving repo %q", repo)
	case protocol.OpenInteractiveStatus_RunnerBusy:
		return "", fmt.Errorf("runner busy")
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return "", &AmbiguousRunnerError{Candidates: candidatesFromResponse(oiResp)}
	case protocol.OpenInteractiveStatus_PinnedNotFound:
		return "", fmt.Errorf("pinned_not_found: the specified runner was not found: %w", ErrPinnedNotFound)
	case protocol.OpenInteractiveStatus_ResumeNotFound:
		return "", fmt.Errorf("resume_not_found: the specified resume task id is unknown")
	case protocol.OpenInteractiveStatus_ResumeNotTerminal:
		return "", fmt.Errorf("resume_not_terminal: the resume target is still queued/running (or another resume is already in flight)")
	default:
		return "", fmt.Errorf("server-side error opening interactive (status=%d)", oiResp.Status)
	}

	taskIDHex := hex.EncodeToString(oiResp.TaskId.Id[:])
	streamID := trsf.StreamID(oiResp.StreamId)

	stream := peer.WaitForBidirectionalStream(ctx, c.Transport(), streamID)
	if stream == nil {
		return taskIDHex, fmt.Errorf("stream %d not visible after OpenInteractive", streamID)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	session := &InteractiveSession{
		stream:    stream,
		taskIDHex: taskIDHex,
		ctx:       sessCtx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	installAndPumpSession(session)
	return taskIDHex, nil
}

// installAndPumpSession makes session the active interactive session and starts
// its recv pump. It reproduces, for the browser's single shared xterm, the
// single-writer-at-a-time property the native TUI/CLI get from RemoteShell.
//
// Ordering is deliberate:
//  1. Bump the generation and install the new session FIRST, so the previous
//     session's recv pump immediately fails its write guard and drops any
//     residual frames. This is what prevents the takeover corruption: the
//     server's replay ring already contains every frame it forwarded to the
//     old tui (runnerPump appends to the ring before forwarding), so the old
//     stream's tail is a *duplicate* of the replay — dropping it is correct,
//     letting it paint would double-render and desync the parser.
//  2. Detach the previous session (close its stream) and drain its goroutine,
//     bounded by detachDrainTimeout, so goroutines don't pile up across rapid
//     reattaches. This runs OUTSIDE activeInteractiveMu — the goroutine's exit
//     path also takes that lock, so holding it across the drain would deadlock.
//  3. Start the new session's pump, which replays the ring and then live output.
func installAndPumpSession(session *InteractiveSession) {
	activeInteractiveMu.Lock()
	old := activeInteractiveSession
	session.gen = interactiveGen.Add(1)
	activeInteractiveSession = session
	activeInteractiveMu.Unlock()

	if old != nil {
		old.detach()
		old.waitDone(detachDrainTimeout)
	}

	go session.recvPump()
}

// recvPump reads frames from the session's stream and writes stdout/stderr
// payload bytes to the browser xterm via harness_xtermWrite. Control frames
// (signal echoes) are ignored — the browser does not need to surface them. It
// exits (closing session.done) on stream EOF/error or when the session is
// superseded. Writes are gated by the generation guard so a superseded session
// cannot interleave its output with the successor's replay.
func (s *InteractiveSession) recvPump() {
	defer close(s.done)
	staleLogged := false
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		f := &frame.Frame{}
		if err := f.Read(s.stream); err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Info("interactive recv ended", "err", err, "task", s.taskIDHex)
			}
			activeInteractiveMu.Lock()
			// wasActive distinguishes a far-side close of the *current* session
			// (another client took it over, or the session itself exited) from a
			// local supersede / explicit detach — those already cleared
			// activeInteractiveSession, and should not surface a takeover notice.
			wasActive := activeInteractiveSession == s
			if wasActive {
				activeInteractiveSession = nil
			}
			activeInteractiveMu.Unlock()
			s.markClosed()
			if wasActive {
				notifyInteractiveClosed(s.taskIDHex)
			}
			return
		}
		switch f.Header.Type {
		case frame.FrameType_Stdout, frame.FrameType_Stderr:
			if f.Header.Len == 0 {
				continue
			}
			data := *f.Data()
			if len(data) == 0 {
				continue
			}
			// Single-writer guard: only the current generation paints the
			// shared xterm. A superseded session (older gen) was already
			// detached before the bump, so its stream is closing; drop its
			// residual output rather than interleaving it with the new
			// session's replay and desyncing the parser.
			if interactiveGen.Load() != s.gen {
				if !staleLogged {
					slog.Info("interactive: dropping output from superseded session",
						"task", s.taskIDHex, "sessionGen", s.gen, "currentGen", interactiveGen.Load())
					staleLogged = true
				}
				return
			}
			arr := js.Global().Get("Uint8Array").New(len(data))
			js.CopyBytesToJS(arr, data)
			js.Global().Call("harness_xtermWrite", arr)
		default:
			// Stdin / Control frames going *back* to the client are not
			// part of the contract. Ignore.
		}
	}
}

// waitDone blocks until the session's recv pump has exited (done closed) or
// timeout elapses, whichever comes first.
func (s *InteractiveSession) waitDone(timeout time.Duration) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.done:
	case <-t.C:
	}
}

// SendInteractive writes user-typed bytes (from xterm.onData) to the active
// interactive stream, wrapping them in a Stdin frame so the runner-side
// exec.ExecuteCommand parser sees them. Called from JS via
// window.harness.sendInteractive.
func SendInteractive(data []byte) error {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveMu.Unlock()
	if session == nil {
		return errors.New("no active interactive session")
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed {
		return errors.New("interactive session is closed")
	}
	if len(data) == 0 {
		return nil
	}
	hdr := frame.FrameHeader{
		Type: frame.FrameType_Stdin,
		Len:  uint32(len(data)),
	}
	// AppendData accepts multiple byte slices — passing the header and
	// payload separately avoids an extra concat.
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	if err := session.stream.AppendData(false, hdr.MustAppend(nil), dataCopy); err != nil {
		return fmt.Errorf("stream write: %w", err)
	}
	return nil
}

// ResizeInteractive forwards a window-size change to the runner using the
// canonical exec/frame wire format: a Frame{Type=Control} whose payload is a
// Control{Type=TerminalWindowSize, TerminalWindowSize{Columns, Rows, Width,
// Height}}. Width/Height are 0 (browsers don't expose pixel size symmetrically
// to the cell grid).
//
// This matches what (CommandExecutionStream).SetTerminalWindowSize encodes on
// the native side — see exec/exec.go. Verified against exec/frame/frame.go:
// the spec's speculative `[byte kind, u16 cols, u16 rows]` 5-byte layout
// would NOT be accepted by the runner's frame.Frame.Read.
func ResizeInteractive(cols, rows uint16) error {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveMu.Unlock()
	if session == nil {
		return errors.New("no active interactive session")
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed {
		return errors.New("interactive session is closed")
	}

	ctrl := frame.Control{Type: frame.ControlType_TerminalWindowSize}
	ctrl.SetTerminalWindowSize(frame.TerminalWindowSize{
		Columns: cols,
		Rows:    rows,
		Width:   0,
		Height:  0,
	})
	enc, err := ctrl.Append(nil)
	if err != nil {
		return fmt.Errorf("encode control: %w", err)
	}
	hdr := frame.FrameHeader{
		Type: frame.FrameType_Control,
		Len:  uint32(len(enc)),
	}
	if err := session.stream.AppendData(false, hdr.MustAppend(nil), enc); err != nil {
		return fmt.Errorf("stream write resize: %w", err)
	}
	return nil
}

// DetachInteractive closes the active session, if any. Idempotent. Called
// from JS via window.harness.detachInteractive (e.g. on tab close, or when
// the user clicks a Detach button).
func DetachInteractive() {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveSession = nil
	// Supersede the generation so any in-flight recv pump for this session
	// stops painting the shared xterm immediately (no session is current
	// after an explicit detach).
	interactiveGen.Add(1)
	activeInteractiveMu.Unlock()
	if session != nil {
		session.detach()
	}
}

// ActiveInteractiveTaskID returns the hex task id of the currently-attached
// interactive session, or "" if none. Lets JS reflect the active task in
// the UI without re-querying the server.
func ActiveInteractiveTaskID() string {
	activeInteractiveMu.Lock()
	defer activeInteractiveMu.Unlock()
	if activeInteractiveSession == nil {
		return ""
	}
	return activeInteractiveSession.taskIDHex
}

func (s *InteractiveSession) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
}

// notifyInteractiveClosed tells the browser that the active interactive session
// ended from the far side — another client took it over, or the session itself
// exited — so the WebUI can replace the stale "attached" indicator instead of
// leaving it. The handler (window.harness_onInteractiveClosed) decides how to
// surface it and intentionally does NOT clear the terminal. The call is guarded
// so a missing handler (e.g. a non-WebUI wasm host) is a no-op.
func notifyInteractiveClosed(taskIDHex string) {
	fn := js.Global().Get("harness_onInteractiveClosed")
	if fn.Type() != js.TypeFunction {
		return
	}
	fn.Invoke(taskIDHex)
}

func (s *InteractiveSession) detach() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.stream.CloseBoth()
	s.cancel()
}
