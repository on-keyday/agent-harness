package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func fullSchedule(t *testing.T, id string, n int) Schedule {
	t.Helper()
	var sel protocol.RunnerSelector
	sel.Kind = protocol.RunnerSelectorKind_ByHostname
	var hn protocol.Hostname
	if !hn.SetName([]byte("box")) {
		t.Fatal("SetName")
	}
	if !sel.SetHostname(hn) {
		t.Fatal("SetHostname")
	}
	base := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	s := Schedule{
		ID: id, Cron: "*/5 * * * *", TZ: "Asia/Tokyo",
		State: protocol.ScheduleTaskState_Bound, TaskID: strings.Repeat("ab", 16),
		Repo: "r", Agent: "claude", Selector: sel, Prompt: "do it", ExtraArgs: []string{"--x", "y"},
		ResumeConversation: true,
		Run: RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{
			Deadline: time.Hour, Quiet: 3 * time.Minute, Blocked: 4 * time.Minute, QueueTimeout: 90 * time.Second,
			TurnEnd: protocol.TurnEndSignal_Lifecycle, Rows: 24, Cols: 80,
		}, Record: true, Oneshot: true, NoPTY: true},
		Grant: authority.Grant{
			Caps: protocol.Capability_FileRead | protocol.Capability_Schedule,
			Scope: authority.Scope{Base: protocol.ScopeBase_Global, IDs: []string{strings.Repeat("cd", 16)},
				VisBase: protocol.ScopeBase_None, VisBasePresent: true, ExcludeSelf: true,
				VisIDs:    []string{strings.Repeat("ef", 16)},
				Overrides: []authority.ScopeOverride{{Caps: protocol.Capability_FileRead, Base: protocol.ScopeBase_None, ExcludeSelf: true, IDs: []string{strings.Repeat("01", 16)}}}},
			DelegateCaps:   protocol.Capability_FileRead,
			DelegateScope:  authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{strings.Repeat("23", 16)}},
			SpawnDepth:     authority.LimitOf(3),
			MaxDescendants: authority.LimitOf(7),
		},
		Creator: strings.Repeat("45", 16), CreatedAt: base, EditedAt: base.Add(time.Minute), EditedBy: "operator",
		NextAt: base.Add(2 * time.Minute),
		History: []HistoryEntry{{
			Outcome: protocol.ScheduleOutcome_ResumedBy, DueAt: base, At: base.Add(time.Second), ExpectRun: 4,
			TaskID: strings.Repeat("ab", 16), QueuedAt: base.Add(2 * time.Second), StartedAt: base.Add(3 * time.Second),
			EndedAt: base.Add(4 * time.Second), EndReason: protocol.EndReason_Deadline,
			ByKind: protocol.ClientKind_Webui, By: "someone", Message: "m",
		}},
	}
	return s
}

func scheduleID(n int) string { return fmt.Sprintf("%032x", n) }

func TestScheduleStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := fullSchedule(t, scheduleID(1), 0), fullSchedule(t, scheduleID(2), 1)
	if err := st.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(b); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Schedule{a, b} {
		got, ok := st2.Get(want.ID)
		if !ok {
			t.Fatalf("%s missing after reload", want.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("schedule %s changed over the file:\n got %+v\nwant %+v", want.ID, got, want)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) || !got.NextAt.Equal(want.NextAt) {
			t.Errorf("times changed")
		}
	}
	if len(st2.List()) != 2 {
		t.Fatalf("List = %d", len(st2.List()))
	}
	if err := st2.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if st3, _ := LoadScheduleStore(path); len(st3.List()) != 1 {
		t.Fatal("delete not persisted")
	}
	if err := st2.Delete(a.ID); err != ErrScheduleNotFound {
		t.Fatalf("second delete = %v", err)
	}
}

func TestScheduleStoreUpdateRollsBack(t *testing.T) {
	st, _ := LoadScheduleStore("")
	a := fullSchedule(t, scheduleID(1), 0)
	_ = st.Put(a)
	if err := st.Update(a.ID, func(s *Schedule) error { s.Prompt = "changed"; return fmt.Errorf("no") }); err == nil {
		t.Fatal("want f's error")
	}
	if got, _ := st.Get(a.ID); got.Prompt != a.Prompt {
		t.Fatal("failed update leaked")
	}
	_ = st.Update(a.ID, func(s *Schedule) error { s.Prompt = "changed"; return nil })
	if got, _ := st.Get(a.ID); got.Prompt != "changed" {
		t.Fatal("update lost")
	}
	if at, ok := st.EarliestNext(); !ok || !at.Equal(a.NextAt) {
		t.Fatalf("EarliestNext = %v %v", at, ok)
	}
}

func TestScheduleStoreCorruptRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"schedules":[{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScheduleStore(path); err == nil {
		t.Fatal("truncated file loaded")
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"schedules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadScheduleStore(path)
	if err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("version 2: err = %v", err)
	}
}

func TestScheduleStoreFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, _ := LoadScheduleStore(path)
	if err := st.Put(fullSchedule(t, scheduleID(1), 0)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestHistoryEvictsOldest(t *testing.T) {
	var s Schedule
	for i := 0; i < historyMax+1; i++ {
		s.appendHistory(HistoryEntry{ExpectRun: uint32(i)})
	}
	if len(s.History) != historyMax {
		t.Fatalf("len %d", len(s.History))
	}
	for i, e := range s.History {
		if e.ExpectRun != uint32(i+1) {
			t.Fatalf("entry %d has run %d", i, e.ExpectRun)
		}
	}
}

func TestSettlePending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, _ := LoadScheduleStore(path)
	mk := func(n int, state protocol.ScheduleTaskState, taskID string, expect uint32) Schedule {
		s := fullSchedule(t, scheduleID(n), n)
		s.State, s.TaskID = state, taskID
		s.History = []HistoryEntry{{Outcome: protocol.ScheduleOutcome_Pending, TaskID: taskID, ExpectRun: expect}}
		return s
	}
	ta, tb, tc, td := strings.Repeat("a1", 16), strings.Repeat("b2", 16), strings.Repeat("c3", 16), strings.Repeat("d4", 16)
	for _, s := range []Schedule{
		mk(1, protocol.ScheduleTaskState_Bound, ta, 3),
		mk(2, protocol.ScheduleTaskState_Bound, tb, 3),
		mk(3, protocol.ScheduleTaskState_PendingCreate, tc, 0),
		mk(4, protocol.ScheduleTaskState_PendingCreate, td, 0),
	} {
		if err := st.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	tasks := map[string]uint32{ta: 3, tb: 2, tc: 1}
	if err := st.Settle(func(id string) (uint32, bool) { r, ok := tasks[id]; return r, ok }); err != nil {
		t.Fatal(err)
	}
	st2, _ := LoadScheduleStore(path) // the settlement is on disk
	check := func(n int, out protocol.ScheduleOutcome, state protocol.ScheduleTaskState) {
		t.Helper()
		s, _ := st2.Get(scheduleID(n))
		if s.History[0].Outcome != out || s.State != state {
			t.Errorf("schedule %d: outcome %v state %v, want %v %v", n, s.History[0].Outcome, s.State, out, state)
		}
	}
	check(1, protocol.ScheduleOutcome_Fired, protocol.ScheduleTaskState_Bound)
	check(2, protocol.ScheduleOutcome_Lost, protocol.ScheduleTaskState_Bound)
	check(3, protocol.ScheduleOutcome_Fired, protocol.ScheduleTaskState_Bound)
	check(4, protocol.ScheduleOutcome_Lost, protocol.ScheduleTaskState_PendingCreate)
	if s, _ := st2.Get(scheduleID(3)); s.TaskID != tc {
		t.Errorf("schedule 3 bound to %q", s.TaskID)
	}
}

func TestUnloadableZoneDoesNotBlockStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st, _ := LoadScheduleStore(path)
	s := fullSchedule(t, scheduleID(1), 0)
	s.TZ = "Mars/Olympus"
	if err := st.Put(s); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatalf("an unloadable zone blocked start: %v", err)
	}
	if got, _ := st2.Get(s.ID); got.TZ != "Mars/Olympus" {
		t.Fatalf("zone name not kept: %q", got.TZ)
	}
}

