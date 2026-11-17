package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
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
	r := e.create(t, "", kindSpec(protocol.TaskKind_Stream), "p")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	sc := e.stored(t, schedIDHex(r.Id))
	if sc.Kind != protocol.TaskKind_Stream || sc.Run.NoPTY || sc.Run.Cond.TurnEnd != protocol.TurnEndSignal_Lifecycle {
		t.Errorf("kind %v noPTY %v turn-end %v", sc.Kind, sc.Run.NoPTY, sc.Run.Cond.TurnEnd)
	}
	// No kind on a create is interactive.
	r = e.create(t, "", createSpec(), "p")
	if sc := e.stored(t, schedIDHex(r.Id)); sc.Kind != protocol.TaskKind_Interactive {
		t.Errorf("default kind %v", sc.Kind)
	}
}

func TestScheduleKindUnsupported(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	for _, k := range []protocol.TaskKind{protocol.TaskKind_Oneshot, protocol.TaskKind(99)} {
		if r := e.create(t, "", kindSpec(k), "p"); r.Status != protocol.ScheduleStatus_KindUnsupported {
			t.Errorf("kind %v: %v %s", k, r.Status, r.Message)
		}
	}
}

func TestScheduleCreateStreamWithOneshotOrPtyRefused(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := kindSpec(protocol.TaskKind_Stream)
	sp.SetOneshotSet(true)
	sp.SetOneshot(true)
	if r := e.create(t, "", sp, "p"); r.Status != protocol.ScheduleStatus_OneshotStream {
		t.Errorf("stream+oneshot: %v %s", r.Status, r.Message)
	}
	sp = kindSpec(protocol.TaskKind_Stream)
	sp.SetPtySet(true)
	sp.SetPty(true)
	if r := e.create(t, "", sp, "p"); r.Status != protocol.ScheduleStatus_PtyStream {
		t.Errorf("stream+pty: %v %s", r.Status, r.Message)
	}
}

// The combination is judged after the overlay: a stored --oneshot and an edit
// naming only the kind.
func TestScheduleEditStreamOverOneshotRefused(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	sp := createSpec()
	sp.SetOneshotSet(true)
	sp.SetOneshot(true)
	r := e.create(t, "", sp, "p")
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Stream)
	if r := e.edit(t, "", id, edit, ""); r.Status != protocol.ScheduleStatus_OneshotStream {
		t.Errorf("%v %s", r.Status, r.Message)
	}
	if sc := e.stored(t, id); sc.Kind != protocol.TaskKind_Interactive || !sc.Run.Oneshot {
		t.Errorf("refused edit changed the schedule: kind %v oneshot %v", sc.Kind, sc.Run.Oneshot)
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
	r := e.create(t, "", sp, "p")
	if r.Status != protocol.ScheduleStatus_Ok {
		t.Fatalf("%v %s", r.Status, r.Message)
	}
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Stream)
	if r := e.edit(t, "", id, edit, ""); r.Status != protocol.ScheduleStatus_TurnEndUnsupported {
		t.Errorf("%v %s", r.Status, r.Message)
	}
	if sc := e.stored(t, id); sc.Kind != protocol.TaskKind_Interactive {
		t.Errorf("refused edit stored kind %v", sc.Kind)
	}
}

func TestScheduleEditKindBack(t *testing.T) {
	e := newSchedEnv(t)
	e.addCandidate("claude")
	r := e.create(t, "", kindSpec(protocol.TaskKind_Stream), "p")
	id := schedIDHex(r.Id)
	edit := &protocol.ScheduleSpec{}
	edit.SetKindPresent(true)
	edit.SetKind(protocol.TaskKind_Interactive)
	if r := e.edit(t, "", id, edit, ""); r.Status != protocol.ScheduleStatus_Ok {
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
	r := e.create(t, "", kindSpec(protocol.TaskKind_Stream), "p")
	d, refusal := e.h.scheduleDetail("", r.Id)
	if refusal != nil {
		t.Fatalf("%v %s", refusal.Status, refusal.Message)
	}
	if d.Kind != protocol.TaskKind_Stream {
		t.Errorf("detail kind %v", d.Kind)
	}
}
