package tui

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestExecsModalStatsEventUpdatesTheRow(t *testing.T) {
	m := NewExecsModal()
	m.SetSize(200, 20)
	m.ApplySnapshot([]protocol.ExecRunInfo{{ExecId: 4}})
	m.ApplyEvent(protocol.ExecStatusEvent{Kind: protocol.StatusEventKind_ExecStats,
		Info: protocol.ExecRunInfo{ExecId: 4, StdoutBytes: 2048, Taps: 1}})
	row := m.table.Rows()[0]
	if row[4] != "2.0kB" || row[7] != "1" {
		t.Fatalf("row after stats: %v", row)
	}
	if len(m.table.Rows()) != 1 {
		t.Fatal("a stats event for a known exec must update, not append")
	}
}

func TestExecsModalRowPrintsZeroCounters(t *testing.T) {
	row := execRunInfoRow(&protocol.ExecRunInfo{ExecId: 4}, time.Now())
	if row[3] != "0" || row[5] != "0" || row[6] != "never" || row[7] != "0" {
		t.Fatalf("an idle exec must print zeros, not blanks: %v", row)
	}
}
