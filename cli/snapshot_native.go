//go:build !js

package cli

import (
	"context"
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// collectScreen view-attaches via CollectRaw and feeds the captured byte burst
// through a headless VT emulator. It returns the built emulator (the CALLER
// must Close it) plus the resolved grid size. Shared by SessionSnapshot (plain
// text) and SessionSnapshotStyled (text + style spans).
//
// The emulator is sized from the TerminalWindowSize the server replays ahead of
// the ring (the controlling client's PTY size); defRows/defCols are the
// fallback when the session reports no size, in which case a full-screen TUI
// may mis-render.
func (c *Client) collectScreen(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (*vt.Emulator, int, int, error) {
	captured, rows, cols, ok, err := c.CollectRaw(ctx, taskIDHex, settle)
	if err != nil {
		return nil, 0, 0, err
	}
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

// SessionSnapshotRaw view-attaches to a detachable interactive session and
// returns the verbatim PTY replay burst — escape sequences intact — without
// running it through the VT emulator. Unlike SessionSnapshot's flattened text,
// the result can be written straight to a real terminal to reproduce the screen
// exactly, or diffed byte-for-byte when the rendered text looks wrong.
func (c *Client) SessionSnapshotRaw(ctx context.Context, taskIDHex string, settle time.Duration) ([]byte, error) {
	captured, _, _, _, err := c.CollectRaw(ctx, taskIDHex, settle)
	if err != nil {
		return nil, err
	}
	return captured, nil
}

// SessionSnapshotStyled is SessionSnapshot plus a textual report of styled
// spans (faint/bold/italic/reverse/...) scanned from the VT cell grid. The
// plain render drops SGR attributes, so e.g. a faint placeholder/ghost line
// looks identical to real input; this side-channel surfaces the attribute the
// flattened text throws away — without re-emitting raw escapes (which an LLM
// reader can't use). Returns (plainText, styleReport).
func (c *Client) SessionSnapshotStyled(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration, withAttrs, withColor bool) (string, string, error) {
	emu, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return taskIDHex, "", err
	}
	text := emu.String()
	report := formatSpans(collectSpans(emu, cols, rows, withAttrs, withColor))
	_ = emu.Close() // unblocks the drain goroutine
	return text, report, nil
}

// ScreenSpan is one maximal horizontal run of grid cells sharing the same
// styling — the structured form of a `--- styles ---` report line. Attrs is nil
// when attributes were not collected AND when the run carries none; Fg/Bg are ""
// for the terminal default AND when colors were not collected. Neither absence
// is self-describing, which is why the collection flags travel on the enclosing
// ScreenSnapshot rather than being inferred per span.
type ScreenSpan struct {
	Row   int      `json:"row"`
	Start int      `json:"start"`
	End   int      `json:"end"`
	Attrs []string `json:"attrs,omitempty"`
	Fg    string   `json:"fg,omitempty"`
	Bg    string   `json:"bg,omitempty"`
	Text  string   `json:"text"`
}

// label rebuilds the run-merge key this span was collected under. It is what
// makes the human report a projection of the structured form rather than a
// second scan: formatSpans renders through it, so the two can never disagree
// about what a span says.
func (s ScreenSpan) label() string {
	var parts []string
	if len(s.Attrs) > 0 {
		parts = append(parts, strings.Join(s.Attrs, "+"))
	}
	if s.Fg != "" {
		parts = append(parts, "fg"+s.Fg)
	}
	if s.Bg != "" {
		parts = append(parts, "bg"+s.Bg)
	}
	return strings.Join(parts, " ")
}

// ScreenSnapshot is a session screen as data: the VT grid as one string per
// row, plus the styled spans indexed by those same row numbers.
//
// Attrs/Color report whether each style dimension was COLLECTED, not whether
// any was found — an empty Spans with Attrs=false means "not asked for", with
// Attrs=true it means "asked for, none present". Lines is always exactly Rows
// long so a span's Row indexes it directly.
type ScreenSnapshot struct {
	Task  string       `json:"task"`
	Rows  int          `json:"rows"`
	Cols  int          `json:"cols"`
	Attrs bool         `json:"attrs"`
	Color bool         `json:"color"`
	Lines []string     `json:"lines"`
	Spans []ScreenSpan `json:"spans"`
}

// SessionSnapshotStructured is SessionSnapshotStyled's data form: same capture,
// same single span scan, returned as a struct instead of two strings. The CLI's
// --json path renders this; the text paths render projections of it.
//
// The grid stays width-WRAPPED here exactly as it is on screen — a long logical
// line is still split across rows. Use SessionSnapshotRaw or `session exec` when
// logical lines matter; this shape trades that for row/column addressability.
func (c *Client) SessionSnapshotStructured(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration, withAttrs, withColor bool) (*ScreenSnapshot, error) {
	emu, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return nil, err
	}
	snap := &ScreenSnapshot{
		Task:  taskIDHex,
		Rows:  rows,
		Cols:  cols,
		Attrs: withAttrs,
		Color: withColor,
		Lines: screenLines(emu.String(), rows),
		Spans: collectSpans(emu, cols, rows, withAttrs, withColor),
	}
	_ = emu.Close() // unblocks the drain goroutine
	return snap, nil
}

// screenLines splits a rendered screen into exactly rows entries. The emulator
// may hand back fewer (trailing blank rows collapsed) or, defensively, more;
// spans carry absolute grid rows, so the slice is padded/truncated to keep
// Lines[span.Row] valid rather than leaving the caller to bounds-check.
func screenLines(text string, rows int) []string {
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return lines[:rows]
}

// notableAttrs are the cell text attributes worth reporting; layout/color is
// intentionally omitted to keep the report lean and parseable.
const notableAttrs = uv.AttrBold | uv.AttrFaint | uv.AttrItalic | uv.AttrBlink |
	uv.AttrReverse | uv.AttrConceal | uv.AttrStrikethrough

// cellStyleLabel returns a label for the cell's notable styling, limited to the
// requested dimensions; "" = nothing notable (the cell is skipped). The label
// doubles as the run-merge key: adjacent cells with the same label coalesce into
// one span.
func cellStyleLabel(cell *uv.Cell, withAttrs, withColor bool) string {
	if cell == nil {
		return ""
	}
	var parts []string
	if withAttrs {
		if a := cell.Style.Attrs & notableAttrs; a != 0 {
			parts = append(parts, attrNames(a))
		}
	}
	if withColor {
		if fg := colorHex(cell.Style.Fg); fg != "" {
			parts = append(parts, "fg"+fg)
		}
		if bg := colorHex(cell.Style.Bg); bg != "" {
			parts = append(parts, "bg"+bg)
		}
	}
	return strings.Join(parts, " ")
}

// colorHex renders a cell color as #rrggbb, or "" for the terminal default
// (nil Fg/Bg). Every color.Color answers RGBA(), so this handles 16-color,
// 256-color, and truecolor cells uniformly — color is cheap to render; the
// reason it is opt-in (--color) is volume: most cells carry a color, so the
// report balloons, unlike the rare faint/bold attribute spans.
func colorHex(c color.Color) string {
	if c == nil {
		return ""
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// cellWidth is the column span of a cell's grapheme (2 for CJK/wide, 1 for
// normal). Advancing the scan cursor by this — rather than always +1 — skips the
// blank continuation cell that follows a wide char, so a CJK run isn't split
// into one span per character (the continuation cell carries no/other style and
// would otherwise break the run). Guards 0/negative to never stall the loop.
func cellWidth(cell *uv.Cell) int {
	if cell == nil || cell.Width < 1 {
		return 1
	}
	return cell.Width
}

// collectSpans walks the VT grid and returns maximal horizontal runs that share
// the same non-empty style label. withAttrs includes faint/bold/etc; withColor
// includes fg/bg hex. Cells with nothing notable are skipped.
//
// This is the ONLY scan of the grid: both the human `--- styles ---` report
// (via formatSpans) and the --json output are rendered from its result, so the
// two cannot drift into two descriptions of the same screen. Always returns a
// non-nil slice — an empty one marshals as [] rather than null.
func collectSpans(emu *vt.Emulator, cols, rows int, withAttrs, withColor bool) []ScreenSpan {
	spans := []ScreenSpan{}
	for y := 0; y < rows; y++ {
		x := 0
		for x < cols {
			cell := emu.CellAt(x, y)
			key := cellStyleLabel(cell, withAttrs, withColor)
			if key == "" {
				x += cellWidth(cell)
				continue
			}
			// Every cell in the run shares the label by construction, so the
			// run's first cell is representative for the structured fields.
			head := cell
			start := x
			var run strings.Builder
			for x < cols {
				cur := emu.CellAt(x, y)
				if cellStyleLabel(cur, withAttrs, withColor) != key {
					break
				}
				run.WriteString(cur.Content)
				x += cellWidth(cur)
			}
			txt := strings.TrimRight(run.String(), " ")
			if txt == "" {
				continue
			}
			span := ScreenSpan{Row: y, Start: start, End: x - 1, Text: txt}
			if withAttrs {
				span.Attrs = attrList(head.Style.Attrs & notableAttrs)
			}
			if withColor {
				span.Fg = colorHex(head.Style.Fg)
				span.Bg = colorHex(head.Style.Bg)
			}
			spans = append(spans, span)
		}
	}
	return spans
}

// formatSpans renders collected spans as the human report, one per line:
//
//	r<row> c<start>-<end> <label>: "<text>"
//
// A clean screen yields "(no styled spans)".
func formatSpans(spans []ScreenSpan) string {
	if len(spans) == 0 {
		return "(no styled spans)"
	}
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "r%d c%d-%d %s: %q\n", s.Row, s.Start, s.End, s.label(), s.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// attrNames is attrList's report form: the same names joined with "+", which is
// how a style label spells a multi-attribute run ("bold+faint").
func attrNames(a uint8) string {
	return strings.Join(attrList(a), "+")
}

func attrList(a uint8) []string {
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
	return names
}
