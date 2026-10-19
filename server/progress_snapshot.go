package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// progressSnapshotPath is where a stopping server leaves every task's progress
// line for its successor, beside the held captures and for the same reason:
// it is state handed to the next server, not history. A WAL record per write
// would grow the log by every line an agent ever wrote; this file is
// rewritten whole on each deliberate stop and removed once read.
func progressSnapshotPath(dataDir string) string {
	return filepath.Join(dataDir, "progress.json")
}

// writeProgressSnapshot writes every task's line, cleared ones included (a
// clear is a write with its own stamp). Through a temp file and a rename, so
// a stop that dies mid-write leaves the previous file or none — never half a
// file. Called on a deliberate stop only; a crash writes nothing and the lines
// are lost, which is the accepted cost.
func writeProgressSnapshot(dataDir string, store *TaskStore) error {
	lines := store.progressLines()
	path := progressSnapshotPath(dataDir)
	if len(lines) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadProgressSnapshot applies the previous server's lines to the tasks the
// WAL replay rebuilt, then removes the file: left in place, a later crash
// would resurrect lines that have since changed. It reports how many lines
// found their task; a task pruned meanwhile is simply skipped. No file is not
// an error — the previous stop was a crash, or nothing had written.
func loadProgressSnapshot(dataDir string, store *TaskStore) (int, error) {
	path := progressSnapshotPath(dataDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer os.Remove(path)
	var lines []progressLine
	if err := json.Unmarshal(b, &lines); err != nil {
		return 0, err
	}
	n := 0
	for _, l := range lines {
		if store.restoreProgress(l) {
			n++
		}
	}
	return n, nil
}
