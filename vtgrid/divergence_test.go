package vtgrid

// Locating a divergence, rather than only counting them.
//
// Both implementations are incremental, so the cheap way to find where they
// part company is to feed them the same chunks in lockstep and compare after
// each one. That costs a single pass over the corpus, and it exercises the
// split-sequence path for free: a chunk boundary lands mid-escape roughly as
// often as a frame boundary does in production.
//
// Run with:
//
//	go test ./vtgrid/ -run TestLocateDivergence -v
//
// It is a diagnostic, not a gate — TestDiffAgainstOracle is the gate. This one
// only reports, so that a failing parity run has somewhere to point.

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

const divergenceChunk = 512

func TestLocateDivergence(t *testing.T) {
	for _, c := range vtCorpora {
		data := loadVTCorpus(t, c.Name)

		oracle := vt.NewEmulator(c.Cols, c.Rows)
		oracle.Scrollback().SetMaxLines(1)
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(io.Discard, oracle) }()
		ours := New(c.Cols, c.Rows)

		found := false
		for off := 0; off < len(data); off += divergenceChunk {
			end := off + divergenceChunk
			if end > len(data) {
				end = len(data)
			}
			chunk := data[off:end]
			_, _ = oracle.Write(chunk)
			_, _ = ours.Write(chunk)

			want := trimAll(splitRows(oracle.String(), c.Rows))
			got := trimAll(ours.Lines())
			row := firstDiff(want, got)
			if row < 0 {
				continue
			}
			found = true
			t.Logf("%s: diverges within bytes [%d,%d) at row %d\n"+
				"    oracle: %s\n    ours  : %s\n    chunk : %s",
				c.Name, off, end, row, clip(want[row]), clip(got[row]), describeSeqs(chunk))
			pinpoint(t, c, data, off, end)
			break
		}
		_ = oracle.Close()
		<-done
		if !found {
			t.Logf("%s: no divergence at any chunk boundary", c.Name)
		}
	}
}

func firstDiff(a, b []string) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}

// describeSeqs renders a chunk as the sequence of commands it carries, so the
// report names the suspect rather than dumping bytes to be squinted at.
func describeSeqs(b []byte) string {
	var out []string
	seen := map[string]int{}
	var order []string
	add := func(s string) {
		if seen[s] == 0 {
			order = append(order, s)
		}
		seen[s]++
	}
	st := 0 // 0 ground, 1 esc, 2 csi, 3 str
	var params []byte
	for _, ch := range b {
		switch st {
		case 0:
			if ch == 0x1b {
				st, params = 1, params[:0]
			}
		case 1:
			switch {
			case ch == '[':
				st, params = 2, params[:0]
			case ch == ']' || ch == 'P' || ch == 'X' || ch == '^' || ch == '_':
				st = 3
			case ch == 0x1b:
			default:
				add("ESC " + string(ch))
				st = 0
			}
		case 2:
			if ch >= 0x40 && ch <= 0x7e {
				add("CSI " + string(params) + string(ch))
				st = 0
			} else if len(params) < 24 {
				params = append(params, ch)
			}
		case 3:
			if ch == 0x07 || ch == 0x9c {
				add("OSC")
				st = 0
			}
		}
	}
	for _, k := range order {
		out = append(out, fmt.Sprintf("%s×%d", k, seen[k]))
	}
	if len(out) > 18 {
		out = out[:18]
	}
	if len(out) == 0 {
		return "(printable only)"
	}
	return strings.Join(out, " ")
}

// pinpoint re-runs both implementations up to the start of a diverging chunk
// and then advances ONE BYTE AT A TIME, so the report names the exact offset
// where the two screens part rather than a 512-byte window. Rebuilding costs
// one full oracle feed per corpus, which is why it only runs for a corpus that
// already showed a divergence.
func pinpoint(t *testing.T, c vtCorpus, data []byte, from, to int) {
	t.Helper()
	oracle := vt.NewEmulator(c.Cols, c.Rows)
	oracle.Scrollback().SetMaxLines(1)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, oracle) }()
	defer func() { _ = oracle.Close(); <-done }()
	ours := New(c.Cols, c.Rows)

	_, _ = oracle.Write(data[:from])
	_, _ = ours.Write(data[:from])
	if row := firstDiff(trimAll(splitRows(oracle.String(), c.Rows)), trimAll(ours.Lines())); row >= 0 {
		t.Logf("    (already differing at row %d before byte %d — the chunk scan found it late)", row, from)
	}
	for i := from; i < to; i++ {
		_, _ = oracle.Write(data[i : i+1])
		_, _ = ours.Write(data[i : i+1])
		want := trimAll(splitRows(oracle.String(), c.Rows))
		got := trimAll(ours.Lines())
		row := firstDiff(want, got)
		if row < 0 {
			continue
		}
		lo := i - 48
		if lo < from {
			lo = from
		}
		t.Logf("    → splits at byte %d, row %d\n"+
			"      preceding: %q\n"+
			"      oracle row: %s\n      ours   row: %s",
			i, row, string(data[lo:i+1]), clip(want[row]), clip(got[row]))
		return
	}
	t.Logf("    → byte-level replay did not reproduce it (state-dependent, not a single sequence)")
}
