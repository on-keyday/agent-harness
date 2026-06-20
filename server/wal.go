package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// WALEvent is one append record. Only the fields relevant to the event type are populated.
//
// Selector serialization: protocol.RunnerSelector is a generated wire-format struct
// with an internal tagged-union field that does not marshal to useful JSON directly.
// We encode the selector as opaque base64-of-wire-bytes using RunnerSelector.MustAppend /
// RunnerSelector.DecodeExact. This is mechanical (no custom logic per variant) and
// round-trips perfectly. The JSON key is "selector_b64". Legacy WAL entries that
// pre-date this field decode as a zero RunnerSelector (Kind == RunnerSelectorKind_Any),
// which is the correct "any runner" default.
type WALEvent struct {
	Type        string `json:"type"` // "task_created" | "task_assigned" | "task_finished" | "task_cancelled" | "task_failed"
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
	OriginKind uint8 `json:"origin_kind,omitempty"`
	// ResumedByKind records the ClientKind of the most recent resumer.
	// Written on task_resumed events; legacy entries default to 0 (Unspecified).
	ResumedByKind uint8 `json:"resumed_by_kind,omitempty"`
	// CreatorTaskID is the hex-encoded task id of the agent principal that
	// created this task. Empty for operator-created tasks.
	// Written on task_created events; legacy entries default to "" (zero).
	CreatorTaskID string `json:"creator_task_id,omitempty"`
	// Capabilities is the bitmask stored at task_created time. Legacy WAL
	// entries without this field default to 0 (Capability_None).
	Capabilities uint32 `json:"capabilities,omitempty"`
	WorktreeDir  string `json:"worktree_dir,omitempty"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
	DiffInfo    []byte `json:"diff_info,omitempty"`
	// BoundRunnerID, when non-empty, pins the task to a specific runner.
	BoundRunnerID string `json:"bound_runner_id,omitempty"`
	// Reason holds a human-readable failure description (used by task_failed events).
	Reason string `json:"reason,omitempty"`
	// ExtraArgs are per-task CLI arguments forwarded verbatim to the runner.
	// Persisted on task_created so a server restart replaying the WAL re-creates
	// the queued task with the same per-task arg list.
	ExtraArgs []string `json:"extra_args,omitempty"`
	Ts        int64    `json:"ts"` // unix nano

	// Selector is the runner-selection constraint. It is not stored directly as
	// a JSON struct — see the selectorB64 field for the serialized form.
	// Populated by WALEvent.UnmarshalJSON and consumed by WALEvent.MarshalJSON.
	Selector protocol.RunnerSelector `json:"-"`
}

// walEventJSON is the over-the-wire representation of WALEvent used by
// MarshalJSON / UnmarshalJSON to add the base64 selector field alongside
// the other plain JSON fields.
type walEventJSON struct {
	Type          string   `json:"type"`
	TaskID        string   `json:"task_id,omitempty"`
	RunnerID      string   `json:"runner_id,omitempty"`
	RepoPath      string   `json:"repo_path,omitempty"`
	Prompt        string   `json:"prompt,omitempty"`
	Kind          uint8    `json:"kind,omitempty"`
	OriginKind    uint8    `json:"origin_kind,omitempty"`
	ResumedByKind uint8    `json:"resumed_by_kind,omitempty"`
	CreatorTaskID string   `json:"creator_task_id,omitempty"`
	Capabilities  uint32   `json:"capabilities,omitempty"`
	WorktreeDir   string   `json:"worktree_dir,omitempty"`
	ExitCode      *int32   `json:"exit_code,omitempty"`
	DiffInfo      []byte   `json:"diff_info,omitempty"`
	BoundRunnerID string   `json:"bound_runner_id,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	ExtraArgs     []string `json:"extra_args,omitempty"`
	Ts            int64    `json:"ts"`
	// SelectorB64 holds the base64-encoded wire bytes of the RunnerSelector.
	// Empty / absent means Kind == RunnerSelectorKind_Any (zero value).
	SelectorB64 string `json:"selector_b64,omitempty"`
}

// MarshalJSON encodes the Selector as base64 wire bytes and delegates the rest
// of the fields to the plain walEventJSON struct.
func (e WALEvent) MarshalJSON() ([]byte, error) {
	j := walEventJSON{
		Type:          e.Type,
		TaskID:        e.TaskID,
		RunnerID:      e.RunnerID,
		RepoPath:      e.RepoPath,
		Prompt:        e.Prompt,
		Kind:          e.Kind,
		OriginKind:    e.OriginKind,
		ResumedByKind: e.ResumedByKind,
		CreatorTaskID: e.CreatorTaskID,
		Capabilities:  e.Capabilities,
		WorktreeDir:   e.WorktreeDir,
		ExitCode:      e.ExitCode,
		DiffInfo:      e.DiffInfo,
		BoundRunnerID: e.BoundRunnerID,
		Reason:        e.Reason,
		ExtraArgs:     e.ExtraArgs,
		Ts:            e.Ts,
	}
	// Only encode the selector if it carries a non-Any kind (i.e. it has payload).
	if e.Selector.Kind != protocol.RunnerSelectorKind_Any {
		wire := e.Selector.MustAppend(nil)
		j.SelectorB64 = base64.StdEncoding.EncodeToString(wire)
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes the base64 selector wire bytes back into Selector and
// copies the remaining fields from walEventJSON.
func (e *WALEvent) UnmarshalJSON(b []byte) error {
	var j walEventJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	e.Type = j.Type
	e.TaskID = j.TaskID
	e.RunnerID = j.RunnerID
	e.RepoPath = j.RepoPath
	e.Prompt = j.Prompt
	e.Kind = j.Kind
	e.OriginKind = j.OriginKind
	e.ResumedByKind = j.ResumedByKind
	e.CreatorTaskID = j.CreatorTaskID
	e.Capabilities = j.Capabilities
	e.WorktreeDir = j.WorktreeDir
	e.ExitCode = j.ExitCode
	e.DiffInfo = j.DiffInfo
	e.BoundRunnerID = j.BoundRunnerID
	e.Reason = j.Reason
	e.ExtraArgs = j.ExtraArgs
	e.Ts = j.Ts

	if j.SelectorB64 != "" {
		wire, err := base64.StdEncoding.DecodeString(j.SelectorB64)
		if err != nil {
			return err
		}
		if err := e.Selector.DecodeExact(wire); err != nil {
			return err
		}
	}
	// If SelectorB64 is absent, e.Selector remains zero (Kind == Any). That is
	// the correct default for pre-3.1 WAL entries.
	return nil
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
