package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/on-keyday/agent-harness/exec/frame"
	"github.com/on-keyday/objtrsf/trsf"
)

// frameHeaderSize is the wire size of exec/frame.FrameHeader: 1-byte Type
// followed by 4-byte big-endian Len. Hard-coded here rather than imported
// from exec/frame because SessionMux only needs the *boundary*, not the
// frame's semantic content. Keep in sync with exec/frame/frame.bgn.
const frameHeaderSize = 5

// viewerQueueDepth bounds per-viewer buffering. A viewer that cannot drain its
// queue this fast is dropped (it can never block the runner pump or the writer).
const viewerQueueDepth = 256

// viewerConn is one read-only observer of the session. Its output is delivered
// through a bounded channel by a dedicated pump; its input is read-and-discarded.
type viewerConn struct {
	stream trsf.BidirectionalStream
	ch     chan []byte
	cancel context.CancelFunc
}

// readOneFrame reads exactly one wire-encoded frame (header + payload)
// from r and returns the concatenated bytes. Used by runnerPump to keep
// ring-buffer entries aligned to frame boundaries: a byte-level ring that
// wraps mid-frame would feed the client's parser a bogus header and
// deadlock it on a fake Len. Returns the read error verbatim — callers
// should stop the session on any non-nil error.
func readOneFrame(r io.Reader) ([]byte, error) {
	hdr := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	payloadLen := binary.BigEndian.Uint32(hdr[1:5])
	out := make([]byte, frameHeaderSize+int(payloadLen))
	copy(out, hdr)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, out[frameHeaderSize:]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// encodeStdoutFrame wraps payload in one exec/frame Stdout frame (1-byte type +
// 4-byte big-endian length + payload), matching the wire format runnerPump
// forwards and the ring stores, so a synthesised frame is indistinguishable
// from a live one to the client's parser.
func encodeStdoutFrame(payload []byte) []byte {
	out := make([]byte, frameHeaderSize+len(payload))
	out[0] = byte(frame.FrameType_Stdout)
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[frameHeaderSize:], payload)
	return out
}

// SessionHooks lets the controller observe SessionMux state transitions.
// Any field may be nil. Hooks fire from goroutines other than the caller's,
// so callbacks must be safe to call concurrently with other SessionMux
// methods (do not call back into the same SessionMux's Stop()/Attach()
// synchronously without expecting reentrancy).
type SessionHooks struct {
	OnAttach func(taskID string)
	OnDetach func(taskID string)
	OnStop   func(taskID string)
}

