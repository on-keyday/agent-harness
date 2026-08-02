package tui

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// refBuffer is a deliberately naive model of the same editing semantics: the
// whole document as one rune slice with a single offset. It is too slow to
// ship and too simple to be wrong, which is the point — editBuffer's
// line/window bookkeeping is where an aliasing or index bug would hide, and
// this gives every operation an independent answer to be checked against.
type refBuffer struct {
	text []rune
	pos  int
}

func (r *refBuffer) Value() string { return string(r.text) }

func (r *refBuffer) insert(rs []rune) {
	out := make([]rune, 0, len(r.text)+len(rs))
	out = append(out, r.text[:r.pos]...)
	out = append(out, rs...)
	out = append(out, r.text[r.pos:]...)
	r.text = out
	r.pos += len(rs)
}

func (r *refBuffer) backspace() {
	if r.pos == 0 {
		return
	}
	out := make([]rune, 0, len(r.text)-1)
	out = append(out, r.text[:r.pos-1]...)
	out = append(out, r.text[r.pos:]...)
	r.text = out
	r.pos--
}

func (r *refBuffer) deleteForward() {
	if r.pos >= len(r.text) {
		return
	}
	out := make([]rune, 0, len(r.text)-1)
	out = append(out, r.text[:r.pos]...)
	out = append(out, r.text[r.pos+1:]...)
	r.text = out
}

func (r *refBuffer) left() {
	if r.pos > 0 {
		r.pos--
	}
}

func (r *refBuffer) right() {
	if r.pos < len(r.text) {
		r.pos++
	}
}

func (r *refBuffer) home() {
	for r.pos > 0 && r.text[r.pos-1] != '\n' {
		r.pos--
	}
}

func (r *refBuffer) end() {
	for r.pos < len(r.text) && r.text[r.pos] != '\n' {
		r.pos++
	}
}

// TestEditBufMatchesReferenceModel drives both implementations through the
// same random edit sequence and compares the document after every step. Only
// operations whose cursor semantics are identical in both models are used —
// up/down are excluded because the reference has no notion of a display
// column — so a divergence here is a real defect, not a modelling artifact.
func TestEditBufMatchesReferenceModel(t *testing.T) {
	const seed = 20260802
	rng := rand.New(rand.NewSource(seed))
	start := "これは日本語のメモです。\nsecond line here\n三行目です\n\nlast\n"

	b := newEditBuffer()
	b.SetSize(24, 6)
	b.SetValue(start)
	b.CursorTop()

	ref := &refBuffer{text: []rune(start)}

	inserts := [][]rune{
		[]rune("a"), []rune("あ"), []rune("XY"), []rune("日本"),
		[]rune("\n"), []rune(" "), []rune("ぁー〜"),
	}

	for step := 0; step < 4000; step++ {
		var op string
		switch rng.Intn(8) {
		case 0, 1, 2:
			rs := inserts[rng.Intn(len(inserts))]
			op = "insert " + string(rs)
			b.InsertRunes(rs)
			ref.insert(rs)
		case 3:
			op = "backspace"
			b.Backspace()
			ref.backspace()
		case 4:
			op = "delete"
			b.DeleteForward()
			ref.deleteForward()
		case 5:
			op = "left"
			b.CursorLeft()
			ref.left()
		case 6:
			op = "right"
			b.CursorRight()
			ref.right()
		case 7:
			if rng.Intn(2) == 0 {
				op = "home"
				b.CursorHome()
				ref.home()
			} else {
				op = "end"
				b.CursorEnd()
				ref.end()
			}
		}

		got, want := b.Value(), ref.Value()
		if got != want {
			t.Fatalf("step %d (%s): documents diverged\n got: %q\nwant: %q", step, op, got, want)
		}
		// The saved file must always be something the loader will take back:
		// FileEditLoad refuses invalid UTF-8 or an embedded NUL.
		if !utf8.ValidString(got) {
			t.Fatalf("step %d (%s): buffer holds invalid UTF-8", step, op)
		}
		if bytes.IndexByte([]byte(got), 0) >= 0 {
			t.Fatalf("step %d (%s): buffer holds a NUL byte: %q", step, op, got)
		}
		// Rendering must never crash or leak a NUL onto the screen either.
		for _, row := range b.Render() {
			if strings.IndexByte(row, 0) >= 0 {
				t.Fatalf("step %d (%s): rendered row holds a NUL: %q", step, op, row)
			}
		}
	}
}
