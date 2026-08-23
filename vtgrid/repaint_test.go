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

// poison is the state a torn-off full-screen app leaves an observer in: alt
// buffer, a scroll region, origin mode, hidden cursor, no autowrap, a live SGR
// pen, and debris on the grid. It is the reattach symptom in a string literal.
const poison = "\x1b[?1049h\x1b[5;20r\x1b[?6h\x1b[?25l\x1b[?7l\x1b[7;31;44m" +
	"leftovers from a full-screen app\r\nsecond line of debris"

// replayTailBytes is what a real `session attach` replays: cli/attach_native.go
// passes replayLimit 0, which is the whole ring, and the ring defaults to 1 MiB
// (server/task_handler.go). Cut at an arbitrary offset with no ground-state
// scan, which is harsher than what the server does.
//
// It was 32 KiB, chosen as "smaller than production must be safer". It is not:
// a shorter tail is LESS likely to contain the app's ESC[?1049h, so it leaves
// the observer on the main buffer and quietly skips the case where the repaint
// has to fix an observer already on the alternate one. Every alt-screen capture
// in the set reaches that case at 128 KiB and above, and at 32 KiB only one
// does. Truncating harder is not the conservative choice it looks like.
const replayTailBytes = 1 << 20

// replayTailSizes are the replay sizes that actually occur, plus one far below
// any of them. The corpora are 256 KiB, so the largest is the whole stream.
var replayTailSizes = []struct {
	Name string
	N    int
}{
	{"tail-32k", 32 << 10},         // below anything production does
	{"grid-pane-128k", 128 << 10},  // cli/preview_wasm.go GridPaneReplayLimit
	{"full-ring", replayTailBytes}, // session attach: replayLimit 0
}

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

// repaintOracleDefects is how many rows x/vt loses on a corpus's repaint in
// cases where x/vt is the one that is wrong. Same shape and same reasoning as
// knownOracleDefects in diff_test.go: an entry is NOT a suppression — the count
// is asserted exactly, so a fix upstream fails this test and tells you to delete
// the entry rather than going quiet and leaving a stale excuse in the tree.
//
// claude-tui is the single defect that map already records at row 36, reached
// here by a second route. x/vt honours the eight-bit ST (0x9C) inside an OSC;
// 0x9C is the second byte of U+2733 ✳; and that glyph opens the window title
// Claude Code sets. So the title Repaint now re-emits ends one byte in and the
// rest of it prints onto the grid. vtgrid.Terminal.str documents why this parser
// refuses that terminator.
//
// Emitting the title is not what puts those bytes in front of an emulator for
// the first time: the app itself sends that OSC live, to the same observer, over
// the path that has always carried it. An emulator that breaks on it broke
// before any repaint happened.
var repaintOracleDefects = map[string]int{
	"claude-tui": 1,
}

// TestRepaintReconstructsScreen asserts that the repaint lands the server's
// screen on an observer regardless of the state that observer was in.
//
// The ring-only column in the log is the CURRENT behaviour for the same input,
// and it is the reason this exists: at a full-ring replay htop reconstructs 5
// of 40 rows and vim-split 2 of 40.
//
// The ring observer is run at every replay size that actually occurs, not just
// one. Size is not a smooth axis here: whether the app's ESC[?1049h falls
// inside the window decides whether the observer arrives on the alternate
// buffer, which is a different case for the repaint to solve and the one that
// exposed the conhost DECTCEM defect. A single small tail tests the easy half
// and reports full marks.
func TestRepaintReconstructsScreen(t *testing.T) {
	type observer struct {
		Name string
		Pre  []byte
	}
	var totRows, totCells, okRows, okCells, okOracle int

	for _, c := range extCorpora {
		data := loadExtCorpus(t, c.Name)

		server := vtgrid.New(c.Cols, c.Rows)
		_, _ = server.Write(data)
		want := trimRows(server.Lines())
		cx, cy, cv := server.Cursor()
		rp := server.Repaint()

		observers := []observer{{"fresh", nil}, {"poisoned", []byte(poison)}}
		for _, s := range replayTailSizes {
			observers = append(observers, observer{"ring/" + s.Name, ringTail(data, s.N)})
		}

		for _, ob := range observers {
			term := vtgrid.New(c.Cols, c.Rows)
			_, _ = term.Write(ob.Pre)
			before := countMatch(want, trimRows(term.Lines()))
			onAltBefore := term.AltScreen()
			_, _ = term.Write(rp)

			rows := countMatch(want, trimRows(term.Lines()))
			cells, cTot := countCellMatch(server, term)
			orc := countMatch(want,
				oracleRows(append(append([]byte{}, ob.Pre...), rp...), c.Cols, c.Rows))

			totRows += c.Rows
			totCells += cTot
			okRows += rows
			okCells += cells
			okOracle += orc

			if rows != c.Rows {
				t.Errorf("%s/%s: rows %d/%d after repaint", c.Name, ob.Name, rows, c.Rows)
			}
			if cells != cTot {
				t.Errorf("%s/%s: cells %d/%d after repaint", c.Name, ob.Name, cells, cTot)
			}
			switch defect := repaintOracleDefects[c.Name]; {
			case orc == c.Rows-defect:
				// As expected — including the defect==0 case, which is most of them.
			case defect > 0 && orc == c.Rows:
				t.Errorf("%s/%s: x-vt now reconstructs all %d rows — its known "+
					"defect no longer reproduces; delete the repaintOracleDefects "+
					"entry for %s (and check knownOracleDefects in diff_test.go)",
					c.Name, ob.Name, c.Rows, c.Name)
			default:
				t.Errorf("%s/%s: x-vt rows %d/%d after repaint, want %d",
					c.Name, ob.Name, orc, c.Rows, c.Rows-defect)
			}
			gx, gy, gv := term.Cursor()
			if gx != cx || gy != cy || gv != cv {
				t.Errorf("%s/%s: cursor (%d,%d,%v), want (%d,%d,%v)",
					c.Name, ob.Name, gx, gy, gv, cx, cy, cv)
			}
			if term.AltScreen() != server.AltScreen() {
				t.Errorf("%s/%s: alt-screen %v, want %v",
					c.Name, ob.Name, term.AltScreen(), server.AltScreen())
			}

			t.Logf("%-14s %-20s before=%2d/%d on-alt-before=%-5v  after=%2d/%d cells=%d/%d x-vt=%2d",
				c.Name, ob.Name, before, c.Rows, onAltBefore, rows, c.Rows, cells, cTot, orc)
		}
	}

	pct := func(n, d int) float64 { return 100 * float64(n) / float64(d) }
	t.Logf("ROWS  %d/%d (%.1f%%)   x-vt %d/%d (%.1f%%)   CELLS %d/%d (%.3f%%)",
		okRows, totRows, pct(okRows, totRows), okOracle, totRows, pct(okOracle, totRows),
		okCells, totCells, pct(okCells, totCells))
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
			Repaint: base64.StdEncoding.EncodeToString(server.Repaint()),
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
