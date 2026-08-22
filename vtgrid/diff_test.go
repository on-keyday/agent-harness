package vtgrid

// Differential test against github.com/charmbracelet/x/vt.
//
// x/vt is the ORACLE here, not ground truth: where the two disagree, either
// could be the wrong one, and the report is written to be read rather than to
// be trusted. What it buys is that a divergence gets found by a machine on
// real captured output instead of by a person noticing a wrong screen later.
//
// The comparison is on rendered rows with trailing blanks trimmed. Trailing
// blanks are not a rendering difference anyone acts on, and keeping them would
// bury real divergences under a uniform column of noise.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// renderOracle feeds a corpus through x/vt and returns its rows.
func renderOracle(tb testing.TB, data []byte, cols, rows int) []string {
	tb.Helper()
	emu := vt.NewEmulator(cols, rows)
	emu.Scrollback().SetMaxLines(1) // the smallest x/vt accepts; 0 is a no-op
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, emu)
	}()
	_, _ = emu.Write(data)
	out := splitRows(emu.String(), rows)
	_ = emu.Close()
	<-done
	return out
}

func renderOurs(data []byte, cols, rows int) []string {
	t := New(cols, rows)
	_, _ = t.Write(data)
	return t.Lines()
}

// splitRows normalises a rendered screen to exactly rows entries. The oracle
// may return fewer (trailing blank rows collapsed), and a row-indexed
// comparison needs both sides the same length.
func splitRows(s string, rows int) []string {
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return lines[:rows]
}

func trimAll(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(r, " ")
	}
	return out
}

// knownOracleDefects lists rows where the two renders differ AND the oracle is
// the one that is wrong. An entry is not a suppression: the test asserts the
// divergence is STILL THERE, so if x/vt is fixed upstream the suite fails
// telling you to delete the entry, rather than going quiet and leaving a stale
// excuse in the tree.
var knownOracleDefects = map[string]map[int]string{
	"claude-tui": {
		36: "x/vt honours the eight-bit ST (0x9C) inside an OSC. 0x9C is the " +
			"second byte of U+2733 ✳, the glyph Claude Code puts in its window " +
			"title, so x/vt ends the sequence one byte in and prints the rest of " +
			"the title onto the grid. cli/snapshot_native.go already documents " +
			"the symptom; see vtgrid.Terminal.str for the cause. " +
			"TestLocateDivergence finds the same defect at INTERMEDIATE frames " +
			"in altscreen, claude-tui, codex-tui and torture, where a later " +
			"repaint covers it again before the final grid — four sightings, " +
			"one cause, each pinpointed to the byte after ESC ] 0 ; U+2733.",
	},
}

// TestDiffAgainstOracle reports, per corpus, how many rendered rows match
// x/vt. It fails on any mismatch — the point of the exercise is parity — and
// prints the first few differing rows side by side so the failure is
// actionable rather than a count.
func TestDiffAgainstOracle(t *testing.T) {
	const showRows = 4
	totalRows, totalMatch := 0, 0
	for _, c := range vtCorpora {
		data := loadVTCorpus(t, c.Name)
		want := trimAll(renderOracle(t, data, c.Cols, c.Rows))
		got := trimAll(renderOurs(data, c.Cols, c.Rows))

		defects := knownOracleDefects[c.Name]
		match, expected, shown := 0, 0, 0
		var b strings.Builder
		for i := range want {
			if want[i] == got[i] {
				if _, listed := defects[i]; listed {
					t.Errorf("%s row %d: the known oracle defect no longer reproduces — "+
						"delete its entry from knownOracleDefects", c.Name, i)
				}
				match++
				continue
			}
			if why, listed := defects[i]; listed {
				expected++
				t.Logf("%-12s row %d differs as expected: %s", c.Name, i, why)
				continue
			}
			if shown < showRows {
				fmt.Fprintf(&b, "\n    row %2d\n      oracle: %s\n      ours  : %s", i, clip(want[i]), clip(got[i]))
				shown++
			}
		}
		totalRows += len(want)
		totalMatch += match + expected
		pct := 100 * float64(match+expected) / float64(len(want))
		if match+expected == len(want) {
			t.Logf("%-12s %d/%d rows match (100%%, %d known oracle defect(s))", c.Name, match, len(want), expected)
			continue
		}
		t.Errorf("%-12s %d/%d rows match (%.0f%%)%s", c.Name, match, len(want), pct, b.String())
	}
	t.Logf("TOTAL %d/%d rows (%.1f%%)", totalMatch, totalRows, 100*float64(totalMatch)/float64(totalRows))
}

// clip keeps a divergence report readable. VTDIFF_WIDTH raises the cut when a
// difference lives past the default column, which is otherwise invisible: two
// rows clipped at the same point look identical while differing after it.
func clip(s string) string {
	w := 96
	if v := os.Getenv("VTDIFF_WIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			w = n
		}
	}
	if len([]rune(s)) <= w {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…", string([]rune(s)[:w]))
}

// TestTitleAgainstOracle checks the OSC 0/2 title separately, because it is not
// on the grid and a row comparison cannot see it. The oracle is not used here:
// x/vt's Title callback truncates a title at its first multi-byte character,
// which is exactly why cli/snapshot_native.go scans the bytes instead. The
// expectation is therefore the corpus's own last complete title.
func TestTitleAgainstOracle(t *testing.T) {
	for _, c := range vtCorpora {
		data := loadVTCorpus(t, c.Name)
		term := New(c.Cols, c.Rows)
		_, _ = term.Write(data)
		want := lastOSCTitleRef(data)
		if got := term.Title(); got != want {
			t.Errorf("%s: title = %q, want %q", c.Name, got, want)
		}
	}
}

// lastOSCTitleRef is an independent scan for the last complete OSC 0/2 title,
// written straight rather than through the parser it is checking.
func lastOSCTitleRef(b []byte) string {
	out := ""
	for i := 0; i+3 < len(b); {
		if b[i] != 0x1b || b[i+1] != ']' {
			i++
			continue
		}
		j := i + 2
		cmd, digits := 0, 0
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			cmd = cmd*10 + int(b[j]-'0')
			digits++
			j++
		}
		if digits == 0 || j >= len(b) || b[j] != ';' {
			i += 2
			continue
		}
		j++
		start, end, next := j, -1, -1
		for k := j; k < len(b); k++ {
			if b[k] == 0x07 {
				end, next = k, k+1
				break
			}
			if b[k] == 0x1b && k+1 < len(b) && b[k+1] == '\\' {
				end, next = k, k+2
				break
			}
			if b[k] == 0x1b {
				break
			}
		}
		if end < 0 {
			i += 2
			continue
		}
		if cmd == 0 || cmd == 2 {
			out = string(b[start:end])
		}
		i = next
	}
	return out
}
