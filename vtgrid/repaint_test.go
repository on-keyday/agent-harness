package vtgrid_test

// Can a server holding a vtgrid screen hand a client the SAME screen without
// replaying the bytes that produced it?
//
// The harness answers reattach today by replaying a byte ring, which carries no
// terminal state: a full-screen app that was torn off mid-episode comes back as
// debris, and the mode preamble deliberately refuses to replay the alt-screen
// modes because it has no content to put behind them (server/mode_tracker.go
// says so in as many words). A repaint sequence built from the grid is the
// alternative. This file is the gate on whether it actually reconstructs.
//
// External test package on purpose: the repaint would live in server/, so it
// may only use vtgrid's EXPORTED API. Being unable to reach an unexported field
// is the point, not an inconvenience.
//
// Measured against three emulators — vtgrid itself, x/vt as an independent Go
// implementation, and (out of process, see TestRepaintExportForBrowser) the
// xterm.js the WebUI ships.

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
	"github.com/on-keyday/agent-harness/vtgrid"
)

// extCorpus mirrors vtCorpus for the external test package, which cannot see
// the in-package table. TestRepaintCorpusListIsComplete keeps the copy honest.
type extCorpus struct {
	Name       string
	Rows, Cols int
}

var extCorpora = []extCorpus{
	{"agy-start", 40, 150}, {"agy-tui", 40, 150}, {"altscreen", 40, 150},
	{"bash-scroll", 40, 150}, {"claude-start", 40, 150}, {"claude-tui", 40, 150},
	{"codex-start", 40, 150}, {"codex-tui", 40, 150}, {"conpty-ssh", 36, 173},
	{"herdr-mouse", 36, 173}, {"herdr-tui", 36, 173}, {"htop", 40, 150},
	{"opencode-tui", 40, 150}, {"pwsh", 40, 150}, {"torture", 40, 150},
	{"win-start", 40, 150}, {"win-cmd", 40, 150}, {"vim-split", 40, 150},
}

// TestRepaintCorpusListIsComplete fails when a capture is added to testdata but
// not to extCorpora. Without it the duplicated table goes stale silently and the
// new corpus — the one somebody added because it broke something — is the one
// case the repaint gate never runs.
func TestRepaintCorpusListIsComplete(t *testing.T) {
	ents, err := os.ReadDir(filepath.Join("testdata", "vtcorpus"))
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	var onDisk []string
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".raw.gz") {
			onDisk = append(onDisk, strings.TrimSuffix(n, ".raw.gz"))
		}
	}
	listed := make(map[string]bool, len(extCorpora))
	for _, c := range extCorpora {
		listed[c.Name] = true
	}
	var missing []string
	for _, n := range onDisk {
		if !listed[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("corpora on disk but not in extCorpora: %v\n"+
			"add them (with the size from vtcorpus_test.go's vtCorpora) so the "+
			"repaint gate covers them too", missing)
	}
}

