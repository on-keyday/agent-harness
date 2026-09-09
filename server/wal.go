package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// SelectorMigrated reports that Selector was reconstructed by
	// decodeWALSelector's legacy path rather than read straight off the wire
	// bytes. Derived from the record, never stored in one, so that a whole file
	// can say this once instead of every record saying it on every boot — which
	// is also why it is the single field TestWALEventJSONRoundTripCopiesEveryField
	// exempts.
	SelectorMigrated bool `json:"-"`
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
		sel, migrated, derr := decodeWALSelector(wire)
		if derr != nil {
			return derr
		}
		e.Selector, e.SelectorMigrated = sel, migrated
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

// WALDefect is one line ReadWAL could not turn into an event, with the line
// number because the reader's next move is to go look at that line: an error
// naming only "invalid character" points at a file with a hundred thousand of
// them.
type WALDefect struct {
	Line int
	Err  error
}

// WALReport is what one read of the WAL had to work around. Its zero value
// means the file was read exactly as written.
type WALReport struct {
	// Defects are the lines that were skipped. Their tasks are missing from the
	// returned events, so this is the operator-visible measure of how much
	// history a read could not account for.
	Defects []WALDefect
	// Migrated counts records whose stored selector needed decodeWALSelector's
	// legacy path — history that IS accounted for, under a re-typed selector arm.
	Migrated int
	// TruncatedAfter is non-zero when the scanner gave up mid-file, so every
	// line after it is missing regardless of what it contained.
	TruncatedAfter int
}

// Empty reports that the read needed no allowances at all.
func (r WALReport) Empty() bool {
	return len(r.Defects) == 0 && r.Migrated == 0 && r.TruncatedAfter == 0
}

// ReadWAL returns the events from path in order. Returns nil, zero, nil if path
// doesn't exist.
//
// A defect is LOCAL. One unreadable line costs that line and nothing else,
// which is the property this function did not have and needs: the WAL is the
// only record that a task ever existed, and it is read by a replay that decodes
// embedded WIRE bytes (WALEvent.Selector), so any future schema change to an
// embedded format reaches back into records written years earlier. When the
// failure was file-wide, exactly that happened — the opaque-RunnerID change made
// every legacy `--runner`-pinned record undecodable, and each one took the
// entire history with it: the server booted with an empty store and `restore`
// answered Unreadable, with all the bytes still on disk. The same shape had
// already cost the file once before, when a long prompt overran the scanner's
// 1 MiB token cap.
//
// So err never means "discard the events". It means the read did not complete —
// the file would not open, or an I/O error cut it short — and the events
// returned beside it are the history that WAS read. Every caller replays what
// it got and logs the rest; there is no path on which readable records on disk
// end up unreplayed, which is the whole point.
func ReadWAL(path string) ([]WALEvent, WALReport, error) {
	var report WALReport
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, report, nil
		}
		return nil, report, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// One line per event, and a task_created carries the whole PROMPT, so the
	// old 1 MiB cap made a single long-prompt submit unreadable. 64 MiB is past
	// anything a prompt reaches and still bounded; overrunning even that now
	// truncates the tail rather than discarding the file.
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
			report.Defects = append(report.Defects, WALDefect{Line: line, Err: fmt.Errorf("%s:%d: %w", path, line, err)})
			continue
		}
		if ev.SelectorMigrated {
			report.Migrated++
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		// Not recoverable line-by-line — the scanner cannot be advanced past
		// what it refused — so the tail is gone either way. The distinction
		// that matters is WHOSE fault it is: one oversized record is damage to
		// that record, while an I/O failure is the read not completing, and
		// only the second is something the caller can retry or repair.
		report.TruncatedAfter = line
		report.Defects = append(report.Defects, WALDefect{Line: line + 1, Err: fmt.Errorf("%s: after line %d: %w", path, line, err)})
		if !errors.Is(err, bufio.ErrTooLong) {
			return events, report, fmt.Errorf("%s: after line %d: %w", path, line, err)
		}
	}
	return events, report, nil
}

// LogTo writes the report as one line per concern, or nothing when the read
// needed no allowances. One place, because all four readers of the WAL owe the
// operator the same account of what a read could not do.
func (r WALReport) LogTo(log *slog.Logger, op, path string) {
	if log == nil || r.Empty() {
		return
	}
	if n := len(r.Defects); n > 0 {
		log.Error("WAL: records skipped as unreadable — their tasks are absent",
			"op", op, "path", path, "skipped", n, "first", r.Defects[0].Err)
	}
	if r.TruncatedAfter > 0 {
		log.Error("WAL: read stopped mid-file; every later record is absent",
			"op", op, "path", path, "after_line", r.TruncatedAfter)
	}
	if r.Migrated > 0 {
		log.Warn("WAL: legacy by_runner_id selectors re-read as by_conn_id",
			"op", op, "path", path, "migrated", r.Migrated)
	}
}
