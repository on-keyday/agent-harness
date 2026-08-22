package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
	"github.com/on-keyday/objtrsf/exec/frame"
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
	// cowriter distinguishes the two observer kinds that share this struct and
	// this map: a viewer's input is discarded, a cowriter's is forwarded to the
	// runner. They differ in what the operator can DO through them, so the
	// counts are reported apart rather than as one "observers" total.
	cowriter bool
	// allowResize is ORTHOGONAL to cowriter: it says this observer's
	// TerminalWindowSize frames are honoured. Recorded per attach from the
	// caller's exec_resize capability, because that is the only moment the
	// principal is known — the frames themselves carry no identity.
	allowResize bool
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

// capReplayTail returns the suffix of frame-aligned replay data whose length is
// ≤ limit, starting at a frame boundary that is ALSO a VT ground-state boundary.
// limit==0 (or data already within limit) returns data unchanged.
//
// The ring stores complete frames, so dropping whole LEADING frames keeps the
// client's exec/frame parser aligned; the dropped frames are older scrollback an
// observer does not need to render the current screen. But a frame boundary is
// NOT a VT escape-sequence boundary: cropping in the middle of an OSC/DCS/CSI
// sequence delivers that sequence's TAIL to the client's emulator, which — fresh
// in ground state — renders it as stray printable text (the reported symptom: a
// window-title OSC whose head was evicted bleeds its trailing characters into
// the grid pane; see TestCapReplayTail_NoOSCLeak). So the crop must start where
// vtGroundScan reports ground state.
//
// Among ground-safe frame boundaries the smallest fitting suffix wins. If none
// fits the limit, we fall back to the LATEST ground-safe boundary seen (backing
// up to include a sequence's head is safe and cannot bleed a tail — correctness
// over the soft byte cap). The last frame is thus never split, and the result is
// never empty.
func capReplayTail(data []byte, limit uint32) []byte {
	if limit == 0 || len(data) <= int(limit) {
		return data
	}
	lim := int(limit)
	var sc vtGroundScan
	off := 0
	lastGround := 0 // start-of-stream is a ground-state boundary by definition
	for off < len(data) {
		if sc.ground() {
			// Smallest ground-safe suffix that fits the limit: take it.
			if off > 0 && len(data)-off <= lim {
				return data[off:]
			}
			lastGround = off
		}
		if len(data)-off < frameHeaderSize {
			break // malformed tail
		}
		total := frameHeaderSize + int(binary.BigEndian.Uint32(data[off+1:off+5]))
		if total <= 0 || off+total > len(data) {
			break // malformed frame; don't split it
		}
		sc.scan(data[off+frameHeaderSize : off+total])
		off += total
	}
	return data[lastGround:]
}

// encodeStdoutFrame wraps payload in one exec/frame Stdout frame (1-byte type +
// 4-byte big-endian length + payload), matching the wire format runnerPump
// forwards and the ring stores, so a synthesised frame is indistinguishable
// from a live one to the client's parser.
func encodeStdoutFrame(payload []byte) []byte {
	return encodeFrame(frame.FrameType_Stdout, payload)
}

// encodeSynthFrame wraps payload as a Synth frame: bytes this SERVER invented —
// a terminal-mode preamble, a screen repaint — rather than bytes the PTY
// emitted.
//
// They reach the terminal exactly like output and at exactly this position in
// the stream, so the type changes nothing about where they go. It exists so a
// reader that cares which is which can tell — `session snapshot --raw` promises
// the bytes the process actually produced, and cannot keep that promise if
// invented ones wear the same label.
func encodeSynthFrame(payload []byte) []byte {
	return encodeFrame(frame.FrameType_Synth, payload)
}