// SessionMux owns the runner-side bidi stream for a detachable interactive
// session. It pumps runner output into a RingBuffer, forwards to whatever
// tuiStream is currently attached, and accepts new client tuiStreams that
// take over from any existing attach.
type SessionMux struct {
	ctx    context.Context
	cancel context.CancelFunc

	taskID string
	runner trsf.BidirectionalStream
	ring   *RingBuffer
	modes  *modeTracker

	// runnerWriteMu serializes writes to the runner stream and keeps them
	// frame-atomic. With multi-writer (one control tui + N cowriters all
	// forwarding input), an unsynchronised write could interleave a cowriter
	// frame into the middle of a control frame and desync the runner's
	// frame-aligned reader. Every forwarder writes ONE complete frame under
	// this lock. Distinct from mu (which guards attach/viewer state).
	runnerWriteMu sync.Mutex

	// mainMark is the ring append-index of the frame at which the session last
	// returned to the primary screen (a full-screen app's alt-screen exit).
	// On reattach, when the session is currently on the primary screen, replay
	// starts here instead of at the ring head, skipping the dead alt-screen
	// episode whose verbatim replay would corrupt the display. Zero (the
	// default) means "no alt-screen exit recorded" → full replay. Atomic so the
	// runner pump can publish it without coordinating with the attach path.
	mainMark atomic.Int64

	mu        sync.Mutex
	tui       trsf.BidirectionalStream
	tuiCancel context.CancelFunc

	viewers map[*viewerConn]struct{}

	// lastWinSize is the raw wire bytes of the most recent TerminalWindowSize
	// control frame seen on the tui→runner direction (the controlling client's
	// PTY size). Replayed verbatim to a new viewer ahead of the ring so a
	// read-only snapshot can size its terminal grid to match the size the
	// absolute-positioned output was painted at. A viewer never sends its own
	// size (viewerInputDrain discards its input), so without this it could not
	// learn the size and would mis-render full-screen TUIs. Guarded by mu.
	lastWinSize []byte

	onDetach func(taskID string)
	onAttach func(taskID string)
	onStop   func(taskID string)

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewSessionMux creates a SessionMux and starts the runner pump goroutine.
// parentCtx cancellation propagates to Stop. Hooks are installed before
// runnerPump starts, eliminating any race window.
func NewSessionMux(parentCtx context.Context, taskID string, runner trsf.BidirectionalStream, ring *RingBuffer, hooks SessionHooks) *SessionMux {
	ctx, cancel := context.WithCancel(parentCtx)
	m := &SessionMux{
		ctx:      ctx,
		cancel:   cancel,
		taskID:   taskID,
		runner:   runner,
		ring:     ring,
		modes:    newModeTracker(),
		stopped:  make(chan struct{}),
		viewers:  make(map[*viewerConn]struct{}),
		onAttach: hooks.OnAttach,
		onDetach: hooks.OnDetach,
		onStop:   hooks.OnStop,
	}
	go m.runnerPump()
	return m
}

// runnerPump reads ONE frame at a time from the runner stream, appends the
// wire-encoded frame to the ring, and forwards it to the attached tui
// (if any). Reading per-frame (instead of per-arbitrary-byte-chunk) is
// what keeps the ring's drop policy aligned to frame boundaries: when a
// future Append wraps around, the dropped entry is one or more *whole*
// frames, never a partial header. It calls Stop on exit so that a
// runner-side EOF/error tears everything down.
func (m *SessionMux) runnerPump() {
	defer m.Stop()
	for {
		if m.ctx.Err() != nil {
			return
		}
		frameBytes, err := readOneFrame(m.runner)
		if err != nil {
			return
		}
		// Track DEC private-mode state from display output so a reattach can
		// re-establish modes (e.g. a hidden cursor) whose controlling sequence
		// has since been evicted from the ring. Only Stdout/Stderr carry it.
		wasAlt := m.modes.onAltScreen()
		if len(frameBytes) >= frameHeaderSize {
			switch frame.FrameType(frameBytes[0]) {
			case frame.FrameType_Stdout, frame.FrameType_Stderr:
				m.modes.feed(frameBytes[frameHeaderSize:])
			}
		}
		m.ring.Append(frameBytes)
		// If this frame carried the alt-screen exit (alt → primary), mark it as
		// the replay start point: everything before is a now-finished
		// full-screen episode that must not be replayed verbatim. The mark is
		// the just-appended frame's index, so replay includes the ESC[?1049l
		// itself (ensuring a reattaching client also leaves the alt buffer).
		if wasAlt && !m.modes.onAltScreen() {
			m.mainMark.Store(int64(m.ring.AppendCount() - 1))
		}
		m.mu.Lock()
		tui := m.tui
		m.mu.Unlock()
		if tui != nil {
			if werr := tui.AppendData(false, frameBytes); werr != nil {
				m.mu.Lock()
				m.detachLocked(tui)
				m.mu.Unlock()
			}
		}
		// Fan out to viewers (non-blocking). A viewer whose queue is full
		// cannot keep up and is dropped here — never blocking this pump.
		m.mu.Lock()
		for v := range m.viewers {
			select {
			case v.ch <- frameBytes:
			default:
				m.dropViewerLocked(v)
			}
		}
		m.mu.Unlock()
	}
}

// Attach installs a new tui stream. If one is already attached it is
// force-closed (takeover semantics). The ring buffer contents are replayed
// to the new tui before live forwarding resumes.
func (m *SessionMux) Attach(ctx context.Context, tui trsf.BidirectionalStream) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("session_mux: stopped")
	}
	old := m.tui
	if m.tuiCancel != nil {
		m.tuiCancel()
	}
	m.tui = tui
	tuiCtx, tuiCancel := context.WithCancel(m.ctx)
	m.tuiCancel = tuiCancel
	m.mu.Unlock()

	// Force-close the previous tui (takeover).
	if old != nil {
		_ = old.CloseBoth()
	}

	// Replay: first re-establish terminal modes whose controlling sequence may
	// have scrolled out of the ring window (e.g. a hidden cursor), then the
	// buffered output. Both go out as ordinary Stdout frames the client parses
	// exactly like live ones, so the new emulator starts from the right state.
	var replay []byte
	if pre := m.modes.preamble(); len(pre) > 0 {
		replay = append(replay, encodeStdoutFrame(pre)...)
	}
	replay = append(replay, m.replaySnapshot()...)
	if len(replay) > 0 {
		if err := tui.AppendData(false, replay); err != nil {
			m.mu.Lock()
			if m.tui == tui {
				m.tui = nil
				m.tuiCancel = nil
			}
			m.mu.Unlock()
			tuiCancel()
			return err
		}
	}

	if m.onAttach != nil {
		m.onAttach(m.taskID)
	}

	go m.tuiPump(tuiCtx, tui)
	return nil
}

