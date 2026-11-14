package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// historyMax is how many history entries a schedule keeps (S-D7).
const historyMax = 50

const scheduleFileVersion = 1

var ErrScheduleNotFound = errors.New("no such schedule")

// Schedule is one server-kept cron schedule (pillar S, "The schedule"). Times
// are UTC instants; the zone named by TZ is applied only to compute the next
// firing and to display. State is protocol.ScheduleTaskState (S-D21): the one
// type for the concept, not a second one in this package.
type Schedule struct {
	ID                  string // 32 hex
	Cron, TZ            string
	State               protocol.ScheduleTaskState
	TaskID              string // bound task, or the pre-chosen id of a pending creation
	Repo, Agent         string
	Selector            protocol.RunnerSelector
	Prompt              string
	ExtraArgs           []string
	ResumeConversation  bool
	Run                 RunSpec // Mode is always Background
	Grant               authority.Grant
	Creator             string // task hex, "" = operator
	CreatedAt, EditedAt time.Time
	EditedBy            string
	NextAt              time.Time
	History             []HistoryEntry // oldest first, len <= historyMax
}

// HistoryEntry is one firing's record (S-D7).
type HistoryEntry struct {
	Outcome                      protocol.ScheduleOutcome
	DueAt, At                    time.Time
	ExpectRun                    uint32 // S-D22: the task's Run after this firing's resume
	TaskID                       string // the task the entry is about (bound or pre-chosen)
	QueuedAt, StartedAt, EndedAt time.Time
	EndReason                    protocol.EndReason
	ByKind                       protocol.ClientKind
	By                           string
	Message                      string
}

// appendHistory adds e and evicts the oldest entries past historyMax.
func (s *Schedule) appendHistory(e HistoryEntry) {
	s.History = append(s.History, e)
	if n := len(s.History) - historyMax; n > 0 {
		s.History = append([]HistoryEntry(nil), s.History[n:]...)
	}
}

// ScheduleStore is the set of schedules, persisted as <data-dir>/schedules.json
// (S-D4). Not in the WAL.
type ScheduleStore struct {
	mu   sync.Mutex
	path string // "" = memory only
	m    map[string]Schedule
}

// LoadScheduleStore reads path (absent = empty). A file that does not parse,
// or has another version, is an error: starting empty would drop every
// schedule silently. A schedule whose zone no longer loads is NOT an error:
// the name is kept and the firing reports it.
func LoadScheduleStore(path string) (*ScheduleStore, error) {
	st := &ScheduleStore{path: path, m: map[string]Schedule{}}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("schedules %s: %w", path, err)
	}
	var f scheduleFileJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("schedules %s: %w", path, err)
	}
	if f.Version != scheduleFileVersion {
		return nil, fmt.Errorf("schedules %s: version %d is not supported (this server reads version %d)", path, f.Version, scheduleFileVersion)
	}
	for _, j := range f.Schedules {
		s, err := j.schedule()
		if err != nil {
			return nil, fmt.Errorf("schedules %s: schedule %q: %w", path, j.ID, err)
		}
		if s.ID == "" {
			return nil, fmt.Errorf("schedules %s: a schedule has no id", path)
		}
		st.m[s.ID] = s
	}
	return st, nil
}

func (st *ScheduleStore) Get(id string) (Schedule, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.m[id]
	return s.clone(), ok
}

// List returns every schedule, oldest created first.
func (st *ScheduleStore) List() []Schedule {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.listLocked()
}

