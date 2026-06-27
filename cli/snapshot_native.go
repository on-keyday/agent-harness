//go:build !js

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// collectScreen view-attaches to a detachable interactive session, drains the
// replayed (and briefly-live) PTY byte burst for `settle`, and feeds it through
// a headless VT emulator. It returns the built emulator (the CALLER must Close
// it) plus the resolved grid size. Shared by SessionSnapshot (plain text) and
// SessionSnapshotStyled (text + style spans).
//
// It uses AttachMode_View, so it never takes over the controlling client (a
// live operator keeps typing undisturbed). The emulator is sized from the
// TerminalWindowSize the server replays ahead of the ring (the controlling
// client's PTY size); defRows/defCols are the fallback when the session reports
// no size (e.g. an older server), in which case a full-screen TUI may
// mis-render.
func (c *Client) collectScreen(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (*vt.Emulator, int, int, error) {
	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return nil, 0, 0, err
	}
	defer stream.Close()

	var mu sync.Mutex
	var data []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		out := stream.Stdout()
		for {
			n, rerr := out.Read(buf)
			if n > 0 {
				mu.Lock()
				data = append(data, buf[:n]...)
				full := len(data) > 8*1024*1024
				mu.Unlock()
				if full {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	select {
	case <-time.After(settle):
	case <-done:
	case <-ctx.Done():
	}

	mu.Lock()
	captured := append([]byte(nil), data...)
	mu.Unlock()

	rows, cols, ok := stream.LastWindowSize()
	if !ok || rows == 0 || cols == 0 {
		rows, cols = defRows, defCols
		fmt.Fprintf(os.Stderr,
			"harness-cli: session %s reported no terminal size; rendering at %dx%d (full-screen TUIs may mis-render)\n",
			taskIDHex, cols, rows)
	}

	emu := vt.NewEmulator(int(cols), int(rows))
	// Full-screen apps (claude, vim, …) emit terminal QUERIES early in their
	// output — DA1 (ESC[c), DA2 (ESC[>c), DSR (ESC[5n). x/vt answers these by
	// WRITING a response to its own output side (readable via emu.Read). If
	// nobody drains that, emu.Write blocks forever on the response and the
	// snapshot hangs. A headless render has no app to send the answers to, so
	// drain and discard them. (Bash never sends queries, which is why only
	// full-screen sessions hit this.) The drain goroutine exits on emu.Close.
	go io.Copy(io.Discard, emu)
	emu.Write(captured)
	return emu, int(cols), int(rows), nil
}

// SessionSnapshot view-attaches to a detachable interactive session, feeds the
// replayed PTY byte stream through a headless VT emulator, and returns the
// current screen as plain text — a non-intrusive, terminal-free alternative to
// `session attach` for reading what a session currently shows.
//
// settle is how long to keep collecting bytes after attach before rendering;
// the replay arrives in a burst, so a short window (e.g. 1.5s) is enough for a
// static screen.
func (c *Client) SessionSnapshot(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (string, error) {
	emu, _, _, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return taskIDHex, err
	}
	s := emu.String()
	_ = emu.Close() // unblocks the drain goroutine
	return s, nil
}

// SessionSnapshotStyled is SessionSnapshot plus a textual report of styled
// spans (faint/bold/italic/reverse/...) scanned from the VT cell grid. The
// plain render drops SGR attributes, so e.g. a faint placeholder/ghost line
// looks identical to real input; this side-channel surfaces the attribute the
// flattened text throws away — without re-emitting raw escapes (which an LLM
// reader can't use). Returns (plainText, styleReport).
func (c *Client) SessionSnapshotStyled(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (string, string, error) {
	emu, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return taskIDHex, "", err
	}
	text := emu.String()
	report := scanStyleSpans(emu, cols, rows)
	_ = emu.Close() // unblocks the drain goroutine
	return text, report, nil
}

// notableAttrs are the cell text attributes worth reporting; layout/color is
// intentionally omitted to keep the report lean and parseable.
const notableAttrs = uv.AttrBold | uv.AttrFaint | uv.AttrItalic | uv.AttrBlink |
	uv.AttrReverse | uv.AttrConceal | uv.AttrStrikethrough

// scanStyleSpans walks the VT grid and reports maximal horizontal runs that
// share the same non-empty attribute set, one per line:
//
//	r<row> c<start>-<end> <attrs>: "<text>"
//
// Cells with no attributes (the common case) are skipped, so a clean screen
// yields "(no styled spans)".
func scanStyleSpans(emu *vt.Emulator, cols, rows int) string {
	var b strings.Builder
	for y := 0; y < rows; y++ {
		x := 0
		for x < cols {
			cell := emu.CellAt(x, y)
			if cell == nil || cell.Style.Attrs&notableAttrs == 0 {
				x++
				continue
			}
			attrs := cell.Style.Attrs & notableAttrs
			start := x
			var run strings.Builder
			for x < cols {
				cur := emu.CellAt(x, y)
				if cur == nil || cur.Style.Attrs&notableAttrs != attrs {
					break
				}
				run.WriteString(cur.Content)
				x++
			}
			txt := strings.TrimRight(run.String(), " ")
			if txt == "" {
				continue
			}
			fmt.Fprintf(&b, "r%d c%d-%d %s: %q\n", y, start, x-1, attrNames(attrs), txt)
		}
	}
	if b.Len() == 0 {
		return "(no styled spans)"
	}
	return strings.TrimRight(b.String(), "\n")
}

func attrNames(a uint8) string {
	var names []string
	if a&uv.AttrBold != 0 {
		names = append(names, "bold")
	}
	if a&uv.AttrFaint != 0 {
		names = append(names, "faint")
	}
	if a&uv.AttrItalic != 0 {
		names = append(names, "italic")
	}
	if a&uv.AttrBlink != 0 {
		names = append(names, "blink")
	}
	if a&uv.AttrReverse != 0 {
		names = append(names, "reverse")
	}
	if a&uv.AttrConceal != 0 {
		names = append(names, "conceal")
	}
	if a&uv.AttrStrikethrough != 0 {
		names = append(names, "strike")
	}
	return strings.Join(names, "+")
}
