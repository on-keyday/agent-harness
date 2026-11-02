package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

func boardApp(t *testing.T) *App {
	t.Helper()
	a := &App{boardModal: NewBoardModal()}
	a.boardModal.Open()
	a.boardModal.SetSize(160, 45)
	return a
}

// A flushed topic's message view holds no rows, and O must still page: from
// the newest on disk, which is what the flush took out of the ring.
func TestBoardOlder_EmptyRingPagesFromNewest(t *testing.T) {
	a := boardApp(t)
	a.boardModal.ApplyMessages("kept", nil, nil, true)
	if _, cmd := a.updateKey(key(modalKeys.BoardOlder)); cmd == nil {
		t.Fatal("O on an empty ring produced no request")
	}
}

// F flushes, and is offered, only on a retained topic.
func TestBoardFlushKey_OnlyOnRetained(t *testing.T) {
	a := boardApp(t)
	a.boardModal.ApplyTopics([]cli.BoardTopicRow{
		{Name: "kept", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 1, Retention: &cli.TopicRetention{Forever: true, RingFloor: 41}},
		{Name: "plain", MsgCount: 1, LastSeq: 2, LastPublishedAtMs: 1},
	}, nil)
	view := a.boardModal.View()
	if !strings.Contains(view, modalKeys.BoardFlush+": flush") {
		t.Error("footer on a retained row does not offer flush")
	}
	if !strings.Contains(view, "flushed≤41") {
		t.Error("a flushed topic's retention line does not show the floor")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardFlush)); cmd == nil {
		t.Fatal("F on a retained row produced no request")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyDown})
	if strings.Contains(a.boardModal.View(), modalKeys.BoardFlush+": flush") {
		t.Error("footer on a plain row offers flush")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardFlush)); cmd != nil {
		t.Fatal("F on a topic that is not retained produced a request")
	}
	if !strings.Contains(a.boardModal.View(), "not retained") {
		t.Error("F on a plain row did not say why nothing happened")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardArchive)); cmd == nil {
		t.Error("A on a topic produced no archive request")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardArchives)); cmd == nil {
		t.Error("v produced no archive listing request")
	}
}

func TestBoardArchivesView_EnterReadsAndXDeletes(t *testing.T) {
	a := boardApp(t)
	rows := []cli.BoardArchiveRow{
		{ID: "0a1b2c3d4e5f", Topic: "kept", Through: 9, Count: 3, CreatedAt: time.Unix(1_700_000_000, 0)},
		{ID: "ffffffffffff", Topic: "other", Through: 5, Count: 1, CreatedAt: time.Unix(1_700_000_000, 0)},
	}
	a.boardModal.ApplyArchives("", rows)
	if a.boardModal.Mode() != boardArchives {
		t.Fatalf("mode %v after ApplyArchives", a.boardModal.Mode())
	}
	if !strings.Contains(a.boardModal.View(), rows[0].Summary()) {
		t.Error("the list does not show the CLI's spelling of a row")
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyDown})
	if r, ok := a.boardModal.SelectedArchive(); !ok || r.ID != "ffffffffffff" {
		t.Fatalf("selected %+v", r)
	}
	if _, cmd := a.updateKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("Enter on an archive produced no read")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardForgetContact)); cmd == nil {
		t.Fatal("x on an archive produced no delete")
	}

	// The read lands: the message view, read-only, with X purging from the
	// archive and O paging it.
	a.boardModal.ApplyArchiveMessages("ffffffffffff", "other", seqMsgs(4, 5))
	if a.boardModal.CurArchive() != "ffffffffffff" || a.boardModal.Mode() != boardMessages {
		t.Fatalf("archive %q mode %v", a.boardModal.CurArchive(), a.boardModal.Mode())
	}
	view := a.boardModal.View()
	if !strings.Contains(view, "archive ffffffffffff") {
		t.Error("the header does not name the archive")
	}
	if strings.Contains(view, modalKeys.BoardCompose+": send") {
		t.Error("an archive's view offers send")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardCompose)); cmd != nil || a.boardModal.Composing() {
		t.Error("m opened a composer on an archive")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardPurgeMsg)); cmd == nil {
		t.Error("X in an archive produced no purge")
	}
	if _, cmd := a.updateKey(key(modalKeys.BoardOlder)); cmd == nil {
		t.Error("O in an archive produced no page")
	}
	a.boardModal.PrependArchiveOlder("ffffffffffff", seqMsgs(2, 3))
	if a.boardModal.OldestSeq() != 2 {
		t.Errorf("OldestSeq after an older archive page = %d", a.boardModal.OldestSeq())
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyEsc})
	if a.boardModal.Mode() != boardArchives || a.boardModal.CurArchive() != "" {
		t.Errorf("Esc from an archive went to mode %v (archive %q)", a.boardModal.Mode(), a.boardModal.CurArchive())
	}
	a.updateKey(tea.KeyMsg{Type: tea.KeyEsc})
	if a.boardModal.Mode() != boardTopics {
		t.Errorf("Esc from the archive list went to mode %v", a.boardModal.Mode())
	}
}