// AttachViewer adds a read-only observer (its input is discarded). Unlike
// Attach it does NOT take the writer slot or fire onAttach.
func (m *SessionMux) AttachViewer(ctx context.Context, stream trsf.BidirectionalStream) error {
	return m.attachObserver(stream, false)
}

// AttachCoWriter adds a non-takeover writer: it observes output like a viewer
// AND forwards its input to the runner, EXCEPT TerminalWindowSize frames, which
// are dropped — a cowriter has no size authority (only the control client owns
// the PTY size). Lets an agent co-drive a session alongside a human controller
// without kicking them; the human keeps size ownership so the PTY isn't resized
// out from under them.
func (m *SessionMux) AttachCoWriter(ctx context.Context, stream trsf.BidirectionalStream) error {
	return m.attachObserver(stream, true)
}

// attachObserver registers a viewer (forwardInput=false) or cowriter
// (forwardInput=true): adds it to the output fan-out, replays size+modes+ring,
// and starts its output pump plus either an input drain or an input forwarder.
func (m *SessionMux) attachObserver(stream trsf.BidirectionalStream, forwardInput bool) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("session_mux: stopped")
	}
	vctx, vcancel := context.WithCancel(m.ctx)
	v := &viewerConn{stream: stream, ch: make(chan []byte, viewerQueueDepth), cancel: vcancel}
	m.viewers[v] = struct{}{}
	// Snapshot replay state under the SAME lock as the insert so runnerPump's
	// fan-out cannot interleave between "added" and "snapshotted".
	var replay []byte
	// PTY size first, so the observer's emulator resizes before consuming the
	// absolute-positioned ring content. Verbatim wire frame (already a complete
	// TerminalWindowSize control frame).
	if len(m.lastWinSize) > 0 {
		replay = append(replay, m.lastWinSize...)
	}
	if pre := m.modes.preamble(); len(pre) > 0 {
		replay = append(replay, encodeStdoutFrame(pre)...)
	}
	replay = append(replay, m.replaySnapshot()...)
	m.mu.Unlock()

	// Replay BEFORE starting the output pump, so replayed bytes always precede
	// live frames (live frames buffer in v.ch meanwhile).
	if len(replay) > 0 {
		if err := stream.AppendData(false, replay); err != nil {
			m.dropViewer(v)
			return err
		}
	}
	go m.viewerOutputPump(vctx, v)
	if forwardInput {
		go m.cowriterInputForward(vctx, v)
	} else {
		go m.viewerInputDrain(vctx, v)
	}
	return nil
}

// viewerOutputPump drains v.ch to the viewer stream. Drops the viewer on write error.
func (m *SessionMux) viewerOutputPump(ctx context.Context, v *viewerConn) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-v.ch:
			if err := v.stream.AppendData(false, b); err != nil {
				m.dropViewer(v)
				return
			}
		}
	}
}