func (st *ScheduleStore) listLocked() []Schedule {
	out := make([]Schedule, 0, len(st.m))
	for _, s := range st.m {
		out = append(out, s.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Put creates or replaces s, then saves. A failed save leaves the store as it was.
func (st *ScheduleStore) Put(s Schedule) error {
	if s.ID == "" {
		return errors.New("schedule has no id")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, had := st.m[s.ID]
	st.m[s.ID] = s.clone()
	if err := st.saveLocked(); err != nil {
		st.restoreLocked(s.ID, prev, had)
		return err
	}
	return nil
}

func (st *ScheduleStore) Delete(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, ok := st.m[id]
	if !ok {
		return ErrScheduleNotFound
	}
	delete(st.m, id)
	if err := st.saveLocked(); err != nil {
		st.restoreLocked(id, prev, true)
		return err
	}
	return nil
}

// Update runs f on the schedule under the lock, then saves. An error from f,
// or from the save, leaves the stored schedule unchanged. f may not change ID.
func (st *ScheduleStore) Update(id string, f func(*Schedule) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, ok := st.m[id]
	if !ok {
		return ErrScheduleNotFound
	}
	cur := prev.clone()
	if err := f(&cur); err != nil {
		return err
	}
	if cur.ID != id {
		return errors.New("schedule update changed the id")
	}
	st.m[id] = cur
	if err := st.saveLocked(); err != nil {
		st.restoreLocked(id, prev, true)
		return err
	}
	return nil
}

// EarliestNext is the soonest NextAt among schedules that have one.
func (st *ScheduleStore) EarliestNext() (time.Time, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var best time.Time
	found := false
	for _, s := range st.m {
		if s.NextAt.IsZero() {
			continue
		}
		if !found || s.NextAt.Before(best) {
			best, found = s.NextAt, true
		}
	}
	return best, found
}

// Settle resolves every pending history entry from the task store's view
// (S-D22). task reports a task's persisted Run and whether it exists.
//
//   - A schedule still pending_create: the entry is a creation. The pre-chosen
//     id existing means the creation happened: fired, and the schedule is
//     bound to it. Absent: lost, and the state stays pending_create.
//   - Otherwise the entry is a resume: fired when the task's Run reached
//     ExpectRun, lost when it did not (or the task is gone).
func (st *ScheduleStore) Settle(task func(id string) (run uint32, exists bool)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := false
	for id, s := range st.m {
		var upd *Schedule
		for i := range s.History {
			e := &s.History[i]
			if e.Outcome != protocol.ScheduleOutcome_Pending {
				continue
			}
			if upd == nil {
				c := s.clone()
				upd = &c
				e = &upd.History[i]
			}
			run, exists := task(e.TaskID)
			switch {
			case upd.State == protocol.ScheduleTaskState_PendingCreate:
				if exists {
					e.Outcome = protocol.ScheduleOutcome_Fired
					upd.State = protocol.ScheduleTaskState_Bound
					upd.TaskID = e.TaskID
				} else {
					e.Outcome = protocol.ScheduleOutcome_Lost
				}
			case exists && run >= e.ExpectRun:
				e.Outcome = protocol.ScheduleOutcome_Fired
			default:
				e.Outcome = protocol.ScheduleOutcome_Lost
			}
		}
		if upd != nil {
			st.m[id] = *upd
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return st.saveLocked()
}

func (st *ScheduleStore) restoreLocked(id string, prev Schedule, had bool) {
	if had {
		st.m[id] = prev
	} else {
		delete(st.m, id)
	}
}

func (st *ScheduleStore) saveLocked() error {
	if st.path == "" {
		return nil
	}
	f := scheduleFileJSON{Version: scheduleFileVersion, Schedules: []scheduleJSON{}}
	for _, s := range st.listLocked() {
		j, err := s.toJSON()
		if err != nil {
			return fmt.Errorf("schedules: %w", err)
		}
		f.Schedules = append(f.Schedules, j)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	return writeFileAtomic(st.path, append(data, '\n'))
}

// writeFileAtomic writes data to path+".tmp" (0600), fsyncs it, renames it
// over path and fsyncs the directory, so a crash leaves the old or the new
// file, never a torn one.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("schedules: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("schedules: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil // a directory cannot be opened for Sync there
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	return nil
}

// clone copies the slices a caller could otherwise share with the store.
func (s Schedule) clone() Schedule {
	s.ExtraArgs = append([]string(nil), s.ExtraArgs...)
	if s.ExtraArgs != nil && len(s.ExtraArgs) == 0 {
		s.ExtraArgs = nil
	}
	if s.History != nil {
		s.History = append([]HistoryEntry(nil), s.History...)
	}
	s.Grant.Scope = cloneScope(s.Grant.Scope)
	s.Grant.DelegateScope = cloneScope(s.Grant.DelegateScope)
	return s
}

func cloneScope(sc authority.Scope) authority.Scope {
	if sc.IDs != nil {
		sc.IDs = append([]string(nil), sc.IDs...)
	}
	if sc.VisIDs != nil {
		sc.VisIDs = append([]string(nil), sc.VisIDs...)
	}
	if sc.Overrides != nil {
		o := make([]authority.ScopeOverride, len(sc.Overrides))
		for i, v := range sc.Overrides {
			if v.IDs != nil {
				v.IDs = append([]string(nil), v.IDs...)
			}
			o[i] = v
		}
		sc.Overrides = o
	}
	return sc
}

// The JSON shape. RunnerSelector (private variant) and SpawnLimit (private
// field) do not survive encoding/json, so the file carries the selector as
// its wire bytes in hex and each spawn limit as {limited, value}. Everything
// else marshals as is. Times are written as UTC instants.

type scheduleFileJSON struct {
	Version   int            `json:"version"`
	Schedules []scheduleJSON `json:"schedules"`
}

type spawnLimitJSON struct {
	Limited bool   `json:"limited"`
	Value   uint32 `json:"value"`
}

type grantJSON struct {
	Caps           protocol.Capability `json:"caps"`
	Scope          authority.Scope     `json:"scope"`
	DelegateCaps   protocol.Capability `json:"delegate_caps"`
	DelegateScope  authority.Scope     `json:"delegate_scope"`
	SpawnDepth     spawnLimitJSON      `json:"spawn_depth"`
	MaxDescendants spawnLimitJSON      `json:"max_descendants"`
}

type scheduleJSON struct {
	ID                 string                     `json:"id"`
	Cron               string                     `json:"cron"`
	TZ                 string                     `json:"tz"`
	State              protocol.ScheduleTaskState `json:"state"`
	TaskID             string                     `json:"task_id"`
	Repo               string                     `json:"repo"`
	Agent              string                     `json:"agent"`
	Selector           string                     `json:"selector"` // hex of the wire encoding
	Prompt             string                     `json:"prompt"`
	ExtraArgs          []string                   `json:"extra_args"`
	ResumeConversation bool                       `json:"resume_conversation"`
	Run                RunSpec                    `json:"run"`
	Grant              grantJSON                  `json:"grant"`
	Creator            string                     `json:"creator"`
	CreatedAt          time.Time                  `json:"created_at"`
	EditedAt           time.Time                  `json:"edited_at"`
	EditedBy           string                     `json:"edited_by"`
	NextAt             time.Time                  `json:"next_at"`
	History            []HistoryEntry             `json:"history"`
}

func limitToJSON(l protocol.SpawnLimit) spawnLimitJSON {
	return spawnLimitJSON{Limited: l.Limited(), Value: l.Value()}
}

func limitFromJSON(j spawnLimitJSON) protocol.SpawnLimit {
	if !j.Limited {
		return authority.Unbounded()
	}
	return authority.LimitOf(j.Value)
}

func (s Schedule) toJSON() (scheduleJSON, error) {
	sel, err := s.Selector.Append(nil)
	if err != nil {
		return scheduleJSON{}, fmt.Errorf("schedule %s selector: %w", s.ID, err)
	}
	utc := func(t time.Time) time.Time { return t.UTC() }
	j := scheduleJSON{
		ID: s.ID, Cron: s.Cron, TZ: s.TZ, State: s.State, TaskID: s.TaskID,
		Repo: s.Repo, Agent: s.Agent, Selector: hex.EncodeToString(sel),
		Prompt: s.Prompt, ExtraArgs: s.ExtraArgs, ResumeConversation: s.ResumeConversation,
		Run: s.Run,
		Grant: grantJSON{
			Caps: s.Grant.Caps, Scope: s.Grant.Scope,
			DelegateCaps: s.Grant.DelegateCaps, DelegateScope: s.Grant.DelegateScope,
			SpawnDepth: limitToJSON(s.Grant.SpawnDepth), MaxDescendants: limitToJSON(s.Grant.MaxDescendants),
		},
		Creator: s.Creator, CreatedAt: utc(s.CreatedAt), EditedAt: utc(s.EditedAt),
		EditedBy: s.EditedBy, NextAt: utc(s.NextAt),
	}
	for _, e := range s.History {
		e.DueAt, e.At = utc(e.DueAt), utc(e.At)
		e.QueuedAt, e.StartedAt, e.EndedAt = utc(e.QueuedAt), utc(e.StartedAt), utc(e.EndedAt)
		j.History = append(j.History, e)
	}
	return j, nil
}

func (j scheduleJSON) schedule() (Schedule, error) {
	raw, err := hex.DecodeString(j.Selector)
	if err != nil {
		return Schedule{}, fmt.Errorf("selector: %w", err)
	}
	var sel protocol.RunnerSelector
	if err := sel.DecodeExact(raw); err != nil {
		return Schedule{}, fmt.Errorf("selector: %w", err)
	}
	return Schedule{
		ID: j.ID, Cron: j.Cron, TZ: j.TZ, State: j.State, TaskID: j.TaskID,
		Repo: j.Repo, Agent: j.Agent, Selector: sel,
		Prompt: j.Prompt, ExtraArgs: j.ExtraArgs, ResumeConversation: j.ResumeConversation,
		Run: j.Run,
		Grant: authority.Grant{
			Caps: j.Grant.Caps, Scope: j.Grant.Scope,
			DelegateCaps: j.Grant.DelegateCaps, DelegateScope: j.Grant.DelegateScope,
			SpawnDepth: limitFromJSON(j.Grant.SpawnDepth), MaxDescendants: limitFromJSON(j.Grant.MaxDescendants),
		},
		Creator: j.Creator, CreatedAt: j.CreatedAt, EditedAt: j.EditedAt,
		EditedBy: j.EditedBy, NextAt: j.NextAt, History: j.History,
	}, nil
}
