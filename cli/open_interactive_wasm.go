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
	"syscall/js"

	"github.com/on-keyday/agent-harness/exec/frame"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

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
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
}

// activeInteractiveSession is the singleton current session. Browser UX only
// allows one interactive task at a time; if a second Interactive call lands
// while a session exists, the old one is detached first.
var (
	activeInteractiveSession *InteractiveSession
	activeInteractiveMu      sync.Mutex
)

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
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repo))
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
	case protocol.OpenInteractiveStatus_RunnerBusy:
		return "", fmt.Errorf("runner busy")
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
		cancel:    cancel,
	}

	// Detach any previous session before installing the new one. The
	// browser only ever shows one xterm at a time; if JS forgot to call
	// DetachInteractive before reopening, do it implicitly so the old
	// recv goroutine doesn't keep writing into the (about-to-be-replaced)
	// xterm.
	activeInteractiveMu.Lock()
	if old := activeInteractiveSession; old != nil {
		old.detach()
	}
	activeInteractiveSession = session
	activeInteractiveMu.Unlock()

	// recv goroutine: stream → frame parser → harness_xtermWrite for
	// stdout/stderr payload bytes. Control frames (signal echoes) are
	// currently ignored — the browser does not need to surface them.
	go func() {
		for {
			select {
			case <-sessCtx.Done():
				return
			default:
			}
			f := &frame.Frame{}
			if err := f.Read(stream); err != nil {
				if !errors.Is(err, io.EOF) {
					slog.Info("interactive recv ended", "err", err)
				}
				// recv loop exit ⇒ implicitly detach so JS sees a
				// clean state on next call.
				activeInteractiveMu.Lock()
				if activeInteractiveSession == session {
					activeInteractiveSession = nil
				}
				activeInteractiveMu.Unlock()
				session.markClosed()
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
				arr := js.Global().Get("Uint8Array").New(len(data))
				js.CopyBytesToJS(arr, data)
				js.Global().Call("harness_xtermWrite", arr)
			default:
				// Stdin / Control frames going *back* to the client
				// are not part of the contract. Ignore.
			}
		}
	}()

	return taskIDHex, nil
}

// InteractiveWithSelector is the same as Interactive but accepts an explicit
// runner selector. Callers that want the Any-runner behaviour can use
// Interactive directly.
func (c *Client) InteractiveWithSelector(ctx context.Context, repo string, sel protocol.RunnerSelector) (string, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	oi.SetRepoPath([]byte(repo))
	oi.Selector = sel
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
	case protocol.OpenInteractiveStatus_RunnerBusy:
		return "", fmt.Errorf("runner busy")
	case protocol.OpenInteractiveStatus_AmbiguousRunner:
		return "", fmt.Errorf("ambiguous_runner: multiple runners match; pin one with host")
	case protocol.OpenInteractiveStatus_PinnedNotFound:
		return "", fmt.Errorf("pinned_not_found: the specified runner was not found")
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
		cancel:    cancel,
	}

	activeInteractiveMu.Lock()
	if old := activeInteractiveSession; old != nil {
		old.detach()
	}
	activeInteractiveSession = session
	activeInteractiveMu.Unlock()

	go func() {
		for {
			select {
			case <-sessCtx.Done():
				return
			default:
			}
			f := &frame.Frame{}
			if err := f.Read(stream); err != nil {
				if !errors.Is(err, io.EOF) {
					slog.Info("interactive recv ended", "err", err)
				}
				activeInteractiveMu.Lock()
				if activeInteractiveSession == session {
					activeInteractiveSession = nil
				}
				activeInteractiveMu.Unlock()
				session.markClosed()
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
				arr := js.Global().Get("Uint8Array").New(len(data))
				js.CopyBytesToJS(arr, data)
				js.Global().Call("harness_xtermWrite", arr)
			default:
			}
		}
	}()

	return taskIDHex, nil
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
