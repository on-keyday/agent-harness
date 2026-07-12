package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli"
)

// TestSnapshotTickRearms verifies that a snapshotTickMsg always yields a
// command (the re-armed timer) regardless of connection state, so the poll
// loop never dies once Init starts it.
func TestSnapshotTickRearms(t *testing.T) {
	a := New(Config{})

	_, cmd := a.Update(snapshotTickMsg{})
	if cmd == nil {
		t.Fatal("snapshotTickMsg must re-arm the tick even while disconnected")
	}

	a.connected = true
	a.client = &cli.Client{}
	_, cmd = a.Update(snapshotTickMsg{})
	if cmd == nil {
		t.Fatal("snapshotTickMsg must yield refresh+re-arm while connected")
	}
}

// TestShouldPeriodicRefresh verifies the gate: periodic refresh fires only
// with a live connection AND a bound client — otherwise every 5s tick would
// spam "snapshot: ..." errors into cmdresult during an outage.
func TestShouldPeriodicRefresh(t *testing.T) {
	a := New(Config{})
	if a.shouldPeriodicRefresh() {
		t.Error("must not refresh with no client and no connection")
	}
	a.connected = true
	if a.shouldPeriodicRefresh() {
		t.Error("must not refresh with no bound client")
	}
	a.client = &cli.Client{}
	if !a.shouldPeriodicRefresh() {
		t.Error("must refresh when connected with a bound client")
	}
	a.connected = false
	if a.shouldPeriodicRefresh() {
		t.Error("must not refresh while disconnected")
	}
}
