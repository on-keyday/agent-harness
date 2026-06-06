package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/on-keyday/agent-harness/exec/frame"
	"github.com/on-keyday/agent-harness/trsf"
)

// frameHeaderSize is the wire size of exec/frame.FrameHeader: 1-byte Type
// followed by 4-byte big-endian Len. Hard-coded here rather than imported
// from exec/frame because SessionMux only needs the *boundary*, not the
// frame's semantic content. Keep in sync with exec/frame/frame.bgn.
const frameHeaderSize = 5

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

	mu        sync.Mutex
	tui       trsf.BidirectionalStream
	tuiCancel context.CancelFunc

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
		if len(frameBytes) >= frameHeaderSize {
			switch frame.FrameType(frameBytes[0]) {
			case frame.FrameType_Stdout, frame.FrameType_Stderr:
				m.modes.feed(frameBytes[frameHeaderSize:])
			}
		}
		m.ring.Append(frameBytes)
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
	replay = append(replay, m.ring.Snapshot()...)
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

// tuiPump forwards tui→runner bytes. It detaches (without closing the runner)
// when the tui signals EOF or an error.
func (m *SessionMux) tuiPump(ctx context.Context, tui trsf.BidirectionalStream) {
	const maxRead = 32 * 1024
	for {
		if ctx.Err() != nil {
			return
		}
		data, eof, err := tui.ReadDirect(maxRead)
		if len(data) > 0 {
			// tuiPump: client → runner forward.
			// On runner write failure, the entire session is unrecoverable (peer runner
			// gone or wire error), so we Stop the whole mux. We do NOT fire onDetach
			// here — onDetach is for "client left, runner alive" transitions only.
			// onStop will fire from Stop() and the controller's transition rule will
			// move the task to a terminal state (Failed via runner_unreachable etc).
			if werr := m.runner.AppendData(false, data); werr != nil {
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
		m.mu.Unlock()
		if tui != nil {
			_ = tui.CloseBoth()
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
