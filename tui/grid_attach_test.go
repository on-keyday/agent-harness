package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// Enter = control (takeover) attach; v = read-only view attach. Both close the
// grid and hand back an attach cmd; with no client both are no-ops.
func TestGridModel_EnterControl_vView(t *testing.T) {
	mk := func() GridModel {
		m := NewGridModel()
		m.panes = []*PaneStreamer{NewPaneStreamer("aaaaaaaa", 24, 80)}
		m.open = true
		m.client = &cli.Client{}
		m.SetSize(100, 30)
		return m
	}

	m := mk()
	m2, cmd := m.Update(keyMsg("enter"))
	if m2.open {
		t.Fatal("Enter should close the grid")
	}
	if cmd == nil {
		t.Fatal("Enter should return an attach cmd (control)")
	}

	m = mk()
	m3, cmd3 := m.Update(keyMsg("v"))
	if m3.open {
		t.Fatal("v should close the grid")
	}
	if cmd3 == nil {
		t.Fatal("v should return an attach cmd (view)")
	}

	m = mk()
	m.client = nil
	m4, cmd4 := m.Update(keyMsg("enter"))
	if !m4.open {
		t.Fatal("Enter with no client should keep the grid open")
	}
	if cmd4 != nil {
		t.Fatal("Enter with no client should return nil cmd")
	}
}
