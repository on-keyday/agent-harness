package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// PaneStreamer view-attaches one session read-only and renders its live screen
// into a headless VT emulator, croppable to a pane. It NEVER sends a size — the
// grid has no size authority (Global Constraint) — it sizes its emulator to the
// size the server replays and resizes on mid-stream winsize frames.
type PaneStreamer struct {
	taskID string

	mu      sync.Mutex
	emu     *vtgrid.Terminal
	cols    int
	rows    int
	err     error
	stream  *agentexec.CommandExecutionStream
	cancel  context.CancelFunc
	stopped bool // Stop() ran; a still-attaching pump must close its stream

	// startDelay staggers this pane's FIRST attach (set by GridModel.Open per
	// pane index). Opening N panes fires N attaches over the one shared client at
	// once; their replay bursts starve the later attaches' control responses so
	// some hang at rx=0 forever. Spacing the first attach out avoids the storm.
	startDelay time.Duration

	// Diagnostics (HARNESS_GRID_DIAG): count bytes/reads/attaches/panics so a
	// black pane can render its own state — is this "no bytes arrived" or "bytes
	// arrived but the emulator is blank"? Guarded by mu.
	rxBytes  int
	reads    int
	attaches int
	vtPanics int

	// Arrival RATE over a rolling window, which the cumulative counters above
	// cannot answer: `rx=41230` says bytes arrived at some point, never whether
	// any are arriving NOW. Reading a total requires two screenshots and a
	// subtraction — the same "run it twice, the delta is the signal" the trsf
	// dump has to tell its reader — and a pane you are watching because it looks
	// stuck is exactly where that second sample is expensive.
	//
	// This is the CONTINUOUS form of what `session snapshot` reports as `live`:
	// same quantities, but measured off a stream that is already being read
	// rather than sampled over one 1500 ms capture. It needs no anchoring the way
	// the snapshot does — this stream's replay burst arrives once at attach and
	// then the window rolls past it, whereas a fresh capture is mostly replay.
	// Guarded by mu.
	//
	// Only a COMPLETED window is ever displayed. The first version reported the
	// open one as count/elapsed so an idle pane would "decay to zero on its
	// own" — which it did, visibly, one render at a time: the count stops
	// growing while the divisor does not, so a silent pane showed a number
	// ticking downward forever. Operator, immediately: 「なんかすげー勢いで数値が
	// カウントダウンするみたいになってますけど」. A diagnostic that ANIMATES while
	// nothing is happening reports the renderer's frame rate, not the session's.
	// So the displayed pair changes only when a window rolls, and idleness is a
	// separate, discrete fact — see rateLocked.
	rateStart   time.Time // window start; zero until the first byte ever arrives
	lastArrival time.Time // when a chunk last reached the model; drives the idle cut
	rateBytes   int       // accumulating in the current window
	rateFrames  int
	lastBps     float64 // last COMPLETED window — this is what is shown
	lastFps     float64
}

// rateWindow is how long a rate window runs before it is rolled. One second is
// the shortest window that reads as a rate to a human and the longest that still
// reacts within one glance at the grid.
const rateWindow = time.Second

func NewPaneStreamer(taskID string, defRows, defCols int) *PaneStreamer {
	if defRows <= 0 {
		defRows = 24
	}
	if defCols <= 0 {
		defCols = 80
	}
	return &PaneStreamer{taskID: taskID, emu: vtgrid.New(defCols, defRows), cols: defCols, rows: defRows}
}

func (p *PaneStreamer) TaskID() string { return p.taskID }

// DiagLine reports the pane's internal state as a single line, so a black pane
// can render WHY it's black instead of leaving us to guess. It bisects the two
// hypotheses from one screenshot: rx=0 => no bytes ever arrived (attach/stream
// problem); rx>0 with lc=-1 => bytes arrived but the emulator painted nothing
// (VT processing / sizing / panic). Gated by HARNESS_GRID_DIAG at the call site.
func (p *PaneStreamer) DiagLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emu == nil {
		return "diag: emu=nil (stopped)"
	}
	lc := lastContentRow(p.emu, p.cols, p.rows)
	_, cy, _ := p.emu.Cursor()
	errS := "-"
	if p.err != nil {
		errS = p.err.Error()
		if len(errS) > 24 {
			errS = errS[:24]
		}
	}
	bps, fps := p.rateLocked(time.Now())
	return fmt.Sprintf("rx=%d rd=%d %.0fB/s %.1frd/s at=%d vtp=%d sz=%dx%d lc=%d cy=%d err=%s",
		p.rxBytes, p.reads, bps, fps, p.attaches, p.vtPanics, p.cols, p.rows, lc, cy, errS)
}

