package tui

import (
	"strings"
	"testing"
)

func mkPanes(ids ...string) []*PaneStreamer {
	ps := make([]*PaneStreamer, 0, len(ids))
	for _, id := range ids {
		ps = append(ps, NewPaneStreamer(id, 24, 80))
	}
	return ps
}

func TestGridModel_Paging(t *testing.T) {
	m := NewGridModel()
	m.panes = mkPanes("AAAAAAAA", "BBBBBBBB", "CCCCCCCC", "DDDDDDDD",
		"EEEEEEEE", "FFFFFFFF", "GGGGGGGG", "HHHHHHHH") // 8 panes
	m.open = true
	m.SetSize(120, 40)

	if m.pageCount() != 2 {
		t.Fatalf("8 panes / %d per page = 2 pages, got %d", gridPerPage, m.pageCount())
	}
	if m.pageStart() != 0 || m.pageEnd() != 6 {
		t.Fatalf("page 0 = [0,6), got [%d,%d)", m.pageStart(), m.pageEnd())
	}
	m2, _ := m.Update(keyMsg("]"))
	if m2.page != 1 || m2.pageStart() != 6 || m2.pageEnd() != 8 {
		t.Fatalf("] -> page 1 [6,8), got page %d [%d,%d)", m2.page, m2.pageStart(), m2.pageEnd())
	}
	view := m2.View()
	if !strings.Contains(view, "GGGGGGGG") || !strings.Contains(view, "HHHHHHHH") {
		t.Fatalf("page 1 view must show panes 7 & 8:\n%s", view)
	}
	if strings.Contains(view, "AAAAAAAA") {
		t.Fatalf("page 1 view must NOT show page-0 pane AAAAAAAA")
	}
	// next past the last page is a no-op
	m3, _ := m2.Update(keyMsg("]"))
	if m3.page != 1 {
		t.Fatalf("] past last page should clamp at 1, got %d", m3.page)
	}
	m4, _ := m3.Update(keyMsg("["))
	if m4.page != 0 {
		t.Fatalf("[ -> page 0, got %d", m4.page)
	}
}

func TestGridModel_MovePane(t *testing.T) {
	m := NewGridModel()
	m.panes = mkPanes("AAAAAAAA", "BBBBBBBB", "CCCCCCCC")
	m.open = true
	m.SetSize(120, 40)

	// focus 0 = AAAAAAAA; Shift+L moves it right (swap with BBBBBBBB), focus follows.
	m2, _ := m.Update(keyMsg("L"))
	if m2.panes[0].TaskID() != "BBBBBBBB" || m2.panes[1].TaskID() != "AAAAAAAA" {
		t.Fatalf("L should swap pane0<->pane1, got %s,%s", m2.panes[0].TaskID(), m2.panes[1].TaskID())
	}
	if m2.FocusedTaskID() != "AAAAAAAA" {
		t.Fatalf("focus follows the moved pane, got %s", m2.FocusedTaskID())
	}
	// Shift+H moves it back.
	m3, _ := m2.Update(keyMsg("H"))
	if m3.panes[0].TaskID() != "AAAAAAAA" {
		t.Fatalf("H should swap back, got %s", m3.panes[0].TaskID())
	}
}

func TestGridModel_MovePaneAcrossPage(t *testing.T) {
	m := NewGridModel()
	m.panes = mkPanes("A", "B", "C", "D", "E", "F", "G", "H") // 8 panes, 2 pages
	m.open = true
	m.SetSize(120, 40)
	m.focus = 5 // last pane on page 0
	m.page = 0
	// Shift+L pushes it to index 6 -> page 1; focus + page follow.
	m2, _ := m.Update(keyMsg("L"))
	if m2.focus != 6 || m2.page != 1 {
		t.Fatalf("moving across the page boundary should follow to page 1 (focus 6), got focus %d page %d", m2.focus, m2.page)
	}
}
