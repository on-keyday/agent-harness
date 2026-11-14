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
