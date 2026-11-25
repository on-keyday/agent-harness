package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// kindSpec is createSpec with the kind set.
func kindSpec(k protocol.TaskKind) *protocol.ScheduleSpec {
	sp := createSpec()
	sp.SetKindPresent(true)
	sp.SetKind(k)
	return sp
}

func TestScheduleCreateStreamKind(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	r := e.create(t, objproto.ConnectionID{}, kindSpec(protocol.TaskKind_Stream), "p")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	sc := e.stored(t, schedIDHex(r.Id))
	if sc.Kind != protocol.TaskKind_Stream || sc.Run.NoPTY || sc.Run.Cond.TurnEnd != protocol.TurnEndSignal_Lifecycle {
		t.Errorf("kind %v noPTY %v turn-end %v", sc.Kind, sc.Run.NoPTY, sc.Run.Cond.TurnEnd)
	}
	// No kind on a create is interactive.
	r = e.create(t, objproto.ConnectionID{}, createSpec(), "p")
	if sc := e.stored(t, schedIDHex(r.Id)); sc.Kind != protocol.TaskKind_Interactive {
		t.Errorf("default kind %v", sc.Kind)
	}
}

func TestScheduleKindUnsupported(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	for _, k := range []protocol.TaskKind{protocol.TaskKind_Credential, protocol.TaskKind(99)} {
		if r := e.create(t, objproto.ConnectionID{}, kindSpec(k), "p"); r.Status != protocol.ScheduleStatus_KindUnsupported {
			t.Errorf("kind %v: %v %s", k, r.Status, r.Message)
		}
	}
}

func TestScheduleCreateStreamWithPtyRefused(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := kindSpec(protocol.TaskKind_Stream)
	sp.SetPtySet(true)
	sp.SetPty(true)
	if r := e.create(t, objproto.ConnectionID{}, sp, "p"); r.Status != protocol.ScheduleStatus_PtyStream {
		t.Errorf("stream+pty: %v %s", r.Status, r.Message)
	}
}

// Editing a oneshot schedule to stream switches its kind cleanly (both are
// values of TaskKind, so they cannot coexist).
func TestScheduleEditStreamOverOneshotSwitchesKind(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := kindSpec(protocol.TaskKind_Oneshot)
	r := e.create(t, objproto.ConnectionID{}, sp, "p")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("create oneshot: %v %s", r.Status, r.Message)
	}
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Stream)
	if r := e.edit(t, objproto.ConnectionID{}, id, edit, ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	if sc := e.stored(t, id); sc.Kind != protocol.TaskKind_Stream {
		t.Errorf("edited kind = %v, want stream", sc.Kind)
	}
}

// Switching the kind re-judges the stored conditions: a turn-end the stream
// kind lacks is refused, not kept.
func TestScheduleEditStreamKeepsCheckingConditions(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := createSpec()
	sp.SetTurnEndPresent(true)
	sp.SetTurnEnd(protocol.TurnEndSignal_Program)
	r := e.create(t, objproto.ConnectionID{}, sp, "p")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Stream)
	if r := e.edit(t, objproto.ConnectionID{}, id, edit, ""); r.Status != protocol.ScheduleStatus_TurnEndUnsupported {
		t.Errorf("%v %s", r.Status, r.Message)
	}
	if sc := e.stored(t, id); sc.Kind != protocol.TaskKind_Interactive {
		t.Errorf("refused edit stored kind %v", sc.Kind)
	}
}

func TestScheduleEditKindBack(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	r := e.create(t, objproto.ConnectionID{}, kindSpec(protocol.TaskKind_Stream), "p")
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Interactive)
	if r := e.edit(t, objproto.ConnectionID{}, id, edit, ""); r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	if sc := e.stored(t, id); sc.Kind != protocol.TaskKind_Interactive {
		t.Errorf("kind %v", sc.Kind)
	}
}

// A schedules.json from before the kind existed reads as interactive; a
// stream schedule round-trips by name.
func TestScheduleStoreKindAbsentIsInteractive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	st, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	b := fireTestSchedule(2, protocol.ScheduleTaskState_PendingCreate, "")
	b.Kind = protocol.TaskKind_Stream
	for _, sc := range []Schedule{a, b} {
		if err := st.Put(sc); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind": "stream"`) {
		t.Errorf("stream not stored by name:\n%s", raw)
	}
	// Strip every kind field: the file an older binary wrote.
	old := strings.ReplaceAll(string(raw), `"kind": "stream",`, "")
	old = strings.ReplaceAll(old, `"kind": "interactive",`, "")
	if strings.Contains(old, `"kind"`) {
		t.Fatalf("fixture still has a kind:\n%s", old)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadScheduleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range st2.List() {
		if sc.Kind != protocol.TaskKind_Interactive {
			t.Errorf("%s: kind %v from a file without one", sc.ID, sc.Kind)
		}
	}
}