func TestSettleEveryPendingEntry(t *testing.T) {
	st, _ := LoadScheduleStore("")
	ta, tb := strings.Repeat("a1", 16), strings.Repeat("b2", 16)
	pend := func(task string, run uint32) HistoryEntry {
		return HistoryEntry{Outcome: protocol.ScheduleOutcome_Pending, TaskID: task, ExpectRun: run}
	}
	// Bound: two pending resumes, the task's Run reached the second.
	s1 := fullSchedule(t, scheduleID(1), 0)
	s1.State, s1.TaskID = protocol.ScheduleTaskState_Bound, ta
	s1.History = []HistoryEntry{pend(ta, 2), pend(ta, 3)}
	// Bound: the task reached the first only.
	s2 := fullSchedule(t, scheduleID(2), 1)
	s2.State, s2.TaskID = protocol.ScheduleTaskState_Bound, ta
	s2.History = []HistoryEntry{pend(ta, 2), pend(ta, 3), pend(ta, 4)}
	// PendingCreate with two creations: the first id exists, the second does not.
	// The second is judged against its own recorded id, not as a resume of the
	// task the first one just bound.
	s3 := fullSchedule(t, scheduleID(3), 2)
	s3.State, s3.TaskID = protocol.ScheduleTaskState_PendingCreate, tb
	s3.History = []HistoryEntry{pend(ta, 0), pend(tb, 0)}
	for _, s := range []Schedule{s1, s2, s3} {
		if err := st.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	tasks := map[string]uint32{ta: 3}
	if err := st.Settle(func(id string) (uint32, bool) { r, ok := tasks[id]; return r, ok }); err != nil {
		t.Fatal(err)
	}
	out := func(n int) (o []protocol.ScheduleOutcome) {
		s, _ := st.Get(scheduleID(n))
		for _, e := range s.History {
			o = append(o, e.Outcome)
		}
		return
	}
	F, L := protocol.ScheduleOutcome_Fired, protocol.ScheduleOutcome_Lost
	for n, want := range map[int][]protocol.ScheduleOutcome{1: {F, F}, 2: {F, F, L}, 3: {F, L}} {
		if got := out(n); !reflect.DeepEqual(got, want) {
			t.Errorf("schedule %d outcomes %v, want %v", n, got, want)
		}
	}
	if s, _ := st.Get(scheduleID(3)); s.State != protocol.ScheduleTaskState_Bound || s.TaskID != ta {
		t.Errorf("schedule 3: state %v task %q, want Bound to the first id that exists", s.State, s.TaskID)
	}
}

func TestScheduleStoreFailedUpdateDoesNotLeakSelector(t *testing.T) {
	st, _ := LoadScheduleStore("")
	a := fullSchedule(t, scheduleID(1), 0)
	want, err := a.Selector.Append(nil) // the wire bytes before anything touches the store
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(a); err != nil {
		t.Fatal(err)
	}
	wire := func() []byte {
		got, _ := st.Get(a.ID)
		b, err := got.Selector.Append(nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// A refused Update that edited the selector in place.
	_ = st.Update(a.ID, func(c *Schedule) error {
		c.Selector.Hostname().Name[0] = 'X'
		var hn protocol.Hostname
		hn.SetName([]byte("other"))
		c.Selector.SetHostname(hn)
		return fmt.Errorf("refused")
	})
	if !reflect.DeepEqual(wire(), want) {
		t.Fatalf("a refused Update changed the stored selector")
	}
	// A caller editing what Get returned, or what it Put, does not edit the store.
	got, _ := st.Get(a.ID)
	got.Selector.Hostname().Name[0] = 'Y'
	a.Selector.Hostname().Name[1] = 'Z'
	if !reflect.DeepEqual(wire(), want) {
		t.Fatal("Get or Put shares the selector with the store")
	}
}

// schedulesFixtureV1 is a version-1 schedules.json written out by hand, with
// every field set. It is the test that fails when a Go field is renamed: the
// file's keys are the format, and a round trip through the code alone cannot
// see a rename that the writer and the reader share. Do not regenerate it from
// the code; edit it only with a format change.
const schedulesFixtureV1 = `{
  "version": 1,
  "schedules": [
    {
      "id": "00000000000000000000000000000001",
      "cron": "*/5 * * * *",
      "tz": "Asia/Tokyo",
      "state": 1,
      "task_id": "abababababababababababababababab",
      "repo": "r",
      "agent": "claude",
      "selector_b64": "AgNib3g=",
      "prompt": "do it",
      "extra_args": ["--x", "y"],
      "resume_conversation": true,
      "run": {
        "mode": 1,
        "deadline_ns": 3600000000000,
        "quiet_ns": 180000000000,
        "blocked_ns": 240000000000,
        "queue_timeout_ns": 90000000000,
        "turn_end": 3,
        "rows": 24,
        "cols": 80,
        "record": true,
        "oneshot": true,
        "no_pty": true
      },
      "grant": {
        "caps": 536870920,
        "scope": {
          "base": 2,
          "ids": ["cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"],
          "vis_base": 1,
          "vis_base_present": true,
          "exclude_self": true,
          "vis_ids": ["efefefefefefefefefefefefefefefef"],
          "overrides": [
            {"caps": 8, "base": 1, "exclude_self": true, "ids": ["01010101010101010101010101010101"]}
          ]
        },
        "delegate_caps": 8,
        "delegate_scope": {
          "base": 2,
          "ids": ["23232323232323232323232323232323"],
          "vis_base": 2,
          "vis_base_present": true,
          "exclude_self": true,
          "vis_ids": ["67676767676767676767676767676767"],
          "overrides": [
            {"caps": 16, "base": 2, "exclude_self": true, "ids": ["89898989898989898989898989898989"]}
          ]
        },
        "spawn_depth": {"limited": true, "value": 3},
        "max_descendants": {"limited": true, "value": 7}
      },
      "creator": "45454545454545454545454545454545",
      "created_at": "2026-11-15T09:30:00Z",
      "edited_at": "2026-11-15T09:31:00Z",
      "edited_by": "operator",
      "next_at": "2026-11-15T09:32:00Z",
      "history": [
        {
          "outcome": 8,
          "due_at": "2026-11-15T09:30:00Z",
          "at": "2026-11-15T09:30:01Z",
          "expect_run": 4,
          "task_id": "abababababababababababababababab",
          "queued_at": "2026-11-15T09:30:02Z",
          "started_at": "2026-11-15T09:30:03Z",
          "ended_at": "2026-11-15T09:30:04Z",
          "end_reason": 4,
          "by_kind": 3,
          "by": "someone",
          "message": "m"
        }
      ]
    }
  ]
}
`

func TestScheduleFileFixtureV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	if err := os.WriteFile(path, []byte(schedulesFixtureV1), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.Get(scheduleID(1))
	if !ok {
		t.Fatal("fixture schedule missing")
	}
	// Every field of what was loaded is set, so a key the reader no longer
	// reads shows as a zero here, not as a silent default.
	assertAllSet(t, "Schedule", reflect.ValueOf(got))
	ref := fullSchedule(t, "x", 0)
	wantSel, _ := ref.Selector.Append(nil)
	gotSel, _ := got.Selector.Append(nil)
	if !reflect.DeepEqual(wantSel, gotSel) {
		t.Errorf("selector %x, want %x", gotSel, wantSel)
	}
	base := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
	cid := func(b string) string { return strings.Repeat(b, 16) }
	want := Schedule{
		ID: scheduleID(1), Cron: "*/5 * * * *", TZ: "Asia/Tokyo",
		State: protocol.ScheduleTaskState_Bound, TaskID: cid("ab"), Repo: "r", Agent: "claude",
		Selector: got.Selector, Prompt: "do it", ExtraArgs: []string{"--x", "y"}, ResumeConversation: true,
		Run: RunSpec{Mode: protocol.RunMode_Background, Cond: RunConditions{
			Deadline: time.Hour, Quiet: 3 * time.Minute, Blocked: 4 * time.Minute, QueueTimeout: 90 * time.Second,
			TurnEnd: protocol.TurnEndSignal_Lifecycle, Rows: 24, Cols: 80,
		}, Record: true, Oneshot: true, NoPTY: true},
		Grant: authority.Grant{
			Caps: protocol.Capability_FileRead | protocol.Capability_Schedule,
			Scope: authority.Scope{Base: protocol.ScopeBase_Global, IDs: []string{cid("cd")},
				VisBase: protocol.ScopeBase_None, VisBasePresent: true, ExcludeSelf: true, VisIDs: []string{cid("ef")},
				Overrides: []authority.ScopeOverride{{Caps: protocol.Capability_FileRead, Base: protocol.ScopeBase_None, ExcludeSelf: true, IDs: []string{cid("01")}}}},
			DelegateCaps: protocol.Capability_FileRead,
			DelegateScope: authority.Scope{Base: protocol.ScopeBase_Global, IDs: []string{cid("23")},
				VisBase: protocol.ScopeBase_Global, VisBasePresent: true, ExcludeSelf: true, VisIDs: []string{cid("67")},
				Overrides: []authority.ScopeOverride{{Caps: 16, Base: protocol.ScopeBase_Global, ExcludeSelf: true, IDs: []string{cid("89")}}}},
			SpawnDepth: authority.LimitOf(3), MaxDescendants: authority.LimitOf(7),
		},
		Creator: cid("45"), CreatedAt: base, EditedAt: base.Add(time.Minute), EditedBy: "operator", NextAt: base.Add(2 * time.Minute),
		History: []HistoryEntry{{
			Outcome: protocol.ScheduleOutcome_ResumedBy, DueAt: base, At: base.Add(time.Second), ExpectRun: 4,
			TaskID: cid("ab"), QueuedAt: base.Add(2 * time.Second), StartedAt: base.Add(3 * time.Second),
			EndedAt: base.Add(4 * time.Second), EndReason: protocol.EndReason_Deadline,
			ByKind: protocol.ClientKind_Webui, By: "someone", Message: "m",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fixture loaded as\n got %+v\nwant %+v", got, want)
	}
}

// assertAllSet fails for any zero field reachable from v, except the opaque
// protocol values (RunnerSelector, SpawnLimit), which the callers check by
// their own accessors.
func assertAllSet(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Type() {
	case reflect.TypeOf(time.Time{}):
		if v.Interface().(time.Time).IsZero() {
			t.Errorf("%s is zero", path)
		}
		return
	case reflect.TypeOf(protocol.RunnerSelector{}):
		return
	case reflect.TypeOf(protocol.SpawnLimit{}):
		l := v.Interface().(protocol.SpawnLimit)
		if !l.Limited() || l.Value() == 0 {
			t.Errorf("%s is unbounded or 0", path)
		}
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			assertAllSet(t, path+"."+v.Type().Field(i).Name, v.Field(i))
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
		for i := 0; i < v.Len(); i++ {
			assertAllSet(t, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero", path)
		}
	}
}

// TestOnlySelectorEmbedsAWireFormatInSchedules is the schedules.json twin of
// TestOnlySelectorEmbedsAWireFormatInTheWAL: no struct from the protocol
// package may appear in the types the file is written from, because its schema
// would then be frozen into the file with no migration path. The selector is
// stored as bytes and decoded by decodePersistedSelector.
func TestOnlySelectorEmbedsAWireFormatInSchedules(t *testing.T) {
	if bad := wireFormatFields(reflect.TypeOf(scheduleFileJSON{}), "scheduleFileJSON", map[reflect.Type]bool{}); len(bad) > 0 {
		t.Errorf("schedules.json embeds protocol wire formats with no migration path: %v. "+
			"Store a decomposed form, or a byte string decoded through decodePersistedSelector.", bad)
	}
	// The guard itself: it sees a wire struct nested anywhere.
	type nested struct {
		A []struct {
			B map[string]*protocol.SpawnLimit
		}
	}
	if bad := wireFormatFields(reflect.TypeOf(nested{}), "nested", map[reflect.Type]bool{}); len(bad) != 1 {
		t.Errorf("guard missed a nested wire struct: %v", bad)
	}
}

func wireFormatFields(rt reflect.Type, path string, seen map[reflect.Type]bool) []string {
	const protocolPkg = "github.com/on-keyday/agent-harness/runner/protocol"
	for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
		rt = rt.Elem()
	}
	if rt.Kind() == reflect.Map {
		return wireFormatFields(rt.Elem(), path+"[]", seen)
	}
	if rt.Kind() != reflect.Struct || seen[rt] {
		return nil
	}
	if rt.PkgPath() == protocolPkg {
		return []string{path + " (" + rt.Name() + ")"}
	}
	seen[rt] = true
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		out = append(out, wireFormatFields(f.Type, path+"."+f.Name, seen)...)
	}
	return out
}
