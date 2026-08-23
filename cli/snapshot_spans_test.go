//go:build !js

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// screenWith renders content into a fresh cols x rows screen model.
func screenWith(cols, rows int, content string) *vtgrid.Terminal {
	term := vtgrid.New(cols, rows)
	_, _ = term.Write([]byte(content))
	return term
}

// The `--- styles ---` report must be a PROJECTION of the collected spans, not
// a second scan of the grid: --json and the text form otherwise drift into two
// descriptions of one screen. This pins the projection by rebuilding the report
// line from the struct's own fields and comparing it to formatSpans' output.
func TestFormatSpansIsAProjectionOfCollectSpans(t *testing.T) {
	term := screenWith(20, 2, "AB\x1b[1mBOLD\x1b[0mCD")
	spans := collectSpans(term, 20, 2, true, false)
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
	term := screenWith(20, 1, "\x1b[38;2;255;135;175mERR\x1b[0m")
	spans := collectSpans(term, 20, 1, false, true)
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
	term := screenWith(20, 1, "\x1b[1m日\x1b[0m")
	spans := collectSpans(term, 20, 1, true, false)
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
	term := screenWith(10, 1, "plain")
	spans := collectSpans(term, 10, 1, true, true)
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

// The cursor and the buffer are terminal STATE, and no amount of reading Lines
// recovers them: a cursor parked mid-transcript and one sitting at the prompt
// produce identical rows, and so do the alternate and primary buffers. Both are
// reported unconditionally, so a reader never has to ask whether the fields are
// present before trusting them.
func TestSnapshotReportsCursorAndBuffer(t *testing.T) {
	// Cursor left at row 1 column 3 with a hidden cursor, on the primary buffer.
	term := screenWith(20, 3, "hello\r\n\x1b[2;4H\x1b[?25l")
	snap := buildSnapshot(term, "task", "", 20, 3, LiveActivity{}, false, false)
	if snap.Cursor.X != 3 || snap.Cursor.Y != 1 {
		t.Errorf("cursor = (%d,%d), want (3,1)", snap.Cursor.X, snap.Cursor.Y)
	}
	if snap.Cursor.Visible {
		t.Error("cursor reported visible after ESC[?25l")
	}
	if snap.AltScreen {
		t.Error("alt_screen true on the primary buffer")
	}

	// Hiding does not move it: position and visibility are independent, and a
	// snapshot that folded them together would lose one.
	shown := screenWith(20, 3, "hello\r\n\x1b[2;4H\x1b[?25l\x1b[?25h")
	if s := buildSnapshot(shown, "task", "", 20, 3, LiveActivity{}, false, false); !s.Cursor.Visible ||
		s.Cursor.X != 3 || s.Cursor.Y != 1 {
		t.Errorf("cursor = %+v, want the same position, visible", s.Cursor)
	}

	alt := screenWith(20, 3, "\x1b[?1049hfullscreen")
	if s := buildSnapshot(alt, "task", "", 20, 3, LiveActivity{}, false, false); !s.AltScreen {
		t.Error("alt_screen false after ESC[?1049h")
	}
}

// The fields must survive to the wire under the names the --json help text
// promises. Renaming a JSON tag is invisible to every Go-side test above.
func TestSnapshotCursorMarshalsUnderItsDocumentedNames(t *testing.T) {
	snap := buildSnapshot(screenWith(10, 1, "x"), "task", "", 10, 1, LiveActivity{}, false, false)
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"cursor", "alt_screen"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%q missing from the snapshot object: %s", k, b)
		}
	}
	var cur map[string]json.RawMessage
	if err := json.Unmarshal(got["cursor"], &cur); err != nil {
		t.Fatalf("cursor is not an object: %s", got["cursor"])
	}
	for _, k := range []string{"x", "y", "visible"} {
		if _, ok := cur[k]; !ok {
			t.Errorf("cursor.%s missing: %s", k, got["cursor"])
		}
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
