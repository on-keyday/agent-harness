package tui

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// PaneStreamer view-attaches one session read-only and renders its live screen
// into a headless VT emulator, croppable to a pane. It NEVER sends a size — the
// grid has no size authority (Global Constraint) — it sizes its emulator to the
// size the server replays and resizes on mid-stream winsize frames.
type PaneStreamer struct {
	taskID string

	mu     sync.Mutex
	emu    *vt.Emulator
	cols   int
	rows   int
	err    error
	stream  *agentexec.CommandExecutionStream
	cancel  context.CancelFunc
	stopped bool // Stop() ran; a still-attaching pump must close its stream
}

func NewPaneStreamer(taskID string, defRows, defCols int) *PaneStreamer {
	if defRows <= 0 {
		defRows = 24
	}
	if defCols <= 0 {
		defCols = 80
	}
	emu := vt.NewEmulator(defCols, defRows)
	return &PaneStreamer{taskID: taskID, emu: emu, cols: defCols, rows: defRows}
}

func (p *PaneStreamer) TaskID() string { return p.taskID }

func (p *PaneStreamer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *PaneStreamer) Start(ctx context.Context, c *cli.Client) {
	cctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = cancel
	emu := p.emu
	p.mu.Unlock()

	// x/vt answers DA1/DA2/DSR queries by writing to its own output side; drain
	// and discard or emu.Write blocks forever (cli/snapshot_native.go pattern).
	// This goroutine exits when emu.Close() (in Stop) makes emu.Read return an
	// error — same lifecycle as the sibling in cli/snapshot_native.go, no
	// separate stop channel to manage. Recover so a VT-layer panic can never
	// take down the whole TUI.
	go func() {
		defer func() { _ = recover() }()
		_, _ = io.Copy(io.Discard, emu)
	}()

	go p.pump(cctx, c)
}

func (p *PaneStreamer) pump(ctx context.Context, c *cli.Client) {
	// Cowrite (not View): observes output exactly like a viewer AND can forward
	// input without taking over the controlling client or claiming size
	// authority — this is what lets the grid type into a focused pane
	// (SendInput) while it stays small. Idle panes send nothing, so cowrite is
	// output-equivalent to view until the user enters input mode.
	// gridReplayLimit caps the scrollback the server replays: a pane shows only
	// a small bottom crop, so it does NOT need the full ~1 MiB ring. Without
	// this, opening a grid of N sessions pulls ~1 MiB × N of replay, which is
	// slow enough over a real link that panes torn down on a repeated
	// open/close never finish their replay and render black. The mode preamble
	// (alt-screen, DEC modes) is sent separately, so a full-screen app repaints
	// correctly from a capped replay.
	const gridReplayLimit = 128 * 1024
	stream, _, err := c.AttachSessionWithReplayLimit(ctx, p.taskID, protocol.AttachMode_Cowrite, gridReplayLimit)
	if err != nil {
		p.setErr(err)
		return
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
	p.mu.Unlock()

	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	lastRows, lastCols := 0, 0
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			resize := ok && (int(rows) != lastRows || int(cols) != lastCols) && rows > 0 && cols > 0
			if !p.feed(buf[:n], int(rows), int(cols), resize) {
				return // emulator torn down (Stop raced)
			}
			if resize {
				lastRows, lastCols = int(rows), int(cols)
			}
		}
		if rerr != nil {
			p.setErr(rerr)
			return
		}
	}
}

