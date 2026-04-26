package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// WALEvent is one append record. Only the fields relevant to the event type are populated.
type WALEvent struct {
	Type        string `json:"type"` // "task_created" | "task_assigned" | "task_finished" | "task_cancelled"
	TaskID      string `json:"task_id,omitempty"`
	RunnerID    string `json:"runner_id,omitempty"`
	RepoPath    string `json:"repo_path,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	// Kind distinguishes oneshot vs interactive tasks. Encoded as the
	// numeric protocol.TaskKind value so the wire format is stable across
	// schema renames. 0 (oneshot) is the default for legacy WAL entries
	// that pre-date this field.
	Kind uint8 `json:"kind,omitempty"`
	// OriginKind records which kind of client (cli / tui / webui) submitted
	// the task. Encoded as the numeric protocol.ClientKind. Legacy WAL
	// entries that pre-date this field default to 0 (Unspecified) on
	// replay, which is the intended sentinel for "unknown origin".
	OriginKind  uint8  `json:"origin_kind,omitempty"`
	WorktreeDir string `json:"worktree_dir,omitempty"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
	DiffInfo    []byte `json:"diff_info,omitempty"`
	Ts          int64  `json:"ts"` // unix nano
}

// WAL is a write-ahead log that appends events as JSONL to a file.
type WAL struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// OpenWAL opens (creating if necessary) the WAL file at path in append mode.
func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{
		f: f,
		w: bufio.NewWriter(f),
	}, nil
}

// Write appends one event as JSON + newline. Flushes immediately so a crash loses at most the last in-flight write.
func (wal *WAL) Write(ev WALEvent) error {
	if ev.Ts == 0 {
		ev.Ts = time.Now().UnixNano()
	}
	wal.mu.Lock()
	defer wal.mu.Unlock()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := wal.w.Write(b); err != nil {
		return err
	}
	return wal.w.Flush()
}

// Close flushes buffered data and closes the underlying file.
func (wal *WAL) Close() error {
	wal.mu.Lock()
	defer wal.mu.Unlock()
	if err := wal.w.Flush(); err != nil {
		return err
	}
	return wal.f.Close()
}

// ReadWAL returns all events from path in order. Returns nil, nil if path doesn't exist.
func ReadWAL(path string) ([]WALEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)

	var events []WALEvent
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev WALEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
