//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// collectScreen view-attaches via CollectRaw and feeds the captured byte burst
// through a headless screen model. It returns the built terminal plus the
// resolved grid size. Shared by SessionSnapshot (plain text) and
// SessionSnapshotStyled (text + style spans).
//
// The model is sized from the TerminalWindowSize the server replays ahead of
// the ring (the controlling client's PTY size); defRows/defCols are the
// fallback when the session reports no size, in which case a full-screen TUI
// may mis-render.
//
// It renders through vtgrid rather than a general-purpose emulator, for two
// reasons that are both measured rather than assumed (vtgrid/bench_test.go, and
// the parity suite over vtgrid/testdata/vtcorpus):
//
//   - Cost. This path re-renders the WHOLE replay ring on every call, and the
//     ring is a megabyte. Through x/vt a ring of scrolled shell output took
//     6.9 s of CPU per snapshot; the same bytes here take about 37 ms, because
//     scrolling rotates row headers instead of copying cells.
//   - No response channel. x/vt answers DA1/DA2/DSR by writing to its own
//     output side, so a caller that does not drain it blocks forever — this
//     function used to run a discard goroutine for exactly that. vtgrid never
//     emits a byte, so there is nothing to drain and nothing to close.
func (c *Client) collectScreen(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (*vtgrid.Terminal, string, int, int, error) {
	captured, rows, cols, ok, err := c.CollectRaw(ctx, taskIDHex, settle)
	if err != nil {
		return nil, "", 0, 0, err
	}
	if !ok || rows == 0 || cols == 0 {
		rows, cols = defRows, defCols
		fmt.Fprintf(os.Stderr,
			"harness-cli: session %s reported no terminal size; rendering at %dx%d (full-screen TUIs may mis-render)\n",
			taskIDHex, cols, rows)
	}

	term := vtgrid.New(int(cols), int(rows))
	_, _ = term.Write(captured)

	// The OSC 0/2 window title is NOT part of the cell grid, and for an agent it
	// is the single most informative byte on the wire: Claude Code re-emits it on
	// every spinner tick, which makes it the one continuously-reasserted signal
	// that a turn is running. vtgrid parses it out of the same pass that built
	// the grid.
	//
	// This used to be a separate byte scan, because x/vt's Title callback
	// truncates a title at its first multi-byte character. That was never a
	// quirk of the callback: x/vt honours the eight-bit ST (0x9C) inside an OSC,
	// and 0x9C is the second byte of U+2733 ✳ — the glyph Claude Code puts in
	// its title — so the sequence ends one byte in and the rest of the title
	// prints onto the grid. vtgrid does not accept 0x9C as a terminator in a
	// UTF-8 stream, which fixes the title and the stray text together.
	return term, term.Title(), int(cols), int(rows), nil
}

// renderScreen flattens the grid to text with trailing blanks removed from each
// row.
//
// vtgrid keeps them — its Lines() pads every row to the full width, on the
// grounds that whether they matter is the caller's business. Here they do not:
// a snapshot is read by people and by detection rules, neither of which has any
// use for 140 spaces after a prompt, and the --json form would carry them in
// every one of its lines. Trimming here rather than in the model keeps the
// model's grid addressable by column.
func renderScreen(term *vtgrid.Terminal) string {
	lines := term.Lines()
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// SessionSnapshot view-attaches to a detachable interactive session, feeds the
// replayed PTY byte stream through a headless screen model, and returns the
// current screen as plain text — a non-intrusive, terminal-free alternative to
// `session attach` for reading what a session currently shows.
//
// settle is how long to keep collecting bytes after attach before rendering;
// the replay arrives in a burst, so a short window (e.g. 1.5s) is enough for a
// static screen.
func (c *Client) SessionSnapshot(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (string, error) {
	term, _, _, _, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return taskIDHex, err
	}
	return renderScreen(term), nil
}

// SessionSnapshotRaw view-attaches to a detachable interactive session and
// returns the verbatim PTY replay burst — escape sequences intact — without
// rendering it. Unlike SessionSnapshot's flattened text, the result can be
// written straight to a real terminal to reproduce the screen exactly, or
// diffed byte-for-byte when the rendered text looks wrong.
func (c *Client) SessionSnapshotRaw(ctx context.Context, taskIDHex string, settle time.Duration) ([]byte, error) {
	captured, _, _, _, err := c.CollectRaw(ctx, taskIDHex, settle)
	if err != nil {
		return nil, err
	}
	return captured, nil
}

// SessionSnapshotStyled is SessionSnapshot plus a textual report of styled
// spans (faint/bold/italic/reverse/...) scanned from the cell grid. The plain
// render drops SGR attributes, so e.g. a faint placeholder/ghost line looks
// identical to real input; this side-channel surfaces the attribute the
// flattened text throws away — without re-emitting raw escapes (which an LLM
// reader can't use). Returns (plainText, styleReport).
func (c *Client) SessionSnapshotStyled(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration, withAttrs, withColor bool) (string, string, error) {
	term, _, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return taskIDHex, "", err
	}
	return renderScreen(term), formatSpans(collectSpans(term, cols, rows, withAttrs, withColor)), nil
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

// ScreenSnapshot is a session screen as data: the grid as one string per row,
// plus the styled spans indexed by those same row numbers.
//
// Attrs/Color report whether each style dimension was COLLECTED, not whether
// any was found — an empty Spans with Attrs=false means "not asked for", with
// Attrs=true it means "asked for, none present". Lines is always exactly Rows
// long so a span's Row indexes it directly.
type ScreenSnapshot struct {
	Task string `json:"task"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
	// Title is the last OSC 0/2 window title seen in the replayed burst, "" when
	// the session set none within the captured window. It is reported beside the
	// grid rather than folded into it because it is not ON the grid, and because
	// it is the input to the detection rules that read `region: "title"`.
	//
	// Empty is a measurement, not a gap: a long-idle session may have set its
	// title once, long enough ago that the ring dropped it.
	Title string `json:"title"`
	// Cursor is where the session's cursor is and whether it is shown. Beside
	// the grid rather than on it for the same reason as Title: it is terminal
	// state, not a cell.
	//
	// It is here because nothing else could answer for it. The grid says what
	// the screen READS; a reattach that lands the cursor in the middle of a
	// finished transcript, or shows one an app asked to hide, is invisible in
	// Lines and Spans, and those are the reattach defects this project keeps
	// finding. Reported unconditionally: collecting it is free (the model
	// already tracks it) and a field present only sometimes is a field a reader
	// cannot rely on.
	Cursor ScreenCursor `json:"cursor"`
	// AltScreen is true when the session is on the alternate buffer — a
	// full-screen app is live. The same grid means something different
	// depending on it: on the alternate buffer it is an app's canvas that
	// vanishes when the app exits, on the primary it is the tail of scrollback.
	AltScreen bool         `json:"alt_screen"`
	Attrs     bool         `json:"attrs"`
	Color     bool         `json:"color"`
	Lines     []string     `json:"lines"`
	Spans     []ScreenSpan `json:"spans"`
}

// ScreenCursor is the cursor as the session's own terminal model holds it:
// zero-based column and row into the grid, and whether it is being displayed.
//
// Visible is reported as its own bool rather than by moving an invisible cursor
// off-grid or to a sentinel position, because the two facts are independent —
// a hidden cursor still has a position, and an app that hides it while drawing
// still moves it. Collapsing them would throw one away.
type ScreenCursor struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	Visible bool `json:"visible"`
}

// Detect judges this screen with the rules for the named agent, so a caller
// that already holds a snapshot does not re-capture to ask what state it shows.
func (s *ScreenSnapshot) Detect(set DetectRuleSet) DetectExplain {
	return Detect(set, DetectInput{Lines: s.Lines, Title: s.Title})
}

// SessionSnapshotStructured is SessionSnapshotStyled's data form: same capture,
// same single span scan, returned as a struct instead of two strings. The CLI's
// --json path renders this; the text paths render projections of it.
//
// The grid stays width-WRAPPED here exactly as it is on screen — a long logical
// line is still split across rows. Use SessionSnapshotRaw or `session exec` when
// logical lines matter; this shape trades that for row/column addressability.
func (c *Client) SessionSnapshotStructured(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration, withAttrs, withColor bool) (*ScreenSnapshot, error) {
	term, title, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return nil, err
	}
	return buildSnapshot(term, taskIDHex, title, cols, rows, withAttrs, withColor), nil
}

// SessionSnapshotANSI is SessionSnapshotStructured plus the screen re-emitted
// with SGR escapes — the form a person writes to a terminal to see the screen
// as it looked, colours and all.
//
// It returns both from ONE capture. The structured form comes along because
// state detection reads it, and asking for a coloured picture is not a reason
// to give up the verdict that goes with it; capturing twice would be two
// different screens.
//
// Attribute and colour spans are always collected here: the ANSI render needs
// the same per-cell styling, so declining to collect it would save nothing.
func (c *Client) SessionSnapshotANSI(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (string, *ScreenSnapshot, error) {
	term, title, cols, rows, err := c.collectScreen(ctx, taskIDHex, defRows, defCols, settle)
	if err != nil {
		return "", nil, err
	}
	return term.ANSI(), buildSnapshot(term, taskIDHex, title, cols, rows, true, true), nil
}

// buildSnapshot is the single place a ScreenSnapshot is assembled, so the two
// capture entry points cannot drift into two shapes of the same thing.
func buildSnapshot(term *vtgrid.Terminal, taskIDHex, title string, cols, rows int, withAttrs, withColor bool) *ScreenSnapshot {
	cx, cy, cvis := term.Cursor()
	return &ScreenSnapshot{
		Task:      taskIDHex,
		Rows:      rows,
		Cols:      cols,
		Title:     title,
		Cursor:    ScreenCursor{X: cx, Y: cy, Visible: cvis},
		AltScreen: term.AltScreen(),
		Attrs:     withAttrs,
		Color:     withColor,
		Lines:     screenLines(renderScreen(term), rows),
		Spans:     collectSpans(term, cols, rows, withAttrs, withColor),
	}
}

// screenLines splits a rendered screen into exactly rows entries. The renderer
// hands back one line per row already; spans carry absolute grid rows, so the
// slice is padded/truncated defensively to keep Lines[span.Row] valid rather
// than leaving the caller to bounds-check.
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
const notableAttrs = vtgrid.AttrBold | vtgrid.AttrFaint | vtgrid.AttrItalic |
	vtgrid.AttrBlink | vtgrid.AttrReverse | vtgrid.AttrConceal | vtgrid.AttrStrike

// cellStyleLabel returns a label for the cell's notable styling, limited to the
// requested dimensions; "" = nothing notable (the cell is skipped). The label
// doubles as the run-merge key: adjacent cells with the same label coalesce into
// one span.
func cellStyleLabel(cell vtgrid.Cell, withAttrs, withColor bool) string {
	var parts []string
	if withAttrs {
		if a := cell.Attr & notableAttrs; a != 0 {
			parts = append(parts, attrNames(a))
		}
	}
	if withColor {
		if fg := cell.FG.Hex(); fg != "" {
			parts = append(parts, "fg"+fg)
		}
		if bg := cell.BG.Hex(); bg != "" {
			parts = append(parts, "bg"+bg)
		}
	}
	return strings.Join(parts, " ")
}

// cellWidth is the column span of a cell's glyph (2 for CJK/wide, 1 for
// normal). Advancing the scan cursor by this — rather than always +1 — skips the
// blank continuation cell that follows a wide char, so a CJK run isn't split
// into one span per character (the continuation cell carries no/other style and
// would otherwise break the run). A continuation cell reports width 0; guarding
// to 1 keeps the loop moving if the scan ever lands on one directly.
func cellWidth(cell vtgrid.Cell) int {
	if cell.Width < 1 {
		return 1
	}
	return int(cell.Width)
}

// collectSpans walks the grid and returns maximal horizontal runs that share
// the same non-empty style label. withAttrs includes faint/bold/etc; withColor
// includes fg/bg hex. Cells with nothing notable are skipped.
//
// This is the ONLY scan of the grid: both the human `--- styles ---` report
// (via formatSpans) and the --json output are rendered from its result, so the
// two cannot drift into two descriptions of the same screen. Always returns a
// non-nil slice — an empty one marshals as [] rather than null.
func collectSpans(term *vtgrid.Terminal, cols, rows int, withAttrs, withColor bool) []ScreenSpan {
	spans := []ScreenSpan{}
	for y := 0; y < rows; y++ {
		x := 0
		for x < cols {
			cell := term.CellAt(x, y)
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
				cur := term.CellAt(x, y)
				if cellStyleLabel(cur, withAttrs, withColor) != key {
					break
				}
				if cur.Rune != 0 {
					run.WriteRune(cur.Rune)
				}
				x += cellWidth(cur)
			}
			txt := strings.TrimRight(run.String(), " ")
			if txt == "" {
				continue
			}
			span := ScreenSpan{Row: y, Start: start, End: x - 1, Text: txt}
			if withAttrs {
				span.Attrs = attrList(head.Attr & notableAttrs)
			}
			if withColor {
				span.Fg = head.FG.Hex()
				span.Bg = head.BG.Hex()
			}
			spans = append(spans, span)
		}
	}
	return spans
}

// FormatScreenSpans is formatSpans for callers outside this package: the
// structured path hands back spans, and a caller printing the text form must
// render them through the SAME projection the text path uses rather than
// growing a second spelling of the report.
func FormatScreenSpans(spans []ScreenSpan) string { return formatSpans(spans) }

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
func attrNames(a vtgrid.Attr) string {
	return strings.Join(attrList(a), "+")
}

func attrList(a vtgrid.Attr) []string {
	var names []string
	if a&vtgrid.AttrBold != 0 {
		names = append(names, "bold")
	}
	if a&vtgrid.AttrFaint != 0 {
		names = append(names, "faint")
	}
	if a&vtgrid.AttrItalic != 0 {
		names = append(names, "italic")
	}
	if a&vtgrid.AttrBlink != 0 {
		names = append(names, "blink")
	}
	if a&vtgrid.AttrReverse != 0 {
		names = append(names, "reverse")
	}
	if a&vtgrid.AttrConceal != 0 {
		names = append(names, "conceal")
	}
	if a&vtgrid.AttrStrike != 0 {
		names = append(names, "strike")
	}
	return names
}
