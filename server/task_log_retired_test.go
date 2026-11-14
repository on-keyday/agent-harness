package server

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
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

// TestGetTaskLogAnswersNoLog: get_task_log stays a valid kind after A3, so an
// older `harness-cli logs` gets an answer instead of silence: "no log", for
// every caller and every id.
func TestGetTaskLogAnswersNoLog(t *testing.T) {
	h := newTestHandler(t)
	id := runNumberTask(t, h.Tasks)
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(id)
	copy(tid.Id[:], raw)

	// No principal: an operator, which every gate would admit. A stream id is
	// available, so a found=1 answer could be built if the handler tried.
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9840-1"), nextSendStreamID: 3}
	req := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_GetTaskLog, RequestId: 9}}
	req.SetGetLog(protocol.GetTaskLogRequest{TaskId: tid})
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	if resp.Kind != protocol.TaskControlKind_GetTaskLog || resp.RequestId != 9 {
		t.Fatalf("answered kind=%v request=%d, want get_task_log for request 9", resp.Kind, resp.RequestId)
	}
	if gl := resp.GetLog(); gl == nil || gl.Found != 0 || gl.StreamId != 0 {
		t.Fatalf("get_task_log answered %+v, want found=0 stream=0", gl)
	}
	if len(conn.sendStreams) != 0 {
		t.Errorf("get_task_log opened %d streams, want none", len(conn.sendStreams))
	}
}
