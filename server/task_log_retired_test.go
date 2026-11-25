package server

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestPruneLeavesOldLogFilesAlone: the task log was retired in A3 (A-D30),
// and the files a server wrote under <data-dir>/logs/ before then stay on
// disk. Prune forgets the task record; it does not delete its old log, in
// either mode.
func TestPruneLeavesOldLogFilesAlone(t *testing.T) {
	dataDir := t.TempDir()
	s := New(Config{DataDir: dataDir})
	logs := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	byID, byAge := runNumberTask(t, s.tasks), runNumberTask(t, s.tasks)
	for _, id := range []string{byID, byAge} {
		s.tasks.Finish(id, 0, nil)
		if err := os.WriteFile(filepath.Join(logs, id+".log"), []byte("[out]old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var tid protocol.TaskID
	raw, _ := hex.DecodeString(byID)
	copy(tid.Id[:], raw)
	if removed, _, _ := s.taskHandler.PruneFn(nil, &protocol.PruneTasksRequest{TaskIdsLen: 1, TaskIds: []protocol.TaskID{tid}}); removed != 1 {
		t.Fatalf("prune by id removed %d, want 1", removed)
	}
	if removed, _, _ := s.taskHandler.PruneFn(nil, &protocol.PruneTasksRequest{BeforeTs: uint64(time.Now().Add(time.Hour).UnixNano())}); removed != 1 {
		t.Fatalf("prune by age removed %d, want 1", removed)
	}
	for _, id := range []string{byID, byAge} {
		if _, err := os.Stat(filepath.Join(logs, id+".log")); err != nil {
			t.Errorf("%s.log after prune: %v (nothing deletes old logs)", id, err)
		}
	}
}