// viewerInputDrain reads and DISCARDS the viewer's incoming direction. This is
// the read-only enforcement point: unlike tuiPump it never forwards to the
// runner. Draining prevents the bidi recv side from backpressuring/wedging and
// gives prompt EOF when the client closes. ReadDirectContext (not ReadDirect)
// so cancel()/Stop() unblock the read immediately.
func (m *SessionMux) viewerInputDrain(ctx context.Context, v *viewerConn) {
	const maxRead = 32 * 1024
	for {
		_, eof, err := v.stream.ReadDirectContext(ctx, maxRead)
		if eof || err != nil {
			m.dropViewer(v)
			return
		}
	}
}

func (m *SessionMux) dropViewer(v *viewerConn) {
	m.mu.Lock()
	m.dropViewerLocked(v)
	m.mu.Unlock()
}

// dropViewerLocked removes and tears down a viewer. Idempotent: if v is no
// longer in the set, it is a no-op (both viewer goroutines may call it).
// Must be called with m.mu held.
func (m *SessionMux) dropViewerLocked(v *viewerConn) {
	if _, ok := m.viewers[v]; !ok {
		return
	}
	delete(m.viewers, v)
	v.cancel()
	_ = v.stream.CloseBoth()
}

// ViewerCount reports the number of attached viewers (test/inspection helper).
func (m *SessionMux) ViewerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.viewers)
}

// tuiPump forwards control-client input frames to the runner, frame-atomically
// (under runnerWriteMu, so they never interleave with a cowriter's frames). The
// control client is the session's sole size authority: its TerminalWindowSize
// frames are forwarded AND recorded as m.lastWinSize for replay to
// viewers/cowriters. Detaches (without closing the runner) on tui EOF/error.
func (m *SessionMux) tuiPump(ctx context.Context, tui trsf.BidirectionalStream) {
	const maxRead = 32 * 1024
	var acc []byte
	for {
		if ctx.Err() != nil {
			return
		}
		data, eof, err := tui.ReadDirect(maxRead)
		if len(data) > 0 {
			var ok bool
			acc, ok = m.forwardControlFrames(append(acc, data...))
			if !ok {
				// Runner write failed: session unrecoverable (peer runner gone
				// or wire error). Stop the whole mux; onStop fires and the
				// controller moves the task to a terminal state. We do NOT fire
				// onDetach (that's for "client left, runner alive").
				m.Stop()
				return
			}
		}
		if eof || err != nil {
			m.detachOnly(tui)
			return
		}
	}
}

// forwardControlFrames forwards each COMPLETE frame from acc to the runner under
// runnerWriteMu, recording any TerminalWindowSize frame in m.lastWinSize (the
// control client owns the size). Returns the unconsumed tail and false on runner
// write failure.
func (m *SessionMux) forwardControlFrames(acc []byte) ([]byte, bool) {
	for len(acc) >= frameHeaderSize {
		total := frameHeaderSize + int(binary.BigEndian.Uint32(acc[1:5]))
		if len(acc) < total {
			break // incomplete frame; carry to next read
		}
		fb := acc[:total]
		if frameIsWinSize(fb) {
			cp := append([]byte(nil), fb...)
			m.mu.Lock()
			m.lastWinSize = cp
			m.mu.Unlock()
		}
		if err := m.writeFrameToRunner(fb); err != nil {
			return nil, false
		}
		acc = acc[total:]
	}
	return carryTail(acc), true
}

// cowriterInputForward forwards a cowriter's input frames to the runner,
// frame-atomically, but DROPS TerminalWindowSize frames — a cowriter has no size
// authority (only the control client owns the PTY size). Drops the cowriter on
// EOF/error; Stops the mux on runner write failure.
func (m *SessionMux) cowriterInputForward(ctx context.Context, v *viewerConn) {
	const maxRead = 32 * 1024
	var acc []byte
	for {
		data, eof, err := v.stream.ReadDirectContext(ctx, maxRead)
		if len(data) > 0 {
			var ok bool
			acc, ok = m.forwardCowriterFrames(append(acc, data...))
			if !ok {
				m.Stop()
				return
			}
		}
		if eof || err != nil {
			m.dropViewer(v)
			return
		}
	}
}

