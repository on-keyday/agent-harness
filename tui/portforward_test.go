package tui

import "testing"

func TestPortForwardModal_OpenClose(t *testing.T) {
	var m PortForwardModal
	if m.IsOpen() {
		t.Fatal("new modal should be closed")
	}
	m.Open("abc123")
	if !m.IsOpen() || m.TaskID() != "abc123" {
		t.Fatalf("after Open: open=%v task=%q", m.IsOpen(), m.TaskID())
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("after Close: should be closed")
	}
}
