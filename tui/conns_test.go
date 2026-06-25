package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestConnsModalOpenedClosed verifies that ConnOpened adds a row and ConnClosed
// removes it, keyed by the CID string.
func TestConnsModalOpenedClosed(t *testing.T) {
	m := NewConnsModal()

	cid := []byte("conn-id-1")
	openedEv := protocol.ConnStatusEvent{Kind: protocol.StatusEventKind_ConnOpened}
	openedEv.Info.SetCid(cid)
	openedEv.Info.Role = protocol.ConnRole_Unspecified

	m.ApplyEvent(openedEv)
	if got := len(m.rowConns); got != 1 {
		t.Fatalf("after ConnOpened: want 1 row, got %d", got)
	}
	if string(m.rowConns[0].Cid) != string(cid) {
		t.Errorf("row cid: got %q want %q", m.rowConns[0].Cid, cid)
	}

	closedEv := protocol.ConnStatusEvent{Kind: protocol.StatusEventKind_ConnClosed}
	closedEv.Info.SetCid(cid)
	m.ApplyEvent(closedEv)
	if got := len(m.rowConns); got != 0 {
		t.Fatalf("after ConnClosed: want 0 rows, got %d", got)
	}
}

// TestConnsModalIdentifiedUpdatesRole verifies that ConnIdentified updates the
// role of an existing row in place.
func TestConnsModalIdentifiedUpdatesRole(t *testing.T) {
	m := NewConnsModal()

	cid := []byte("conn-id-2")
	openedEv := protocol.ConnStatusEvent{Kind: protocol.StatusEventKind_ConnOpened}
	openedEv.Info.SetCid(cid)
	openedEv.Info.Role = protocol.ConnRole_Unspecified
	m.ApplyEvent(openedEv)

	identEv := protocol.ConnStatusEvent{Kind: protocol.StatusEventKind_ConnIdentified}
	identEv.Info.SetCid(cid)
	identEv.Info.Role = protocol.ConnRole_Tui
	identEv.Info.SetIdentified(true)
	m.ApplyEvent(identEv)

	if got := len(m.rowConns); got != 1 {
		t.Fatalf("after ConnIdentified: want 1 row, got %d", got)
	}
	if m.rowConns[0].Role != protocol.ConnRole_Tui {
		t.Errorf("Role after identified: got=%v want=%v", m.rowConns[0].Role, protocol.ConnRole_Tui)
	}
	if !m.rowConns[0].Identified() {
		t.Error("Identified() should be true after ConnIdentified event")
	}
}

// TestConnsModalApplySnapshot verifies that ApplySnapshot populates rows and
// builds the CID index.
func TestConnsModalApplySnapshot(t *testing.T) {
	m := NewConnsModal()

	conns := make([]protocol.ConnInfo, 2)
	conns[0].SetCid([]byte("cid-a"))
	conns[0].Role = protocol.ConnRole_Cli
	conns[1].SetCid([]byte("cid-b"))
	conns[1].Role = protocol.ConnRole_Runner

	m.ApplySnapshot(conns)
	if got := len(m.rowConns); got != 2 {
		t.Fatalf("after ApplySnapshot: want 2 rows, got %d", got)
	}
	if _, ok := m.byCID["cid-a"]; !ok {
		t.Error("byCID missing cid-a")
	}
	if _, ok := m.byCID["cid-b"]; !ok {
		t.Error("byCID missing cid-b")
	}
}