func loadExtCorpus(tb testing.TB, name string) []byte {
	tb.Helper()
	f, err := os.Open(filepath.Join("testdata", "vtcorpus", name+".raw.gz"))
	if err != nil {
		tb.Fatalf("open %s: %v", name, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	zr, err := gzip.NewReader(f)
	if err != nil {
		tb.Fatalf("gunzip %s: %v", name, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}
	return b
}

// buildRepaint synthesises the bytes a server would append to an attach replay
// so the observer's screen becomes t's screen. Every ordering decision in here
// was made by a measurement, not by taste:
//
//   - The screen selection comes FIRST. Switching buffers is entitled to reset
//     the margins, so resetting them before the switch would be undone by it.
//
//   - Autowrap is off for the duration. Writing the last cell of the last row
//     with it on can scroll the screen out from under the paint.
//
//   - Each row is ERASED BEFORE it is painted, not after. Painting a row that
//     fills the width leaves the cursor ON the last column, so a trailing EL
//     erases the cell just written. That costs nothing on short lines and
//     everything on a TUI: with the erase trailing, htop scored 25/40 rows and
//     vim-split 21/40, while shell output stayed at 40/40.
//
//   - Rows are addressed absolutely rather than joined with ANSI()'s CRLF,
//     which sidesteps the deferred-wrap a full-width row leaves pending.
//
//   - DECTCEM (cursor visibility) is emitted BEFORE the screen selection, while
//     the cursor's POSITION is restored at the very end. Splitting the two
//     halves of "put the cursor back" looks arbitrary and is not: on a real
//     Windows console, DECTCEM issued while the alternate buffer is active does
//     not take effect on it and lands on the MAIN buffer instead — so the
//     hidden cursor stays visible AND the buffer underneath is corrupted.
//     Measured on conhost in both directions; issued before the switch it
//     survives into the alt buffer, which is why it sits here. The three
//     emulators that do honour it either way are indifferent to the move.
//
// Not carried here, on purpose: the input-affecting private modes (bracketed
// paste, mouse, application cursor keys). vtgrid neither tracks nor exposes
// them; server/mode_tracker.go's preamble does, and the two are complementary.
// The preamble must be emitted AFTER this sequence — it restores DECOM/DECAWM,
// which this one deliberately clears in order to address cells absolutely.
func buildRepaint(t *vtgrid.Terminal) []byte {
	_, rows := t.Size()
	x, y, vis := t.Cursor()
	var b strings.Builder
	// DECTCEM before the screen selection, and NOT beside the final cursor
	// positioning where it would read more naturally. On a real Windows
	// console the two halves of "restore the cursor" belong on opposite sides
	// of the switch; see the Windows note above.
	if vis {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	if t.AltScreen() {
		b.WriteString("\x1b[?1049h")
	} else {
		b.WriteString("\x1b[?1049l")
	}
	b.WriteString("\x1b[r")   // DECSTBM: full screen
	b.WriteString("\x1b[?6l") // DECOM off
	b.WriteString("\x1b[?7l") // DECAWM off
	body := strings.Split(t.ANSI(), "\r\n")
	for y := 0; y < rows; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0m\x1b[K", y+1)
		if y < len(body) {
			b.WriteString(body[y])
		}
	}
	b.WriteString("\x1b[0m")
	b.WriteString("\x1b[?7h")
	fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
	return []byte(b.String())
}

// poison is the state a torn-off full-screen app leaves an observer in: alt
// buffer, a scroll region, origin mode, hidden cursor, no autowrap, a live SGR
// pen, and debris on the grid. It is the reattach symptom in a string literal.
const poison = "\x1b[?1049h\x1b[5;20r\x1b[?6h\x1b[?25l\x1b[?7l\x1b[7;31;44m" +
	"leftovers from a full-screen app\r\nsecond line of debris"

// replayTailBytes is a ring snapshot's worth of history, cut at an arbitrary
// offset — no ground-state scan, which is harsher than what the server does.
const replayTailBytes = 32 << 10

func ringTail(data []byte, n int) []byte {
	if len(data) <= n {
		return data
	}
	return data[len(data)-n:]
}

func oracleRows(data []byte, cols, rows int) []string {
	emu := vt.NewEmulator(cols, rows)
	emu.Scrollback().SetMaxLines(1) // the smallest x/vt accepts; 0 is a no-op
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
	_, _ = emu.Write(data)
	out := strings.Split(strings.TrimSuffix(emu.String(), "\n"), "\n")
	for len(out) < rows {
		out = append(out, "")
	}
	_ = emu.Close()
	<-done
	return trimRows(out[:rows])
}

// trimRows drops trailing ASCII spaces only. NOT unicode whitespace: Claude
// Code puts U+00A0 after its `❯` prompt glyph, and a trimmer that eats it
// deletes real screen content while reporting a match.
func trimRows(in []string) []string {
	out := make([]string, len(in))
	for i, r := range in {
		out[i] = strings.TrimRight(r, " ")
	}
	return out
}

func countMatch(want, got []string) int {
	n := 0
	for i := range want {
		if i < len(got) && want[i] == got[i] {
			n++
		}
	}
	return n
}

// describeCell renders everything a cell carries, through the EXPORTED API
// only. Text() and Link() resolve the interned side tables, whose numeric
// indexes are meaningful only relative to their own terminal.
func describeCell(t *vtgrid.Terminal, c vtgrid.Cell) string {
	l, ok := t.Link(c)
	return fmt.Sprintf("q=%q w=%d a=%d u=%d fg=%+v bg=%+v ufg=%+v link=%v/%q/%q",
		t.Text(c), c.Width, c.Attr, c.Under, c.FG, c.BG, c.UnderFG, ok, l.URL, l.Params)
}

// countCellMatch counts CELLS, not rows: a row-text comparison passes while
// every colour on the screen is lost.
func countCellMatch(a, b *vtgrid.Terminal) (match, total int) {
	cols, rows := a.Size()
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			total++
			if describeCell(a, a.CellAt(x, y)) == describeCell(b, b.CellAt(x, y)) {
				match++
			}
		}
	}
	return match, total
}

// TestRepaintReconstructsScreen asserts that the repaint lands the server's
// screen on an observer regardless of the state that observer was in.
//
// The ring-only column in the log is the CURRENT behaviour for the same input,
// and it is the reason this exists: htop 5/40 rows, vim-split 2/40.
func TestRepaintReconstructsScreen(t *testing.T) {
	var totRows, totCells int
	var okFresh, okDirty, okOracle, okPoison, okPoisonOrc int
	var okCellFresh, okCellDirty, okCellPoison int

	for _, c := range extCorpora {
		data := loadExtCorpus(t, c.Name)

		server := vtgrid.New(c.Cols, c.Rows)
		_, _ = server.Write(data)
		want := trimRows(server.Lines())
		rp := buildRepaint(server)

		// An observer starting from nothing.
		fresh := vtgrid.New(c.Cols, c.Rows)
		_, _ = fresh.Write(rp)

		// An observer that consumed a truncated ring first.
		tail := ringTail(data, replayTailBytes)
		dirty := vtgrid.New(c.Cols, c.Rows)
		_, _ = dirty.Write(tail)
		ringOnly := countMatch(want, trimRows(dirty.Lines()))
		_, _ = dirty.Write(rp)

		// The same, through an independent emulator.
		orc := oracleRows(append(append([]byte{}, tail...), rp...), c.Cols, c.Rows)

		// An observer left in the reattach symptom.
		pois := vtgrid.New(c.Cols, c.Rows)
		_, _ = pois.Write([]byte(poison))
		_, _ = pois.Write(rp)
		poisOrc := oracleRows(append([]byte(poison), rp...), c.Cols, c.Rows)

		mf := countMatch(want, trimRows(fresh.Lines()))
		md := countMatch(want, trimRows(dirty.Lines()))
		mo := countMatch(want, orc)
		mp := countMatch(want, trimRows(pois.Lines()))
		mpo := countMatch(want, poisOrc)
		cmF, cTot := countCellMatch(server, fresh)
		cmD, _ := countCellMatch(server, dirty)
		cmP, _ := countCellMatch(server, pois)

		for _, got := range []struct {
			what string
			n    int
		}{
			{"fresh", mf}, {"after-ring", md}, {"after-ring/x-vt", mo},
			{"poisoned", mp}, {"poisoned/x-vt", mpo},
		} {
			if got.n != c.Rows {
				t.Errorf("%s: %s rows %d/%d after repaint", c.Name, got.what, got.n, c.Rows)
			}
		}
		for _, got := range []struct {
			what string
			n    int
		}{{"fresh", cmF}, {"after-ring", cmD}, {"poisoned", cmP}} {
			if got.n != cTot {
				t.Errorf("%s: %s cells %d/%d after repaint", c.Name, got.what, got.n, cTot)
			}
		}
		cx, cy, cv := server.Cursor()
		for _, o := range []struct {
			what string
			term *vtgrid.Terminal
		}{{"after-ring", dirty}, {"poisoned", pois}} {
			gx, gy, gv := o.term.Cursor()
			if gx != cx || gy != cy || gv != cv {
				t.Errorf("%s: %s cursor (%d,%d,%v), want (%d,%d,%v)",
					c.Name, o.what, gx, gy, gv, cx, cy, cv)
			}
			if o.term.AltScreen() != server.AltScreen() {
				t.Errorf("%s: %s alt-screen %v, want %v",
					c.Name, o.what, o.term.AltScreen(), server.AltScreen())
			}
		}

		totRows += c.Rows
		totCells += cTot
		okFresh += mf
		okDirty += md
		okOracle += mo
		okPoison += mp
		okPoisonOrc += mpo
		okCellFresh += cmF
		okCellDirty += cmD
		okCellPoison += cmP

		t.Logf("%-14s rows=%d ring-only=%2d | fresh=%2d after-ring=%2d x-vt=%2d poisoned=%2d poisoned/x-vt=%2d  repaint=%dB",
			c.Name, c.Rows, ringOnly, mf, md, mo, mp, mpo, len(rp))
	}

	pct := func(n, d int) float64 { return 100 * float64(n) / float64(d) }
	t.Logf("ROWS  total=%d fresh=%d (%.1f%%) after-ring=%d (%.1f%%) x-vt=%d (%.1f%%) poisoned=%d (%.1f%%) poisoned/x-vt=%d (%.1f%%)",
		totRows, okFresh, pct(okFresh, totRows), okDirty, pct(okDirty, totRows),
		okOracle, pct(okOracle, totRows), okPoison, pct(okPoison, totRows),
		okPoisonOrc, pct(okPoisonOrc, totRows))
	t.Logf("CELLS total=%d fresh=%d (%.3f%%) after-ring=%d (%.3f%%) poisoned=%d (%.3f%%)",
		totCells, okCellFresh, pct(okCellFresh, totCells),
		okCellDirty, pct(okCellDirty, totCells), okCellPoison, pct(okCellPoison, totCells))
}

// TestRepaintExportForBrowser writes the same inputs and expected screens to a
// directory so the check can be re-run against xterm.js — the emulator the
// WebUI actually ships, and the only one of the three that is not Go. Skipped
// unless REPAINT_EXPORT_DIR is set, because it exists to feed a browser, not to
// gate a build.
//
// Known result for the browser leg (xterm.js as vendored in webui/static):
// 708/708 rows and 18/18 cursor+alt-screen, both after a ring replay and from
// the poisoned state. Per-cell bold counts differ on the two codex corpora
// because xterm.js gives U+2728 ✨ a width of 1 where vtgrid gives it 2 — a
// pre-existing disagreement, unchanged by the repaint: feeding xterm.js the
// full raw stream puts that row's closing border in the same column the
// repaint does.
func TestRepaintExportForBrowser(t *testing.T) {
	dir := os.Getenv("REPAINT_EXPORT_DIR")
	if dir == "" {
		t.Skip("set REPAINT_EXPORT_DIR to export")
	}
	type entry struct {
		Name         string   `json:"name"`
		Cols         int      `json:"cols"`
		Rows         int      `json:"rows"`
		Tail         string   `json:"tail"`
		Repaint      string   `json:"repaint"`
		Poison       string   `json:"poison"`
		Want         []string `json:"want"`
		CurX         int      `json:"cur_x"`
		CurY         int      `json:"cur_y"`
		CurVis       bool     `json:"cur_vis"`
		Alt          bool     `json:"alt"`
		RingOnly     int      `json:"ring_only_match"`
		NonDefaultBG int      `json:"nd_bg"`
		Bold         int      `json:"bold"`
		BoldXY       []int    `json:"bold_xy"`
	}
	var out []entry
	for _, c := range extCorpora {
		data := loadExtCorpus(t, c.Name)
		server := vtgrid.New(c.Cols, c.Rows)
		_, _ = server.Write(data)
		tail := ringTail(data, replayTailBytes)
		ringOnly := vtgrid.New(c.Cols, c.Rows)
		_, _ = ringOnly.Write(tail)

		ndbg, bold := 0, 0
		var boldXY []int
		for y := 0; y < c.Rows; y++ {
			for x := 0; x < c.Cols; x++ {
				cell := server.CellAt(x, y)
				if !cell.BG.IsDefault() {
					ndbg++
				}
				if cell.Attr&vtgrid.AttrBold != 0 {
					bold++
					boldXY = append(boldXY, y*1000+x)
				}
			}
		}
		cx, cy, cv := server.Cursor()
		out = append(out, entry{
			Name: c.Name, Cols: c.Cols, Rows: c.Rows,
			Tail:    base64.StdEncoding.EncodeToString(tail),
			Repaint: base64.StdEncoding.EncodeToString(buildRepaint(server)),
			Poison:  base64.StdEncoding.EncodeToString([]byte(poison)),
			Want:    trimRows(server.Lines()),
			CurX:    cx, CurY: cy, CurVis: cv, Alt: server.AltScreen(),
			RingOnly:     countMatch(trimRows(server.Lines()), trimRows(ringOnly.Lines())),
			NonDefaultBG: ndbg,
			Bold:         bold,
			BoldXY:       boldXY,
		})
	}
	buf, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "probe-data.js")
	if err := os.WriteFile(path, append([]byte("window.PROBE = "), append(buf, ';')...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d corpora, %d bytes)", path, len(out), len(buf))
}
