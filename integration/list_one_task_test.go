//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SnapshotTask (the List naming one task, which whoami reads its own grant
// conditions through) gets that task's row alone across a real server, and no
// row for an id the server does not hold.
func TestIntegrationSnapshotTaskReturnsOneRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	serverCID := startServer(t)
	c := dialClient(t, serverCID)
	repo := tempRepo(t)
	startRunner(t, serverCID, runnerOpts{MaxTasks: 2, Roots: []string{repo}, AgentBin: fakeClaudePath(t)})
	first := mustOpenOneshot(t, c, repo, "first")
	_ = mustOpenOneshot(t, c, repo, "second")

	raw, _ := hex.DecodeString(first)
	var id protocol.TaskID
	copy(id.Id[:], raw)
	row, ok, err := c.SnapshotTask(context.Background(), id)
	if err != nil {
		t.Fatalf("SnapshotTask: %v", err)
	}
	if !ok || row.Id != id {
		t.Fatalf("SnapshotTask(%s): ok=%v id=%x, want its row", first[:12], ok, row.Id.Id)
	}

	var unknown protocol.TaskID
	unknown.Id[0] = 0xff
	if _, ok, err := c.SnapshotTask(context.Background(), unknown); err != nil || ok {
		t.Fatalf("SnapshotTask(unknown): ok=%v err=%v, want no row and no error", ok, err)
	}
}