// feed applies one chunk of PTY bytes to the emulator under the lock, resizing
// first when the session's window size changed. It RECOVERS from panics inside
// the VT emulator: x/vt can index out of range on some escape sequences — e.g. a
// reverseIndex (ESC M) against a scroll region left stale by a resize panics in
// ScrollDown/InsertLineArea (index N into a shorter buffer). A read-only
// monitoring pane must never crash the whole TUI, so a bad sequence is swallowed
// and the pump keeps going; the session's next full repaint recovers the view.
// Returns false only if the emulator was already torn down (Stop raced).
func (p *PaneStreamer) feed(data []byte, rows, cols int, resize bool) (alive bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emu == nil {
		return false
	}
	alive = true
	// recover is registered AFTER the Unlock defer, so on panic it runs first
	// (swallowing), then the lock is released; alive stays true so the pump
	// continues rather than treating a recovered panic as teardown.
	defer func() { _ = recover() }()
	if resize {
		p.emu.Resize(cols, rows)
		// Reset the scroll region (DECSTBM) to the new full screen. x/vt does
		// not clamp a stale region on resize, which later panics in
		// ScrollDown/reverseIndex when the old bottom margin exceeds the new
		// height. The session re-establishes its own region on its next redraw.
		p.emu.Write([]byte("\x1b[r"))
		p.cols, p.rows = cols, rows
	}
	p.emu.Write(data)
	return alive
}

func (p *PaneStreamer) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
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
	emu := p.emu
	p.cancel = nil
	p.stream = nil
	p.emu = nil
	p.stopped = true // a pump still inside AttachSession will close its stream itself
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.Close()
	}
	if emu != nil {
		_ = emu.Close()
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
	if c := emu.CursorPosition().Y + 1; c > bottom {
		bottom = c
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
			if cell == nil || cell.Content == "" {
				run.WriteByte(' ')
				painted++
				x++
				continue
			}
			run.WriteString(cell.Content)
			painted += w
			x += w
		}
		flush()
	}
	return b.String()
}

// cellStyleKey is a cheap comparable identity for a cell's style, used to
// coalesce equal-styled runs. "" is the default (unstyled) cell.
func cellStyleKey(cell *uv.Cell) string {
	if cell == nil {
		return ""
	}
	s := cell.Style
	fg, bg := paneColorHex(s.Fg), paneColorHex(s.Bg)
	if fg == "" && bg == "" && s.Attrs == 0 && s.Underline == uv.UnderlineNone {
		return "" // unstyled: coalesce with default runs
	}
	return fmt.Sprintf("%s|%s|%d|%d", fg, bg, s.Attrs, s.Underline)
}

// cellLipgloss builds the lipgloss style for a cell (fg/bg + notable attrs), so
// a run rendered through it reproduces the session's colors and emphasis.
func cellLipgloss(cell *uv.Cell) lipgloss.Style {
	st := lipgloss.NewStyle()
	if cell == nil {
		return st
	}
	s := cell.Style
	if h := paneColorHex(s.Fg); h != "" {
		st = st.Foreground(lipgloss.Color(h))
	}
	if h := paneColorHex(s.Bg); h != "" {
		st = st.Background(lipgloss.Color(h))
	}
	a := s.Attrs
	if a&uv.AttrBold != 0 {
		st = st.Bold(true)
	}
	if a&uv.AttrFaint != 0 {
		st = st.Faint(true)
	}
	if a&uv.AttrItalic != 0 {
		st = st.Italic(true)
	}
	if a&uv.AttrReverse != 0 {
		st = st.Reverse(true)
	}
	if a&uv.AttrStrikethrough != 0 {
		st = st.Strikethrough(true)
	}
	if s.Underline != uv.UnderlineNone {
		st = st.Underline(true)
	}
	return st
}

// paneColorHex renders a cell color as #rrggbb, or "" for the terminal default
// (nil). Every color.Color answers RGBA(), so 16/256/truecolor are uniform;
// lipgloss downsamples #rrggbb to the terminal's real profile at render time.
func paneColorHex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, bl, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(bl>>8))
}

// lastContentRow returns the highest row index that has any non-blank cell, or
// -1 if the whole grid is blank. Scans from the bottom up and stops at the
// first non-blank row, so it is cheap for the common full-screen case.
func lastContentRow(emu *vt.Emulator, cols, rows int) int {
	for y := rows - 1; y >= 0; y-- {
		for x := 0; x < cols; x++ {
			c := emu.CellAt(x, y)
			if c != nil && c.Content != "" && c.Content != " " {
				return y
			}
		}
	}
	return -1
}

func cellPaneWidth(cell *uv.Cell) int {
	if cell == nil || cell.Width < 1 {
		return 1
	}
	return cell.Width
}
