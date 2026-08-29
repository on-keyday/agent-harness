package tui

import (
	"fmt"
	"strings"
	"testing"
)

// The header must survive any amount of traffic. The exec listing shipped into
// a five-line region that scrolled to the bottom on every write, so a listing
// longer than the region hid its own header — this view exists not to repeat it.
func TestForwardTapViewKeepsItsHeaderWhenFull(t *testing.T) {
	v := NewForwardTapView(7)
	v.Open()
	v.SetSize(120, 20)
	for i := 0; i < 500; i++ {
		v.Append([]string{fmt.Sprintf("#1 ->     12:00:%02d.000  4B", i%60)})
	}
	out := v.View()
	if !strings.Contains(out, "forward #7") {
		t.Fatalf("the view scrolled its own header away:\n%s", out)
	}
}

// A real viewport, not a strip: a tap emits several lines per record.
func TestForwardTapViewIsARealViewport(t *testing.T) {
	v := NewForwardTapView(7)
	if got := v.Height(24); got < 10 {
		t.Fatalf("viewport height for a 24-row terminal = %d", got)
	}
}

// An operator who scrolls up is reading something; new traffic must not yank
// them back. G re-arms following.
func TestForwardTapViewScrollingReleasesFollow(t *testing.T) {
	v := NewForwardTapView(7)
	v.Open()
	v.SetSize(120, 20)
	for i := 0; i < 100; i++ {
		v.Append([]string{fmt.Sprintf("line %d", i)})
	}
	if !v.Following() {
		t.Fatal("a fresh tap must follow")
	}
	v.Update(keyMsg("k"))
	if v.Following() {
		t.Fatal("scrolling up must release follow")
	}
	v.Update(keyMsg("G"))
	if !v.Following() {
		t.Fatal("G must re-arm follow")
	}
}

// The line buffer is bounded: a tap on a busy forward outruns any reader.
func TestForwardTapViewTrimsOldLines(t *testing.T) {
	v := NewForwardTapView(7)
	v.Open()
	v.SetSize(120, 20)
	for i := 0; i < forwardTapMaxLines+250; i++ {
		v.Append([]string{fmt.Sprintf("line %d", i)})
	}
	if v.LineCount() != forwardTapMaxLines {
		t.Fatalf("kept %d lines, want the cap %d", v.LineCount(), forwardTapMaxLines)
	}
}