func encodeFrame(typ frame.FrameType, payload []byte) []byte {
	out := make([]byte, frameHeaderSize+len(payload))
	out[0] = byte(typ)
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
	// OnActivity fires when the session's PTY output quiescence crosses the
	// protocol.ActivityBusyThreshold edge in either direction (busy=true on
	// the first tick that sees fresh output, busy=false once output has been
	// quiet for the threshold). Edge-triggered with idleWatchTick granularity;
	// no per-frame cost. lastOutputUnixNano is the timestamp at detection.
	OnActivity func(taskID string, busy bool, lastOutputUnixNano int64)
	// OnObservers fires when the observer set changes — a viewer or cowriter
	// attached or left. It exists because those attaches deliberately do NOT
	// touch the task state machine (only a control attach moves
	// Running/Detached), so nothing else tells an event-driven client that the
	// counts moved. Always dispatched off the mux lock: it is called from
	// under m.mu on the drop path.
	OnObservers func(taskID string)
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

	// screen is the session's terminal state as a grid, fed from the same
	// frames the ring gets. It exists from the mux's first byte rather than
	// from the first attach, and that is the point: its whole value is having
	// seen output the ring has since evicted, so one created later could only
	// be seeded from the ring — which is the ring with extra steps.
	//
	// Its own mutex rather than m.mu, for the reason modeTracker has one: the
	// feed sits on runnerPump's hot path, and m.mu is held by fan-out and by
	// attach. vtgrid.Terminal has no internal locking of its own.
	screenMu sync.Mutex
	screen   *vtgrid.Terminal

	// runnerWriteMu serializes writes to the runner stream and keeps them
	// frame-atomic. With multi-writer (one control tui + N cowriters all
	// forwarding input), an unsynchronised write could interleave a cowriter
	// frame into the middle of a control frame and desync the runner's
	// frame-aligned reader. Every forwarder writes ONE complete frame under
	// this lock. Distinct from mu (which guards attach/viewer state).
	runnerWriteMu sync.Mutex

	// lastOutput is the unix-nano timestamp of the most recent Stdout/Stderr
	// frame received from the runner (0 = none yet). Control frames do not
	// count. Stamped by runnerPump on every output frame; read by List
	// enrichment and idle watchers. Byte-level quiescence of this value is
	// the busy/idle signal: an in-flight agent TUI repaints its spinner
	// ~every 100ms, an idle prompt emits nothing at all.
	lastOutput atomic.Int64

	// mainMark is the ring append-index of the frame at which the session last
	// returned to the primary screen (a full-screen app's alt-screen exit).
	// On reattach, when the session is currently on the primary screen, replay
	// starts here instead of at the ring head, skipping the dead alt-screen
	// episode whose verbatim replay would corrupt the display. Zero (the
	// default) means "no alt-screen exit recorded" → full replay. Atomic so the
	// runner pump can publish it without coordinating with the attach path.
	mainMark atomic.Int64

	// altTransitions records, in ring append-index order, every alternate-screen
	// switch the session has made, so replaySnapshot can ask the one question
	// that decides whether a finished episode has to be trimmed: was the session
	// on the alternate screen at the ring's OLDEST surviving frame?
	//
	// "Is the most recent entry still in the ring" is the tempting predicate and
	// is unsound. With two episodes E1(a1..x1) and E2(a2..x2), a ring starting
	// between a1 and x1 leaves a2 present while E1 straddles the start — and it
	// is E1's fragments that would paint the primary screen.
	//
	// Pruned lazily, when read, so the per-frame hot path never touches it;
	// altBeforeOldest carries the direction of the most recently pruned entry,
	// which IS the state at the oldest surviving frame.
	altMu           sync.Mutex
	altTransitions  []altTransition
	altBeforeOldest bool

	mu        sync.Mutex
	tui       trsf.BidirectionalStream
	tuiCancel context.CancelFunc

	viewers map[*viewerConn]struct{}

	// lastWinSize is the raw wire bytes of the most recent TerminalWindowSize
	// control frame seen on the tui→runner direction (the controlling client's
	// PTY size). Replayed verbatim to a new viewer ahead of the ring so a
	// read-only snapshot can size its terminal grid to match the size the
	// absolute-positioned output was painted at. An observer's own size is
	// discarded unless it holds exec_resize, so without this it could not learn
	// the size and would mis-render full-screen TUIs. Guarded by mu.
	lastWinSize []byte

	onDetach    func(taskID string)
	onAttach    func(taskID string)
	onObservers func(taskID string)
	onStop      func(taskID string)

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewSessionMux creates a SessionMux and starts the runner pump goroutine.
// parentCtx cancellation propagates to Stop. Hooks are installed before
// runnerPump starts, eliminating any race window.
func NewSessionMux(parentCtx context.Context, taskID string, runner trsf.BidirectionalStream, ring *RingBuffer, hooks SessionHooks) *SessionMux {
	ctx, cancel := context.WithCancel(parentCtx)
	m := &SessionMux{
		ctx:    ctx,
		cancel: cancel,
		taskID: taskID,
		runner: runner,
		ring:   ring,
		modes:  newModeTracker(),
		// 80x24 because the server has no size to use: the PTY's size reaches
		// it only as a TerminalWindowSize frame — from the opener's
		// applyInitialWindowSize, or from a client as it attaches — and the
		// first of those arrives right after the stream opens. So the default
		// is what the grid renders for the handful of bytes before anyone has
		// said, and the conventional terminal size is the honest answer to
		// "what does this session believe it is" in that window.
		screen:      vtgrid.New(80, 24),
		stopped:     make(chan struct{}),
		viewers:     make(map[*viewerConn]struct{}),
		onAttach:    hooks.OnAttach,
		onDetach:    hooks.OnDetach,
		onObservers: hooks.OnObservers,
		onStop:      hooks.OnStop,
	}
	go m.runnerPump()
	if hooks.OnActivity != nil {
		go m.activityWatcher(hooks.OnActivity, protocol.ActivityBusyThreshold, idleWatchTick)
	}
	return m
}

// activityWatcher is the resident busy/idle edge detector behind
// SessionHooks.OnActivity. Same lazy-poll idiom as ArmIdleWatcher: sample the
// lastOutput atomic every tick instead of touching the per-frame hot path.
// Initial state is idle (no output yet renders no badge anyway), so the
// first output frame produces a busy edge within one tick. threshold/tick are
// parameters only so tests can run the edge logic at fast timescales;
// production always passes protocol.ActivityBusyThreshold / idleWatchTick.
func (m *SessionMux) activityWatcher(fn func(taskID string, busy bool, lastOutputUnixNano int64), threshold, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	busy := false
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		lo := m.lastOutput.Load()
		nowBusy := lo != 0 && time.Now().UnixNano()-lo < threshold.Nanoseconds()
		if nowBusy != busy {
			busy = nowBusy
			fn(m.taskID, busy, lo)
		}
	}
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
				m.screenMu.Lock()
				_, _ = m.screen.Write(frameBytes[frameHeaderSize:])
				m.screenMu.Unlock()
				m.lastOutput.Store(time.Now().UnixNano())
			}
		}
		m.ring.Append(frameBytes)
		// If this frame carried the alt-screen exit (alt → primary), mark it as
		// the replay start point: everything before is a now-finished
		// full-screen episode that must not be replayed verbatim. The mark is
		// the just-appended frame's index, so replay includes the ESC[?1049l
		// itself (ensuring a reattaching client also leaves the alt buffer).
		if nowAlt := m.modes.onAltScreen(); nowAlt != wasAlt {
			idx := m.ring.AppendCount() - 1
			if wasAlt && !nowAlt {
				m.mainMark.Store(int64(idx))
			}
			m.altMu.Lock()
			m.altTransitions = append(m.altTransitions, altTransition{index: idx, entered: nowAlt})
			m.altMu.Unlock()
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

// fanoutToViewersLocked delivers one complete frame to every observer
// (viewers AND cowriters, both live in m.viewers) non-blocking, dropping any
// whose bounded queue is full — identical policy to runnerPump's output
// fan-out. Caller MUST hold m.mu. Used to propagate a control-client resize to
// observers so a long-lived read-only renderer tracks the current PTY size
// instead of the stale attach-time size.
func (m *SessionMux) fanoutToViewersLocked(fb []byte) {
	for v := range m.viewers {
		select {
		case v.ch <- fb:
		default:
			m.dropViewerLocked(v)
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
		replay = append(replay, encodeSynthFrame(pre)...)
	}
	replay = append(replay, m.replaySnapshot()...)
	// The screen last, so it overwrites whatever the replayed history left on
	// it. Never capped: a replay limit bounds HISTORY, and the screen is not
	// history.
	if rp := m.screenRepaint(); len(rp) > 0 {
		replay = append(replay, encodeSynthFrame(rp)...)
	}
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

// notifyObservers dispatches the observer-set-changed hook without holding
// m.mu — dropViewerLocked runs under the lock, and a hook that publishes to
// pubsub from there would invert the lock order against runnerPump's fan-out.
func (m *SessionMux) notifyObservers() {
	if m.onObservers != nil {
		go m.onObservers(m.taskID)
	}
}

// AttachViewer adds a read-only observer (its input is discarded). Unlike
// Attach it does NOT take the writer slot or fire onAttach. replayLimit caps the
// replayed ring bytes (0 = full).
func (m *SessionMux) AttachViewer(ctx context.Context, stream trsf.BidirectionalStream, replayLimit uint32, allowResize bool) error {
	return m.attachObserver(stream, false, replayLimit, allowResize)
}

// AttachCoWriter adds a non-takeover writer: it observes output like a viewer
// AND forwards its input to the runner, EXCEPT TerminalWindowSize frames, which
// are dropped — a cowriter has no size authority (only the control client owns
// the PTY size). Lets an agent co-drive a session alongside a human controller
// without kicking them; the human keeps size ownership so the PTY isn't resized
// out from under them.
func (m *SessionMux) AttachCoWriter(ctx context.Context, stream trsf.BidirectionalStream, replayLimit uint32, allowResize bool) error {
	return m.attachObserver(stream, true, replayLimit, allowResize)
}

// attachObserver registers a viewer (forwardInput=false) or cowriter
// (forwardInput=true): adds it to the output fan-out, replays size+modes+ring,
// and starts its output pump plus either an input drain or an input forwarder.
func (m *SessionMux) attachObserver(stream trsf.BidirectionalStream, forwardInput bool, replayLimit uint32, allowResize bool) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("session_mux: stopped")
	}
	vctx, vcancel := context.WithCancel(m.ctx)
	v := &viewerConn{stream: stream, ch: make(chan []byte, viewerQueueDepth), cancel: vcancel, cowriter: forwardInput, allowResize: allowResize}
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
		replay = append(replay, encodeSynthFrame(pre)...)
	}
	// The winsize + mode preamble above are always sent in full (small, and the
	// preamble carries the alt-screen/DEC-mode state). Only the ring SNAPSHOT is
	// capped: an observer that requested a limit (a monitoring grid pane) gets
	// just the last replayLimit bytes, frame-aligned, instead of the whole ~1 MiB
	// ring it would never render.
	replay = append(replay, capReplayTail(m.replaySnapshot(), replayLimit)...)
	// Outside the cap, for the same reason as the control path: replayLimit is
	// how much history this observer wants, and the screen is not history. A
	// monitoring pane that asks for none still gets a correct screen.
	if rp := m.screenRepaint(); len(rp) > 0 {
		replay = append(replay, encodeSynthFrame(rp)...)
	}
	// The replay is QUEUED, not written here, and it goes in while m.mu is
	// still held. That is what keeps it ahead of live frames: runnerPump's
	// fan-out needs the same lock to enqueue anything, so nothing can slip in
	// front of an entry made under it. Ordering comes from the lock rather than
	// from writing first.
	//
	// It used to be a synchronous AppendData, which was survivable only while
	// the replay could be empty — an attach to a session with nothing buffered
	// wrote nothing and could not block. The screen repaint is never empty (a
	// blank 80x24 screen still costs ~380 bytes), so that write would now
	// happen on EVERY attach and an observer that cannot take it would wedge
	// the attach call itself. This file's policy is that an observer which
	// cannot keep up is dropped and never blocks anyone, and the queue is where
	// that policy lives.
	//
	// The cost is that a doomed observer now fails asynchronously, by being
	// dropped, instead of returning an error from the attach. v.ch is fresh
	// here, so the send below cannot fail for want of room.
	if len(replay) > 0 {
		v.ch <- replay
	}
	m.mu.Unlock()

	go m.viewerOutputPump(vctx, v)
	go m.observerInputPump(vctx, v)
	// Announce only once the observer is actually streaming, so a subscriber
	// that refreshes on the event sees the count it is being told about.
	m.notifyObservers()
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

// anyViewerForTest returns one attached observer, or nil. Test-only accessor:
// the viewers map is unexported and a test that wants to drop "one of them"
// has no other way to name a member.
func (m *SessionMux) anyViewerForTest() *viewerConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	for v := range m.viewers {
		return v
	}
	return nil
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
	m.notifyObservers()
}

