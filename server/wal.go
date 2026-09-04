package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
// WALScopeOverride is one Scope.Overrides entry in JSON. Caps is the numeric
// mask so a capability RENAME never invalidates a record — the same reason
// Kind and OriginKind are numbers rather than names.
type WALScopeOverride struct {
	Caps        uint32   `json:"caps"`
	Base        uint8    `json:"base,omitempty"`
	ExcludeSelf bool     `json:"exclude_self,omitempty"`
	IDs         []string `json:"ids,omitempty"`
}

type WALEvent struct {
	Type     string `json:"type"` // "task_created" | "task_assigned" | "task_finished" | "task_cancelled" | "task_failed"
	TaskID   string `json:"task_id,omitempty"`
	RunnerID string `json:"runner_id,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
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
	// ScopeBase / ScopeIDs are the task's TaskScope (see server/scope.go),
	// written on task_created and task_caps_changed. A legacy entry has
	// neither key, so ScopeBase reads back as 0 — which is ScopeBase_Subtree,
	// the pre-scope behaviour. That is why subtree is the zero value.
	ScopeBase uint8    `json:"scope_base,omitempty"`
	ScopeIDs  []string `json:"scope_ids,omitempty"`
	// The axes added by the per-capability change. Every one of them replays
	// correctly from its JSON zero value, which is why the two flags are
	// phrased by-presence and negatively: a legacy record carries none of these
	// keys and must mean "visibility follows the base, self included".
	ScopeVisBase        uint8              `json:"scope_vis_base,omitempty"`
	ScopeVisBasePresent bool               `json:"scope_vis_base_present,omitempty"`
	ScopeExcludeSelf    bool               `json:"scope_exclude_self,omitempty"`
	ScopeVisIDs         []string           `json:"scope_vis_ids,omitempty"`
	ScopeOverrides      []WALScopeOverride `json:"scope_overrides,omitempty"`
	// AgentProfile is the resolved agent profile name for this task (see
	// TaskEntry.AgentProfile). Written on task_created events; legacy
	// entries default to "" (zero). Also reused by task_resumed events.
	AgentProfile string `json:"agent_profile,omitempty"`
	// SkillsInjected records whether the runner this task was ASSIGNED to
	// declares it injects .claude/{settings.json,skills} + .agents/skills.
	// Written on task_assigned; legacy entries default to false, which reads
	// as "unknown / not declared" rather than a positive "bare agent" claim.
	SkillsInjected bool   `json:"skills_injected,omitempty"`
	WorktreeDir    string `json:"worktree_dir,omitempty"`
	ExitCode       *int32 `json:"exit_code,omitempty"`
	DiffInfo       []byte `json:"diff_info,omitempty"`
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
	ScopeBase     uint8    `json:"scope_base,omitempty"`
	ScopeIDs      []string `json:"scope_ids,omitempty"`
	// Mirrors of the axes on WALEvent. This shadow struct is the reason
	// TestWALEventJSONRoundTripCopiesEveryField exists: a field added to
	// WALEvent and forgotten here is dropped on persistence, silently.
	ScopeVisBase        uint8              `json:"scope_vis_base,omitempty"`
	ScopeVisBasePresent bool               `json:"scope_vis_base_present,omitempty"`
	ScopeExcludeSelf    bool               `json:"scope_exclude_self,omitempty"`
	ScopeVisIDs         []string           `json:"scope_vis_ids,omitempty"`
	ScopeOverrides      []WALScopeOverride `json:"scope_overrides,omitempty"`
	AgentProfile        string             `json:"agent_profile,omitempty"`
	SkillsInjected      bool               `json:"skills_injected,omitempty"`
	WorktreeDir         string             `json:"worktree_dir,omitempty"`
	ExitCode            *int32             `json:"exit_code,omitempty"`
	DiffInfo            []byte             `json:"diff_info,omitempty"`
	BoundRunnerID       string             `json:"bound_runner_id,omitempty"`
	Reason              string             `json:"reason,omitempty"`
	ExtraArgs           []string           `json:"extra_args,omitempty"`
	Ts                  int64              `json:"ts"`
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
		ScopeBase:     e.ScopeBase,
		ScopeIDs:      e.ScopeIDs,

		ScopeVisBase:        e.ScopeVisBase,
		ScopeVisBasePresent: e.ScopeVisBasePresent,
		ScopeExcludeSelf:    e.ScopeExcludeSelf,
		ScopeVisIDs:         e.ScopeVisIDs,
		ScopeOverrides:      e.ScopeOverrides,
		AgentProfile:        e.AgentProfile,
		SkillsInjected:      e.SkillsInjected,
		WorktreeDir:         e.WorktreeDir,
		ExitCode:            e.ExitCode,
		DiffInfo:            e.DiffInfo,
		BoundRunnerID:       e.BoundRunnerID,
		Reason:              e.Reason,
		ExtraArgs:           e.ExtraArgs,
		Ts:                  e.Ts,
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
	e.ScopeBase = j.ScopeBase
	e.ScopeIDs = j.ScopeIDs
	e.ScopeVisBase = j.ScopeVisBase
	e.ScopeVisBasePresent = j.ScopeVisBasePresent
	e.ScopeExcludeSelf = j.ScopeExcludeSelf
	e.ScopeVisIDs = j.ScopeVisIDs
	e.ScopeOverrides = j.ScopeOverrides
	e.AgentProfile = j.AgentProfile
	e.SkillsInjected = j.SkillsInjected
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

	sc := bufio.NewScanner(f)
	// One line per event, and a task_created carries the whole PROMPT. At the
	// old 1 MiB cap a single long-prompt submit made the scanner fail, and
	// the failure is not local: it takes the WHOLE file with it, so replay
	// loses every task and `restore` reports nothing to put back forever.
	// 64 MiB is past anything a prompt reaches and still bounded.
	sc.Buffer(make([]byte, 1<<16), 64<<20)

	var events []WALEvent
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var ev WALEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			// The line number, because the caller's next move is to look at
			// that line: an error naming only "invalid character" points at a
			// file with a hundred thousand of them.
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: after line %d: %w", path, line, err)
	}
	return events, nil
}
