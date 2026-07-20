package tui

import (
	"context"
	"io"
	"strings"
	"sync"

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
	stream *agentexec.CommandExecutionStream
	cancel context.CancelFunc
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
	// separate stop channel to manage.
	go io.Copy(io.Discard, emu)

	go p.pump(cctx, c)
}

func (p *PaneStreamer) pump(ctx context.Context, c *cli.Client) {
	stream, _, err := c.AttachSession(ctx, p.taskID, protocol.AttachMode_View)
	if err != nil {
		p.setErr(err)
		return
	}
	p.mu.Lock()
	p.stream = stream
	p.mu.Unlock()

	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	lastRows, lastCols := 0, 0
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			p.mu.Lock()
			if p.emu == nil {
				// Stop() already nil'd the emulator (racing against this
				// still-in-flight read); nothing left to write into.
				p.mu.Unlock()
				return
			}
			if ok && (int(rows) != lastRows || int(cols) != lastCols) && rows > 0 && cols > 0 {
				p.emu.Resize(int(cols), int(rows))
				p.cols, p.rows = int(cols), int(rows)
				lastRows, lastCols = int(rows), int(cols)
			}
			p.emu.Write(buf[:n])
			p.mu.Unlock()
		}
		if rerr != nil {
			p.setErr(rerr)
			return
		}
	}
}

func (p *PaneStreamer) setErr(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
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
// plain text. Activity in shells and full-screen agents concentrates at the
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
				for painted < width {
					b.WriteByte(' ')
					painted++
				}
				break
			}
			if cell == nil || cell.Content == "" {
				b.WriteByte(' ')
				painted++
				x++
				continue
			}
			b.WriteString(cell.Content)
			painted += w
			x += w
		}
	}
	return b.String()
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
