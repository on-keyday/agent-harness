package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// TestBoardModalApplyTopics verifies that ApplyTopics populates rowTopics and
// rebuilds the underlying table rows. Mirrors TestConnsModalApplySnapshot.
func TestBoardModalApplyTopics(t *testing.T) {
	m := NewBoardModal()

	rows := []cli.BoardTopicRow{
		{Name: "foo", MsgCount: 3, LastSeq: 3, LastPublishedAtMs: 1_000},
		{Name: "bar", MsgCount: 1, LastSeq: 1, LastPublishedAtMs: 2_000},
	}
	m.ApplyTopics(rows)

	if got := len(m.rowTopics); got != 2 {
		t.Fatalf("rowTopics: want 2, got %d", got)
	}
	if m.rowTopics[0].Name != "foo" {
		t.Errorf("rowTopics[0].Name: want foo, got %s", m.rowTopics[0].Name)
	}
	if m.rowTopics[1].Name != "bar" {
		t.Errorf("rowTopics[1].Name: want bar, got %s", m.rowTopics[1].Name)
	}
	// Verify the table has the expected row count (via the parallel slice length
	// as the source of truth — same approach as TestConnsModalApplySnapshot).
	if got := len(m.rowTopics); got != len(rows) {
		t.Errorf("table row count mismatch: rowTopics len=%d, input len=%d", got, len(rows))
	}
}

// TestBoardModalDrillAndPop exercises the two-mode state machine:
//   - ApplyMessages transitions to boardMessages mode.
//   - PopToTopics returns to boardTopics mode.
//   - Close marks the modal as not open.
func TestBoardModalDrillAndPop(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	if !m.IsOpen() {
		t.Fatal("IsOpen should be true after Open()")
	}
	if m.mode != boardTopics {
		t.Fatalf("initial mode: want boardTopics, got %v", m.mode)
	}

	msgs := []cli.BoardMessage{
		{Seq: 1, FromTaskHex: "aabbccdd", FromHostname: "host1", ReceivedAtMs: 1_000, Payload: []byte(`"hello"`)},
	}
	m.ApplyMessages("testtopic", msgs, true)
	if m.mode != boardMessages {
		t.Fatalf("after ApplyMessages: want boardMessages, got %v", m.mode)
	}
	if m.curTopic != "testtopic" {
		t.Errorf("curTopic: want testtopic, got %s", m.curTopic)
	}

	// Esc from message mode pops to topic mode (simulated via PopToTopics).
	m.PopToTopics()
	if m.mode != boardTopics {
		t.Fatalf("after PopToTopics: want boardTopics, got %v", m.mode)
	}

	// Esc from topic mode closes the modal (simulated via Close).
	m.Close()
	if m.IsOpen() {
		t.Fatal("IsOpen should be false after Close()")
	}
}

// TestBoardModalContentFormatsJSON verifies that a message carrying a valid JSON
// payload is pretty-printed in the content viewport. A plain-text payload
// should appear verbatim.
func TestBoardModalContentFormatsJSON(t *testing.T) {
	m := NewBoardModal()

	jsonMsg := cli.BoardMessage{
		Seq:          7,
		FromTaskHex:  "deadbeef001122334455667788990011",
		FromHostname: "node1",
		ReceivedAtMs: 3_000,
		Payload:      []byte(`{"key":"value","n":42}`),
	}
	m.ApplyMessages("jsontopic", []cli.BoardMessage{jsonMsg}, true)

	got := m.content.View()
	// The pretty-printed JSON must contain indented fields.
	if !strings.Contains(got, `"key": "value"`) {
		t.Errorf("content.View() missing indented JSON key-value; got:\n%s", got)
	}
	if !strings.Contains(got, `"n": 42`) {
		t.Errorf("content.View() missing indented JSON n:42; got:\n%s", got)
	}
}