// ObserverCounts reports the attached observers split by kind: viewers (input
// discarded) and cowriters (input forwarded to the runner). The CONTROL attach
// is not an observer and is not counted here — ask IsAttached for that.
//
// Both are read under one lock so a caller can never see a total that no single
// moment produced.
func (m *SessionMux) ObserverCounts() (viewers, cowriters int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for v := range m.viewers {
		if v.cowriter {
			cowriters++
		} else {
			viewers++
		}
	}
	return viewers, cowriters
}

// ViewerCount reports attached VIEWERS only (cowriters excluded). Delegates so
// the two never disagree.
func (m *SessionMux) ViewerCount() int {
	v, _ := m.ObserverCounts()
	return v
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
			if err := m.applyWinSizeFrame(fb); err != nil {
				return nil, false
			}
			acc = acc[total:]
			continue
		}
		if err := m.writeFrameToRunner(fb); err != nil {
			return nil, false
		}
		acc = acc[total:]
	}
	return carryTail(acc), true
}

// observerInputPump reads an observer's incoming direction and decides each
// frame on TWO INDEPENDENT axes — whether this observer may type (cowriter) and
// whether its resizes are honoured (allowResize). One pump for both kinds: a
// viewer is simply an observer with neither, and reading is unconditional
// either way, because leaving the bidi recv side undrained backpressures and
// wedges the streams layer (project_trsf_accept_queue_wedge).
//
// Drops the observer on EOF/error; Stops the mux on runner write failure.
func (m *SessionMux) observerInputPump(ctx context.Context, v *viewerConn) {
	const maxRead = 32 * 1024
	var acc []byte
	for {
		data, eof, err := v.stream.ReadDirectContext(ctx, maxRead)
		if len(data) > 0 {
			var ok bool
			acc, ok = m.forwardObserverFrames(v, append(acc, data...))
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

// forwardObserverFrames applies the two axes per COMPLETE frame. Anything not
// permitted is read and discarded, exactly as the whole stream used to be for a
// viewer. Returns the unconsumed tail and false on runner write failure.
func (m *SessionMux) forwardObserverFrames(v *viewerConn, acc []byte) ([]byte, bool) {
	for len(acc) >= frameHeaderSize {
		total := frameHeaderSize + int(binary.BigEndian.Uint32(acc[1:5]))
		if len(acc) < total {
			break
		}
		fb := acc[:total]
		switch {
		case frameIsWinSize(fb):
			// Goes through the SAME helper as the control client's resize, so
			// an observer resize cannot drift from it: the size has to be
			// remembered for the replay preamble and fanned out to the other
			// observers, not merely handed to the runner.
			if v.allowResize {
				if err := m.applyObserverWinSize(fb); err != nil {
					return nil, false
				}
			}
		case v.cowriter:
			if err := m.writeFrameToRunner(fb); err != nil {
				return nil, false
			}
		}
		acc = acc[total:]
	}
	return carryTail(acc), true
}

// applyObserverWinSize honours an observer's resize only while the CONTROL seat
// is EMPTY. The size belongs to the control attach — that is what "control"
// means and it has always been true; exec_resize does not take it away, it lets
// an observer stand in when nobody holds it.
//
// The alternative, last-writer-wins, was rejected after use: an observer resize
// would redraw a human's terminal at a size they did not choose, and their next
// SIGWINCH would silently undo it. Flapping neither party asked for. And the
// case exec_resize exists for is precisely the empty seat — someone who only
// wants to LOOK uses the grid view or the WebUI preview, which are view and
// cowrite attaches that need no size of their own.
//
// The check-then-apply is not atomic with a concurrent control attach; that
// race is benign, because a control client sends its own size as it attaches
// and therefore wins by arriving second.
func (m *SessionMux) applyObserverWinSize(fb []byte) error {
	m.mu.Lock()
	occupied := m.tui != nil
	m.mu.Unlock()
	if occupied {
		return nil // silently ignored: control owns the size
	}
	return m.applyWinSizeFrame(fb)
}

// applyWinSizeFrame records a new PTY size, replays it to the other observers,
// and hands it to the runner. The control client reaches this unconditionally;
// an observer only through applyObserverWinSize.
func (m *SessionMux) applyWinSizeFrame(fb []byte) error {
	cp := append([]byte(nil), fb...)
	m.mu.Lock()
	m.lastWinSize = cp
	m.fanoutToViewersLocked(cp)
	m.mu.Unlock()
	// The grid resizes here rather than at either caller, because this is where
	// the two entry points meet: a control client's frame and — while the
	// control seat is empty — an exec_resize observer's. A resize landing
	// between two feeds is what a real terminal does.
	if cols, rows, ok := winSizeOf(fb); ok && cols > 0 && rows > 0 {
		m.screenMu.Lock()
		m.screen.Resize(cols, rows)
		m.screenMu.Unlock()
	}
	return m.writeFrameToRunner(fb)
}

// screenRepaint returns the bytes that reconstruct the session's current screen
// on an observer, whatever state that observer is in.
func (m *SessionMux) screenRepaint() []byte {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return m.screen.Repaint()
}

// screenSize reports the grid's current dimensions.
func (m *SessionMux) screenSize() (cols, rows int) {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return m.screen.Size()
}

// writeFrameToRunner writes one complete frame to the runner under
// runnerWriteMu, keeping multi-writer forwards frame-atomic.
func (m *SessionMux) writeFrameToRunner(fb []byte) error {
	m.runnerWriteMu.Lock()
	defer m.runnerWriteMu.Unlock()
	return m.runner.AppendData(false, fb)
}

// winSizeOf decodes fb as a TerminalWindowSize control frame, reporting false
// for anything else. Both callers need the same decode — one to route the
// frame, one to resize the grid — so it happens once.
func winSizeOf(fb []byte) (cols, rows int, ok bool) {
	if len(fb) < frameHeaderSize || frame.FrameType(fb[0]) != frame.FrameType_Control {
		return 0, 0, false
	}
	f := &frame.Frame{}
	if err := f.Read(bytes.NewReader(fb)); err != nil {
		return 0, 0, false
	}
	ctrl := f.Control()
	if ctrl == nil || ctrl.Type != frame.ControlType_TerminalWindowSize {
		return 0, 0, false
	}
	ws := ctrl.TerminalWindowSize()
	if ws == nil {
		return 0, 0, false
	}
	return int(ws.Columns), int(ws.Rows), true
}

// frameIsWinSize reports whether fb is a complete TerminalWindowSize control frame.
func frameIsWinSize(fb []byte) bool {
	_, _, ok := winSizeOf(fb)
	return ok
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

// replaySnapshot returns the ring bytes to replay to a (re)attaching client,
// with every reply-soliciting sequence removed on the way out.
//
// The stripping is here rather than at either call site because BOTH of them
// want it and neither is the natural owner: a replay re-asks every question
// the session ever asked, of a terminal that answers all of them, at a moment
// when the asker is usually gone — so the answers arrive as INPUT to whatever
// reads the pty now. The live fan-out in runnerPump is deliberately NOT
// filtered; see stripReplyQueries for why that asymmetry is the whole point.
func (m *SessionMux) replaySnapshot() []byte {
	return stripReplyQueriesFrames(m.replayRing())
}

// replayRing chooses WHICH ring bytes replaySnapshot should hand out.
// On the primary screen it starts from the last alt-screen exit (mainMark),
// skipping a finished full-screen episode whose verbatim replay — absolute-
// cursor frame fragments with no enclosing alt-screen — corrupts the display.
// While a full-screen app is still live (in the alt screen) it replays the
// whole ring, since the app repaints over any partial frame on the next tick.
func (m *SessionMux) replayRing() []byte {
	if m.modes.onAltScreen() {
		return m.ring.Snapshot()
	}
	if !m.altAtOldestSurviving() {
		// The ring begins on the primary screen, so any finished episode inside
		// it enters and leaves the alternate buffer on its own: its frames paint
		// THAT buffer and touch neither the primary screen nor the scrollback.
		// Trimming here would throw away the history from before the episode for
		// no benefit.
		return m.ring.Snapshot()
	}
	// The ring begins INSIDE an episode, so its opening frames are
	// absolute-cursor fragments with no ESC[?1049h ahead of them. Replayed as
	// they are, they paint the primary screen and scroll into the scrollback,
	// where the repaint that fixes the screen cannot reach them.
	return m.ring.SnapshotFrom(int(m.mainMark.Load()))
}

// altTransition is one alternate-screen switch and where it sits in the ring.
type altTransition struct {
	index   int
	entered bool
}

// altAtOldestSurviving reports whether the session was on the alternate screen
// at the ring's oldest surviving frame, pruning transitions that have fallen
// out of the window as it goes.
func (m *SessionMux) altAtOldestSurviving() bool {
	oldest := m.ring.OldestIndex()
	m.altMu.Lock()
	defer m.altMu.Unlock()
	for len(m.altTransitions) > 0 && m.altTransitions[0].index < oldest {
		m.altBeforeOldest = m.altTransitions[0].entered
		m.altTransitions = m.altTransitions[1:]
	}
	return m.altBeforeOldest
}

// RingAppendCount returns how many frames the ring has ever been given, which
// lets a test wait for a specific frame to have landed rather than for a byte
// count that eviction makes unstable.
func (m *SessionMux) RingAppendCount() int { return m.ring.AppendCount() }

// RingBufferLen returns the number of bytes currently stored in the ring buffer.
func (m *SessionMux) RingBufferLen() int { return m.ring.Len() }

// LastOutputUnixNano returns the unix-nano timestamp of the most recent
// Stdout/Stderr frame from the runner, or 0 if none has arrived yet.
func (m *SessionMux) LastOutputUnixNano() int64 { return m.lastOutput.Load() }

// idleWatchTick is the poll interval of an armed idle watcher. Worst-case
// fire latency is threshold+idleWatchTick — irrelevant at human/agent
// timescales, and polling an atomic avoids any per-frame timer churn.
const idleWatchTick = 500 * time.Millisecond

// ArmIdleWatcher registers fn to be called EXACTLY ONCE, from its own
// goroutine, when the session's PTY output has been quiescent for threshold
// — or with stopped=true when the session stops first. Semantics:
//   - already idle at arm time => fires immediately;
//   - no output ever yet (lastOutput==0) => waits for the first output frame,
//     then for the idle edge (arming during process boot means the caller
//     wants the boot turn's end, not an instant fire);
//   - one-shot: the watcher is gone after firing either way.
func (m *SessionMux) ArmIdleWatcher(threshold time.Duration, fn func(stopped bool, lastOutputUnixNano int64)) {
	go func() {
		t := time.NewTicker(idleWatchTick)
		defer t.Stop()
		for {
			lo := m.lastOutput.Load()
			if m.ctx.Err() != nil {
				fn(true, lo)
				return
			}
			if lo != 0 && time.Now().UnixNano()-lo >= threshold.Nanoseconds() {
				fn(false, lo)
				return
			}
			select {
			case <-m.ctx.Done():
				fn(true, m.lastOutput.Load())
				return
			case <-t.C:
			}
		}
	}()
}

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
		// Cleared directly rather than through dropViewerLocked, so no
		// OnObservers fires here: the session is ending, and the task status
		// event that follows carries the counts (zero — the mux is gone). An
		// extra observers event would be a second wake for one outcome.
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