// rateLocked reports the pane's arrival rate in bytes and frames per second.
// Caller holds mu.
//
// Two states, and nothing in between: either the pane has produced something
// within the last rateWindow — in which case the last COMPLETED window's rate
// stands, unchanged until the next one rolls — or it has not, in which case it
// is 0. Silence is a step, not a slope.
//
// That is deliberate and was learned the hard way. Deriving a rate from the OPEN
// window is arithmetically fine and visually wrong: the numerator freezes when
// output stops while the denominator keeps growing, so the pane renders a
// different number on every tick while nothing whatsoever is happening. On a
// grid of six panes that reads as six counters racing downward.
//
// The cost is a beat of lag at both ends: nothing is reported for the first
// rateWindow of a new pane (no window has completed yet), and a burst shorter
// than rateWindow is reported for a full window after it ends. Both are
// preferable to a number that moves on its own — a debug overlay is read by
// eye, and a still value is what makes a changing one mean something.
//
// They are frames, not a frame RATE in the display sense: one screen repaint can
// arrive as several reads and several repaints can coalesce into one, so this
// counts transport boundaries. `rd/s` rather than `fps` in the line for exactly
// that reason.
func (p *PaneStreamer) rateLocked(now time.Time) (bps, fps float64) {
	if p.lastArrival.IsZero() || now.Sub(p.lastArrival) >= rateWindow {
		// Never produced, or silent for at least a full window. 0 is the
		// measurement here, not a missing value.
		return 0, 0
	}
	return p.lastBps, p.lastFps
}

func (p *PaneStreamer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *PaneStreamer) Start(ctx context.Context, c *cli.Client) {
	cctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()

	// No drain goroutine: the screen model answers no device queries, so it
	// never writes anything back and there is nothing that could block a Write.
	// x/vt did answer DA1/DA2/DSR on its own output side, which is why this
	// used to run an io.Copy(io.Discard) beside every pane.
	go p.pump(cctx, c)
}

// gridReplayLimit caps the scrollback the server replays to a pane: it shows
// only a small bottom crop, so it does NOT need the full ~1 MiB ring. Opening a
// grid of N sessions otherwise pulls ~1 MiB × N of replay over the one shared
// client connection; the mode preamble (alt-screen, DEC modes) is sent
// separately, so a full-screen app still repaints correctly from a capped
// replay.
const gridReplayLimit = 128 * 1024

// paneReattachBase / paneReattachMax bound the backoff between reattach
// attempts (see pump). Base is short so a transient drop heals almost
// invisibly; the cap stops N panes from re-flooding the shared link if a
// session stays too fast for the link.
const (
	paneReattachBase = 300 * time.Millisecond
	paneReattachMax  = 3 * time.Second
)

// paneAttachTimeout bounds ONE attach attempt. RoundTripTaskControl has no
// timeout of its own, so a control response starved behind other panes' replay
// bursts (or otherwise lost) would block the attach — and the pane — FOREVER at
// rx=0 (the confirmed permanent-black case). Capping each attempt turns that
// hang into a retryable error the reattach loop recovers from.
const paneAttachTimeout = 8 * time.Second