// forwardCowriterFrames forwards each COMPLETE non-TerminalWindowSize frame to
// the runner under runnerWriteMu; resize frames are silently dropped. Returns
// the unconsumed tail and false on runner write failure.
func (m *SessionMux) forwardCowriterFrames(acc []byte) ([]byte, bool) {
	for len(acc) >= frameHeaderSize {
		total := frameHeaderSize + int(binary.BigEndian.Uint32(acc[1:5]))
		if len(acc) < total {
			break
		}
		fb := acc[:total]
		if !frameIsWinSize(fb) {
			if err := m.writeFrameToRunner(fb); err != nil {
				return nil, false
			}
		}
		acc = acc[total:]
	}
	return carryTail(acc), true
}

// writeFrameToRunner writes one complete frame to the runner under
// runnerWriteMu, keeping multi-writer forwards frame-atomic.
func (m *SessionMux) writeFrameToRunner(fb []byte) error {
	m.runnerWriteMu.Lock()
	defer m.runnerWriteMu.Unlock()
	return m.runner.AppendData(false, fb)
}

// frameIsWinSize reports whether fb is a complete TerminalWindowSize control frame.
func frameIsWinSize(fb []byte) bool {
	if len(fb) < frameHeaderSize || frame.FrameType(fb[0]) != frame.FrameType_Control {
		return false
	}
	f := &frame.Frame{}
	if err := f.Read(bytes.NewReader(fb)); err != nil {
		return false
	}
	ctrl := f.Control()
	return ctrl != nil && ctrl.Type == frame.ControlType_TerminalWindowSize
}

// carryTail copies a partial-frame remainder off the read buffer so a later
// append cannot alias it.
func carryTail(acc []byte) []byte {
	if len(acc) == 0 {
		return nil
	}
	return append([]byte(nil), acc...)
}

func (m *SessionMux) detachOnly(tui trsf.BidirectionalStream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tui != tui {
		return
	}
	m.detachLocked(tui)
}

// detachLocked must be called with m.mu held.
func (m *SessionMux) detachLocked(tui trsf.BidirectionalStream) {
	if m.tui != tui {
		return
	}
	m.tui = nil
	if m.tuiCancel != nil {
		m.tuiCancel()
		m.tuiCancel = nil
	}
	_ = tui.CloseBoth()
	if m.onDetach != nil {
		go m.onDetach(m.taskID)
	}
}

// IsAttached reports whether a tui stream is currently attached.
func (m *SessionMux) IsAttached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tui != nil
}

// replaySnapshot returns the ring bytes to replay to a (re)attaching client.
// On the primary screen it starts from the last alt-screen exit (mainMark),
// skipping a finished full-screen episode whose verbatim replay — absolute-
// cursor frame fragments with no enclosing alt-screen — corrupts the display.
// While a full-screen app is still live (in the alt screen) it replays the
// whole ring, since the app repaints over any partial frame on the next tick.
func (m *SessionMux) replaySnapshot() []byte {
	if m.modes.onAltScreen() {
		return m.ring.Snapshot()
	}
	return m.ring.SnapshotFrom(int(m.mainMark.Load()))
}

// RingBufferLen returns the number of bytes currently stored in the ring buffer.
func (m *SessionMux) RingBufferLen() int { return m.ring.Len() }

// Stop shuts down the mux: cancels the context, closes both the tui (if any)
// and the runner stream, and fires onStop. Idempotent.
func (m *SessionMux) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.mu.Lock()
		tui := m.tui
		m.tui = nil
		if m.tuiCancel != nil {
			m.tuiCancel()
			m.tuiCancel = nil
		}
		vs := make([]*viewerConn, 0, len(m.viewers))
		for v := range m.viewers {
			vs = append(vs, v)
		}
		m.viewers = make(map[*viewerConn]struct{})
		m.mu.Unlock()
		if tui != nil {
			_ = tui.CloseBoth()
		}
		for _, v := range vs {
			v.cancel()
			_ = v.stream.CloseBoth()
		}
		_ = m.runner.CloseBoth()
		if m.onStop != nil {
			m.onStop(m.taskID)
		}
		close(m.stopped)
	})
}

// Wait returns a channel that is closed when Stop completes.
func (m *SessionMux) Wait() <-chan struct{} { return m.stopped }