func TestScheduleFiresStreamKind(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	sc.Kind = protocol.TaskKind_Stream
	putSchedule(t, s, sc)
	if got := s.fireSchedule(sc.ID, sc.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("outcome %v", got)
	}
	after := getSched(t, s, sc.ID)
	task, ok := s.tasks.Get(after.TaskID)
	if !ok || task.Kind != protocol.TaskKind_Stream {
		t.Fatalf("created task kind %v (found %v)", task.Kind, ok)
	}
	// A bound interactive task resumed by a stream schedule runs as stream (K6).
	bound := terminalTask(t, s)
	sc2 := fireTestSchedule(2, protocol.ScheduleTaskState_Bound, bound)
	sc2.Kind = protocol.TaskKind_Stream
	putSchedule(t, s, sc2)
	if got := s.fireSchedule(sc2.ID, sc2.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("resume outcome %v", got)
	}
	if task, _ := s.tasks.Get(bound); task.Kind != protocol.TaskKind_Stream {
		t.Errorf("resumed task kind %v", task.Kind)
	}
	// A zero-valued Kind (a Schedule built without one) fires interactive, not oneshot.
	sc3 := fireTestSchedule(3, protocol.ScheduleTaskState_PendingCreate, "")
	putSchedule(t, s, sc3)
	s.fireSchedule(sc3.ID, sc3.NextAt, false)
	if task, _ := s.tasks.Get(getSched(t, s, sc3.ID).TaskID); task.Kind != protocol.TaskKind_Interactive {
		t.Errorf("zero kind fired as %v", task.Kind)
	}
}

func TestScheduleDetailCarriesKind(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	r := e.create(t, objproto.ConnectionID{}, kindSpec(protocol.TaskKind_Stream), "p")
	d, refusal := e.h.scheduleDetail(objproto.ConnectionID{}, r.Id)
	if refusal != nil {
		t.Fatalf("%v %s", refusal.Status, refusal.Message)
	}
	if d.Kind != protocol.TaskKind_Stream {
		t.Errorf("detail kind %v", d.Kind)
	}
}

// Switching to stream and back restores what the interactive schedule had:
// its --no-pty and its turn-end default (review finding 1).
func TestScheduleKindRoundTripKeepsInteractiveSettings(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := createSpec()
	sp.SetPtySet(true)
	sp.SetPty(false)
	r := e.create(t, objproto.ConnectionID{}, sp, "p")
	id := schedIDHex(r.Id)
	before := e.stored(t, id)
	if !before.Run.NoPTY || before.Run.Cond.TurnEnd != protocol.TurnEndSignal_Any {
		t.Fatalf("start: noPTY %v turn-end %v", before.Run.NoPTY, before.Run.Cond.TurnEnd)
	}
	kind := func(k protocol.TaskKind) {
		t.Helper()
		ed := &protocol.ScheduleSpec{}
		ed.SetKindPresent(true)
		ed.SetKind(k)
		if r := e.edit(t, objproto.ConnectionID{}, id, ed, ""); r.Status != protocol.ScheduleStatus_Ok {
			t.Fatalf("%v %s", r.Status, r.Message)
		}
	}
	kind(protocol.TaskKind_Stream)
	if sc := e.stored(t, id); sc.Run.Cond.TurnEnd != protocol.TurnEndSignal_Lifecycle {
		t.Errorf("stream turn-end %v", sc.Run.Cond.TurnEnd)
	}
	kind(protocol.TaskKind_Interactive)
	after := e.stored(t, id)
	if !after.Run.NoPTY || after.Run.Cond.TurnEnd != protocol.TurnEndSignal_Any {
		t.Errorf("after the round trip: noPTY %v turn-end %v, want true any", after.Run.NoPTY, after.Run.Cond.TurnEnd)
	}
}

// A stream firing's run has no PTY whatever the stored bit says, on the
// creating firing and on a resume (review finding 4).
func TestScheduleStreamFiringRunsWithoutPTY(t *testing.T) {
	s, _ := newFireServer(t, t.TempDir())
	sc := fireTestSchedule(1, protocol.ScheduleTaskState_PendingCreate, "")
	sc.Kind = protocol.TaskKind_Stream
	sc.Run.NoPTY = true
	putSchedule(t, s, sc)
	s.fireSchedule(sc.ID, sc.NextAt, false)
	after := getSched(t, s, sc.ID)
	task, _ := s.tasks.Get(after.TaskID)
	if task.Kind != protocol.TaskKind_Stream || task.NoPTY {
		t.Fatalf("created: kind %v noPTY %v", task.Kind, task.NoPTY)
	}
	s.tasks.Finish(after.TaskID, 0, nil)
	after.NextAt = after.NextAt.Add(-48 * time.Hour)
	putSchedule(t, s, after)
	if got := s.fireSchedule(after.ID, after.NextAt, false); got != protocol.ScheduleOutcome_Fired {
		t.Fatalf("second firing %v", got)
	}
	if task, _ := s.tasks.Get(after.TaskID); task.Kind != protocol.TaskKind_Stream || task.NoPTY || task.Run != 2 {
		t.Errorf("resumed: kind %v noPTY %v run %d", task.Kind, task.NoPTY, task.Run)
	}
}