// pump attaches the pane's cowrite observer and streams output into the
// emulator, REATTACHING with backoff whenever the stream ends without us
// stopping it. That non-stop end is the normal, expected outcome of the
// server's slow-observer policy: each observer is fed through a bounded
// fan-out queue and is DROPPED (its stream force-closed) the moment it can't
// keep up — which a busy session on a slow or heavily-multiplexed link (e.g. N
// grid panes sharing one connection) trips routinely. Without reattach a
// dropped pane goes permanently black; with it the pane simply resyncs from
// the replay snapshot. A truly finished/pruned session is not retried: its
// reattach returns a terminal status (IsAttachPermanent) and the loop exits.
//
// Cowrite (not View) so a focused pane can forward input (SendInput) while
// staying small; idle panes send nothing, so cowrite is output-equivalent to
// view until the user types.
func (p *PaneStreamer) pump(ctx context.Context, c *cli.Client) {
	// Stagger the first attach so N panes don't storm the shared client at once.
	if p.startDelay > 0 && !sleepCtx(ctx, p.startDelay) {
		return
	}
	backoff := paneReattachBase
	for {
		if ctx.Err() != nil {
			return
		}
		// Bound each attempt: a hung attach must become a retryable error, not a
		// permanent rx=0 black pane. The returned stream outlives attachCtx (it
		// only scopes the RPC + stream-visibility wait), so cancelling here is safe.
		attachCtx, cancelAttach := context.WithTimeout(ctx, paneAttachTimeout)
		stream, _, kind, err := c.AttachSessionWithReplayLimit(attachCtx, p.taskID, protocol.AttachMode_Cowrite, gridReplayLimit)
		cancelAttach()
		if err == nil && kind == protocol.TaskKind_Stream {
			// Grid rows are IsPTYKind-filtered, so a pane should never point at
			// a stream task — but this pane renders a VT screen, and painting
			// NDJSON into it would be worse than an error line. Permanent: a
			// task's kind never changes, so reattaching cannot fix it.
			_ = stream.Close()
			p.setErr(fmt.Errorf("event-stream session: no terminal to render"))
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				return // Stop()/Close() cancelled us — not a failure to surface.
			}
			// attachCtx timeout (DeadlineExceeded) falls through here as a normal
			// retryable error since the long-lived ctx is still alive.
			p.setErr(err)
			if cli.IsAttachPermanent(err) {
				return // session gone/finished: reattach can never succeed.
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		p.mu.Lock()
		if p.stopped {
			// Stop() ran while AttachSession was in flight: it captured a nil stream
			// (this one didn't exist yet), so if we kept it, the stream would leak —
			// a server-side observer that never detaches. Close it and exit.
			p.mu.Unlock()
			_ = stream.Close()
			return
		}
		p.stream = stream
		p.err = nil // reattached: clear any prior drop error / "(ended)" header.
		p.attaches++
		p.mu.Unlock()

		start := time.Now()
		rerr := p.readStream(stream)
		_ = stream.Close()

		p.mu.Lock()
		p.stream = nil
		stopped := p.stopped
		p.mu.Unlock()
		if stopped || ctx.Err() != nil {
			return // torn down by Stop()/Close(): don't reattach.
		}
		// The stream ended on its own: server dropped this observer (fell behind)
		// or the session ended. Record the reason, then reattach — the next
		// attach either resyncs (drop) or returns a terminal status (ended) and
		// the loop above stops. A long-lived attach that only just dropped
		// recovers fast; a drop-storm keeps the escalated backoff.
		p.setErr(rerr)
		if time.Since(start) > 5*time.Second {
			backoff = paneReattachBase
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// readStream copies one attached stream into the emulator until it errors
// (EOF, drop, or wire error), returning that error. Returns nil only when the
// emulator was torn down mid-read (Stop raced), which pump treats as a stop.
func (p *PaneStreamer) readStream(stream *agentexec.CommandExecutionStream) error {
	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	lastRows, lastCols := 0, 0
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.reads++
			p.mu.Unlock()
			rows, cols, ok := stream.LastWindowSize()
			resize := ok && (int(rows) != lastRows || int(cols) != lastCols) && rows > 0 && cols > 0
			if !p.feed(buf[:n], int(rows), int(cols), resize) {
				return nil // emulator torn down (Stop raced)
			}
			if resize {
				lastRows, lastCols = int(rows), int(cols)
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}

// nextBackoff doubles cur, clamped to paneReattachMax.
func nextBackoff(cur time.Duration) time.Duration {
	n := cur * 2
	if n > paneReattachMax {
		n = paneReattachMax
	}
	return n
}

// sleepCtx waits d or until ctx ends; returns false if ctx ended first (caller
// should stop rather than retry).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// feed applies one chunk of PTY bytes to the screen model under the lock,
// resizing first when the session's window size changed. Returns false only if
// the model was already torn down (Stop raced).
//
// The recover stays even though the panic it was written for is gone. It was
// added for x/vt indexing out of range on a scroll against a region left stale
// by a resize — vtgrid resets the region as part of resizing, and
// TestResizeClampsScrollRegion holds it there. But a read-only monitoring pane
// must never be able to take down the whole TUI, and that is worth a deferred
// function whether or not a specific way in is known.
func (p *PaneStreamer) feed(data []byte, rows, cols int, resize bool) (alive bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emu == nil {
		return false
	}
	alive = true
	// recover is registered AFTER the Unlock defer, so on panic it runs first
	// (while the lock is still held), then the lock is released; alive stays
	// true so the pump continues rather than treating a recovered panic as
	// teardown. On panic we RESET the scroll region (ESC[r): the panic is almost
	// always a scroll against a region taller than the current buffer (a stale
	// DECSTBM before a resize landed), and if left as-is the emulator keeps
	// panicking on every later scroll and the pane goes PERMANENTLY BLACK.
	// Resetting the region lets the rest of the replay and live output render.
	defer func() {
		if r := recover(); r != nil {
			p.vtPanics++
		}
	}()
	if resize {
		// Resize clamps the scroll region itself, so the `ESC[r` that used to
		// follow this call is gone with the emulator that needed it.
		p.emu.Resize(cols, rows)
		p.cols, p.rows = cols, rows
	}
	p.rxBytes += len(data)
	p.recordArrivalLocked(len(data), time.Now())
	p.emu.Write(data)
	return alive
}

// recordArrivalLocked folds one chunk into the rate window, rolling the window
// once it has run a full rateWindow. Caller holds mu.
//
// It lives here rather than beside `p.reads++` in readStream because this is the
// point where a chunk is known to have REACHED the screen model: a read that
// races Stop is counted by neither, so the rate cannot outlive the pane it
// describes.
func (p *PaneStreamer) recordArrivalLocked(n int, now time.Time) {
	if p.rateStart.IsZero() {
		p.rateStart = now
	}
	p.lastArrival = now
	p.rateBytes += n
	p.rateFrames++
	if elapsed := now.Sub(p.rateStart); elapsed >= rateWindow {
		secs := elapsed.Seconds()
		p.lastBps = float64(p.rateBytes) / secs
		p.lastFps = float64(p.rateFrames) / secs
		p.rateStart, p.rateBytes, p.rateFrames = now, 0, 0
	}
}

// setErr records the pane's latest stream error (shown as "(ended)" in the
// header). It overwrites rather than latching the first error: the pump clears
// p.err on a successful reattach and re-sets it on the next drop, so the header
// tracks the pane's current state instead of a stale one-shot failure.
func (p *PaneStreamer) setErr(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
}

// SendInput forwards raw key bytes to the session over the cowrite stream (the
// server relays them to the runner's PTY without taking over the controlling
// client). No-op before the stream is attached or after Stop. Errors are
// ignored — a dropped keystroke on a monitoring pane is not worth surfacing.
func (p *PaneStreamer) SendInput(data []byte) {
	p.mu.Lock()
	s := p.stream
	p.mu.Unlock()
	if s == nil || len(data) == 0 {
		return
	}
	_, _ = s.Stdin().Write(data)
}

// Stop is idempotent: a second call captures all-nil fields and does nothing.
// Render already snapshots p.emu under p.mu and returns "" if nil, so nil-ing
// p.emu here is safe against a concurrent Render.
func (p *PaneStreamer) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	stream := p.stream
	p.cancel = nil
	p.stream = nil
	p.emu = nil      // nothing to close: the model owns no goroutine and no pipe
	p.stopped = true // a pump still inside AttachSession will close its stream itself
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.Close()
	}
}

// Render returns a bottom-left crop of the emulator grid at width×height cells,
// WITH color/attributes: adjacent cells sharing a style are coalesced into one
// lipgloss-styled run so the pane looks like the session (claude/vim colors are
// preserved). Activity in shells and full-screen agents concentrates at the
// bottom, so the bottom rows are the informative ones when a pane is smaller
// than the real grid. Wide (CJK) cells advance the scan by cell.Width.
func (p *PaneStreamer) Render(width, height int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	emu := p.emu
	cols, rows := p.cols, p.rows
	if emu == nil || width <= 0 || height <= 0 {
		return ""
	}
	// Anchor the crop's bottom to where content actually ends, NOT the geometric
	// bottom. Two cases must both work:
	//   - a short shell at the TOP of a tall screen (e.g. after a control attach
	//     resized its PTY taller than the pane) — the geometric bottom is empty,
	//     so cropping there shows a blank pane (the "grid black after reattach"
	//     bug);
	//   - a full screen that SCROLLED (recent output at the bottom) whose app
	//     parked the cursor higher up (claude, vim, …) — anchoring to the cursor
	//     alone would show stale top content and hide the recent bottom.
	// The last non-blank row handles both; max with the cursor keeps the live
	// line visible if it sits below the last painted content.
	bottom := lastContentRow(emu, cols, rows) + 1
	if _, cy, _ := emu.Cursor(); cy+1 > bottom {
		bottom = cy + 1
	}
	if bottom < 1 {
		bottom = 1
	}
	if bottom > rows {
		bottom = rows
	}
	startY := bottom - height
	if startY < 0 {
		startY = 0
	}
	endY := startY + height
	if endY > rows {
		endY = rows
	}
	var b strings.Builder
	for y := startY; y < endY; y++ {
		if y > startY {
			b.WriteByte('\n')
		}
		x := 0
		painted := 0
		// Coalesce adjacent cells with the same style into one lipgloss run to
		// keep the escape volume down.
		var run strings.Builder
		runKey := ""
		runStyle := lipgloss.NewStyle()
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(runStyle.Render(run.String()))
				run.Reset()
			}
		}
		for x < cols && painted < width {
			cell := emu.CellAt(x, y)
			w := cellPaneWidth(cell)
			// A wide (CJK/box-drawing) cell that would straddle the right edge
			// must NOT be emitted: its visual width would push the line past
			// `width`, and the caller's fixed-width lipgloss box then WRAPS the
			// overflow onto a new row, inflating the pane past its budgeted
			// height (the whole grid then overflows the terminal and the top is
			// clipped). Pad the remaining columns with spaces and stop instead.
			if painted+w > width {
				flush()
				for painted < width {
					b.WriteByte(' ')
					painted++
				}
				break
			}
			if key := cellStyleKey(cell); key != runKey {
				flush()
				runKey = key
				runStyle = cellLipgloss(cell)
			}
			txt := emu.Text(cell)
			if txt == "" {
				// The continuation half of a wide glyph. The scan advances by
				// the glyph's width so it is normally stepped over; guard
				// anyway so a stray one cannot stall the loop.
				x++
				continue
			}
			run.WriteString(txt)
			painted += w
			x += w
		}
		flush()
	}
	return b.String()
}

// cellStyleKey is a cheap comparable identity for a cell's style, used to
// coalesce equal-styled runs. "" is the default (unstyled) cell.
//
// The underline COLOUR is deliberately not in the key: lipgloss cannot express
// one, so two cells differing only there render identically and splitting the
// run would cost escapes for nothing.
func cellStyleKey(cell vtgrid.Cell) string {
	fg, bg := cell.FG.Hex(), cell.BG.Hex()
	if fg == "" && bg == "" && cell.Attr == 0 && cell.Under == vtgrid.UnderlineNone {
		return "" // unstyled: coalesce with default runs
	}
	return fmt.Sprintf("%s|%s|%d|%d", fg, bg, cell.Attr, cell.Under)
}

// cellLipgloss builds the lipgloss style for a cell (fg/bg + notable attrs), so
// a run rendered through it reproduces the session's colors and emphasis.
//
// Colours go through Hex(), which resolves a palette index for us; lipgloss
// then downsamples to whatever the terminal actually supports. That resolution
// is why a pane loses the distinction between `ESC[31m` and `ESC[38;5;1m` that
// the model keeps — lipgloss re-encodes from a hex string and there is nowhere
// to put the original spelling.
func cellLipgloss(cell vtgrid.Cell) lipgloss.Style {
	st := lipgloss.NewStyle()
	if h := cell.FG.Hex(); h != "" {
		st = st.Foreground(lipgloss.Color(h))
	}
	if h := cell.BG.Hex(); h != "" {
		st = st.Background(lipgloss.Color(h))
	}
	if cell.Attr&vtgrid.AttrBold != 0 {
		st = st.Bold(true)
	}
	if cell.Attr&vtgrid.AttrFaint != 0 {
		st = st.Faint(true)
	}
	if cell.Attr&vtgrid.AttrItalic != 0 {
		st = st.Italic(true)
	}
	if cell.Attr&vtgrid.AttrReverse != 0 {
		st = st.Reverse(true)
	}
	if cell.Attr&vtgrid.AttrStrike != 0 {
		st = st.Strikethrough(true)
	}
	if cell.Under != vtgrid.UnderlineNone {
		// lipgloss has one underline, so curly/dotted/dashed all render as a
		// plain one. The model keeps which it was; this surface cannot show it.
		st = st.Underline(true)
	}
	return st
}

// lastContentRow returns the highest row index that has any non-blank cell, or
// -1 if the whole grid is blank. Scans from the bottom up and stops at the
// first non-blank row, so it is cheap for the common full-screen case.
func lastContentRow(term *vtgrid.Terminal, cols, rows int) int {
	for y := rows - 1; y >= 0; y-- {
		for x := 0; x < cols; x++ {
			if c := term.CellAt(x, y); c.Rune != 0 && c.Rune != ' ' {
				return y
			}
		}
	}
	return -1
}

func cellPaneWidth(cell vtgrid.Cell) int {
	if cell.Width < 1 {
		return 1
	}
	return int(cell.Width)
}
