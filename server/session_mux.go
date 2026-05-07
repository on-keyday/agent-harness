package server

import (
	"context"
	"errors"
	"sync"

	"github.com/on-keyday/agent-harness/trsf"
)

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
		stopped:  make(chan struct{}),
		onAttach: hooks.OnAttach,
		onDetach: hooks.OnDetach,
		onStop:   hooks.OnStop,
	}
	go m.runnerPump()
	return m
}

// runnerPump reads from the runner stream, appends to the ring, and
// forwards live data to the attached tui (if any). It calls Stop on exit
// so that a runner-side EOF/error tears everything down.
func (m *SessionMux) runnerPump() {
	defer m.Stop()
	const maxRead = 32 * 1024
	for {
		if m.ctx.Err() != nil {
			return
		}
		data, eof, err := m.runner.ReadDirect(maxRead)
		if len(data) > 0 {
			m.ring.Append(data)
			m.mu.Lock()
			tui := m.tui
			m.mu.Unlock()
			if tui != nil {
				if werr := tui.AppendData(false, data); werr != nil {
					m.mu.Lock()
					m.detachLocked(tui)
					m.mu.Unlock()
				}
			}
		}
		if eof || err != nil {
			return
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

	// Replay ring buffer contents.
	snap := m.ring.Snapshot()
	if len(snap) > 0 {
		if err := tui.AppendData(false, snap); err != nil {
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
