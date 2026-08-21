//go:build !js

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// emuWith renders content into a fresh cols x rows emulator. No drain goroutine:
// the test input contains no terminal queries, which are the only thing that
// makes emu.Write block on its own output side.
func emuWith(cols, rows int, content string) *vt.Emulator {
	emu := vt.NewEmulator(cols, rows)
	emu.Write([]byte(content))
	return emu
}

// The `--- styles ---` report must be a PROJECTION of the collected spans, not
// a second scan of the grid: --json and the text form otherwise drift into two
// descriptions of one screen. This pins the projection by rebuilding the report
// line from the struct's own fields and comparing it to formatSpans' output.
func TestFormatSpansIsAProjectionOfCollectSpans(t *testing.T) {
	emu := emuWith(20, 2, "AB\x1b[1mBOLD\x1b[0mCD")
	defer emu.Close()

	spans := collectSpans(emu, 20, 2, true, false)
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d: %+v", len(spans), spans)
	}
	got := spans[0]
	if got.Row != 0 || got.Start != 2 || got.End != 5 || got.Text != "BOLD" {
		t.Errorf("span = %+v, want row 0 cols 2-5 %q", got, "BOLD")
	}
	if len(got.Attrs) != 1 || got.Attrs[0] != "bold" {
		t.Errorf("attrs = %v, want [bold]", got.Attrs)
	}
	if got.label() != "bold" {
		t.Errorf("label() = %q, want %q", got.label(), "bold")
	}

	want := `r0 c2-5 bold: "BOLD"`
	if report := formatSpans(spans); report != want {
		t.Errorf("formatSpans = %q, want %q", report, want)
	}
}

// Colour spans re-specify the label as "<attrs> fg#rrggbb bg#rrggbb", and the
// structured form must carry the same three pieces separately. Without this the
// JSON could report a colour the text report does not, or vice versa.
func TestCollectSpansCarriesColourSeparatelyFromItsLabel(t *testing.T) {
	emu := emuWith(20, 1, "\x1b[38;2;255;135;175mERR\x1b[0m")
	defer emu.Close()

	spans := collectSpans(emu, 20, 1, false, true)
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d: %+v", len(spans), spans)
	}
	got := spans[0]
	if got.Fg != "#ff87af" {
		t.Errorf("fg = %q, want #ff87af", got.Fg)
	}
	if len(got.Attrs) != 0 {
		t.Errorf("attrs = %v, want none (attributes were not collected)", got.Attrs)
	}
	if !strings.Contains(got.label(), "fg#ff87af") {
		t.Errorf("label() = %q, want it to carry fg#ff87af", got.label())
	}
}

// Start/End are grid COLUMNS. A wide (CJK) cell advances the scan by two, so the
// span covering it must report a two-column span for one character — the skill
// tells readers not to slice a line by these offsets, and this is why.
func TestCollectSpansCountsColumnsNotCharacters(t *testing.T) {
	emu := emuWith(20, 1, "\x1b[1m日\x1b[0m")
	defer emu.Close()

	spans := collectSpans(emu, 20, 1, true, false)
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d: %+v", len(spans), spans)
	}
	if got := spans[0]; got.Start != 0 || got.End != 1 || got.Text != "日" {
		t.Errorf("span = %+v, want one character spanning columns 0-1", got)
	}
}

// An unstyled screen must yield an EMPTY slice rather than nil: nil marshals as
// JSON null, which reads as "no spans field" instead of "no spans". The text
// form has its own sentinel for the same state.
func TestCollectSpansIsEmptyNotNilOnACleanScreen(t *testing.T) {
	emu := emuWith(10, 1, "plain")
	defer emu.Close()

	spans := collectSpans(emu, 10, 1, true, true)
	if spans == nil {
		t.Fatal("collectSpans returned nil; JSON would encode it as null")
	}
	if len(spans) != 0 {
		t.Fatalf("want no spans on an unstyled screen, got %+v", spans)
	}
	b, err := json.Marshal(spans)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Errorf("marshalled %s, want []", b)
	}
	if report := formatSpans(spans); report != "(no styled spans)" {
		t.Errorf("formatSpans = %q, want the no-spans sentinel", report)
	}
}

// Spans carry absolute grid rows, so Lines must be exactly Rows long or
// Lines[span.Row] is a bounds error on the reader's side. The emulator trims
// trailing blank rows, which is the case that would otherwise break it.
func TestScreenLinesIsAlwaysExactlyRowsLong(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		rows int
		want []string
	}{
		{"pads a trimmed tail", "a\nb", 4, []string{"a", "b", "", ""}},
		{"drops the trailing newline", "a\nb\n", 2, []string{"a", "b"}},
		{"truncates an over-long render", "a\nb\nc", 2, []string{"a", "b"}},
		{"an empty screen is still rows long", "", 3, []string{"", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := screenLines(tc.text, tc.rows)
			if len(got) != tc.rows {
				t.Fatalf("len = %d, want %d (%q)", len(got), tc.rows, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
