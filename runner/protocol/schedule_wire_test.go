package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

func schedID(b byte) ScheduleID {
	var id ScheduleID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func TestScheduleDetailRoundTrip(t *testing.T) {
	var d ScheduleDetail
	d.Row.Id = schedID(0x11)
	d.Row.TaskState = ScheduleTaskState_Bound
	d.Row.TaskStatus = TaskStatus(1)
	d.Row.TaskId.Id[0] = 0xaa
	d.Row.NextAt = 1700000000000000000
	d.Row.SetCron([]byte("0 9 * * 1-5"))
	d.Row.SetTz([]byte("Asia/Tokyo"))
	d.Row.SetAgent([]byte("claude"))
	d.Row.SetRepo([]byte("/r"))
	d.Row.SetPromptHead([]byte("do the thing"))
	d.SetPrompt([]byte("do the thing, in full"))

	var pend, fired, refused ScheduleHistoryEntry
	pend.Outcome, pend.DueAt, pend.At = ScheduleOutcome_Pending, 10, 11
	fired.Outcome, fired.DueAt, fired.At = ScheduleOutcome_Fired, 20, 21
	fired.QueuedAt, fired.StartedAt, fired.EndedAt, fired.EndReason = 22, 23, 24, EndReason_ProcessExit
	refused.Outcome, refused.DueAt, refused.At = ScheduleOutcome_SkippedRefused, 30, 31
	refused.ByKind = ClientKind_Schedule
	refused.By.Id[1] = 7
	refused.SetMessage([]byte("spawn limit reached"))
	d.SetHistory([]ScheduleHistoryEntry{pend, fired, refused})
	d.Row.SetLastPresent(true)
	d.Row.SetLast(refused)

	b := d.MustAppend(nil)
	var got ScheduleDetail
	if err := got.DecodeExact(b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MustAppend(nil), b) {
		t.Fatal("re-encode differs")
	}
	if len(got.History) != 3 || got.History[0].Outcome != ScheduleOutcome_Pending ||
		got.History[1].EndReason != EndReason_ProcessExit || got.History[1].EndedAt != 24 ||
		string(got.History[2].Message) != "spawn limit reached" || got.History[2].ByKind != ClientKind_Schedule ||
		got.Row.Last() == nil || got.Row.Last().Outcome != ScheduleOutcome_SkippedRefused ||
		string(got.Prompt) != "do the thing, in full" || got.Row.Id != schedID(0x11) {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestScheduleSpecPresenceBits(t *testing.T) {
	var s ScheduleSpec
	s.SetCronPresent(true)
	s.SetCron([]byte("@every 1h"))
	var got ScheduleSpec
	if err := got.DecodeExact(s.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if got.Cron() == nil || string(*got.Cron()) != "@every 1h" || !got.CronPresent() {
		t.Fatalf("cron lost: %+v", got)
	}
	for name, v := range map[string]bool{
		"tz": got.TzPresent(), "repo": got.RepoPresent(), "task": got.TaskPresent(),
		"agent": got.AgentPresent(), "selector": got.SelectorPresent(), "extra_args": got.ExtraArgsPresent(),
		"requested_caps": got.RequestedCapsPresent(), "resume_conversation": got.ResumeConversationSet(),
		"oneshot": got.OneshotSet(), "record": got.RecordSet(), "pty": got.PtySet(),
		"scope": got.ScopePresent(), "delegate_caps": got.DelegateCapsPresent(),
		"delegate_scope": got.DelegateScopePresent(), "spawn_depth": got.SpawnDepthPresent(),
		"max_descendants": got.MaxDescendantsPresent(), "conditions": got.ConditionsPresent(),
		"prompt": got.PromptPresent(),
	} {
		if v {
			t.Errorf("%s presence bit set on a spec that names only cron", name)
		}
	}
}

func TestTaskInfoScheduleID(t *testing.T) {
	var with, without TaskInfo
	with.SetSchedulePresent(true)
	with.SetScheduleId(schedID(0x42))
	for name, ti := range map[string]TaskInfo{"present": with, "absent": without} {
		var got TaskInfo
		if err := got.DecodeExact(ti.MustAppend(nil)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := name == "present"; got.SchedulePresent() != want {
			t.Fatalf("%s: schedule_present = %v", name, got.SchedulePresent())
		}
	}
	var got TaskInfo
	if err := got.DecodeExact(with.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if got.ScheduleId() == nil || *got.ScheduleId() != schedID(0x42) {
		t.Fatalf("schedule id = %v", got.ScheduleId())
	}
}

func TestTaskStatusEventScheduleID(t *testing.T) {
	var ev TaskStatusEvent
	ev.SetSchedulePresent(true)
	ev.SetScheduleId(schedID(0x43))
	var got TaskStatusEvent
	if err := got.DecodeExact(ev.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	if !got.SchedulePresent() || got.ScheduleId() == nil || *got.ScheduleId() != schedID(0x43) {
		t.Fatalf("event schedule id = %+v", got)
	}
	var none TaskStatusEvent
	if err := got.DecodeExact(none.MustAppend(nil)); err != nil || got.SchedulePresent() {
		t.Fatalf("absent: %v present=%v", err, got.SchedulePresent())
	}
}

func TestScheduleKindsRoundTrip(t *testing.T) {
	var spec ScheduleSpec
	spec.SetCronPresent(true)
	spec.SetCron([]byte("0 0 * * *"))
	spec.SetPromptPresent(true)
	spec.SetPromptStreamId(9)
	req := TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ScheduleEdit, RequestId: 5}}
	req.SetScheduleEdit(ScheduleEditRequest{Id: schedID(1), Spec: spec})
	var got TaskControlRequest
	if err := got.DecodeExact(req.MustAppend(nil)); err != nil {
		t.Fatal(err)
	}
	e := got.ScheduleEdit()
	if e == nil || e.Id != schedID(1) || e.Spec.PromptStreamId() == nil || *e.Spec.PromptStreamId() != 9 || !e.Spec.PromptPresent() {
		t.Fatalf("edit = %+v", e)
	}
	resp := TaskControlResponse{Kind: TaskControlKind_ScheduleShow, RequestId: 5}
	resp.SetScheduleShow(ScheduleResponse{Status: ScheduleStatus_Ok, Id: schedID(1), StreamId: 3})
	var gr TaskControlResponse
	if err := gr.DecodeExact(resp.MustAppend(nil)); err != nil || gr.ScheduleShow() == nil || gr.ScheduleShow().StreamId != 3 {
		t.Fatalf("response = %+v %v", gr, err)
	}
}

// The size guard for a schedule request is the client's CheckControlMessage,
// as for OpenInteractiveRequest (cli/client.go sendChecked): there is no
// per-field bound beyond cron and zone. A realistic create and edit fit; one
// with many scope ids does not and is refused by that check.
func realisticSpec(scopeIDs int) ScheduleSpec {
	var s ScheduleSpec
	s.SetCronPresent(true)
	s.SetCron([]byte("*/15 9-17 * * 1-5"))
	s.SetTzPresent(true)
	s.SetTz([]byte("Asia/Tokyo"))
	s.SetRepoPresent(true)
	s.SetRepoPath(bytes.Repeat([]byte("r"), 400))
	s.SetAgentPresent(true)
	s.SetAgentProfile([]byte("sandbox-claude"))
	s.SetSelectorPresent(true)
	var sel RunnerSelector
	sel.Kind = RunnerSelectorKind_ByHostname
	var h Hostname
	h.SetName([]byte("build-host-01"))
	sel.SetHostname(h)
	s.SetSelector(sel)
	s.SetExtraArgsPresent(true)
	s.SetExtraArgs(AgentArgsFromStrings([]string{"--model", "opus", "--add-dir", "/srv/data"}))
	s.SetRequestedCapsPresent(true)
	s.SetRequestedCaps(Capability_All)
	s.SetScopePresent(true)
	var sc TaskScope
	ids := make([]TaskID, scopeIDs)
	for i := range ids {
		ids[i].Id[0], ids[i].Id[1] = byte(i), byte(i>>8)
	}
	sc.SetIds(ids)
	s.SetScope(sc)
	s.SetConditionsPresent(true)
	s.SetConditions(BackgroundConditions{})
	s.SetPromptPresent(true)
	s.SetPromptStreamId(^uint64(0))
	s.SetSizePresent(true)
	s.SetInitialRows(24)
	s.SetInitialCols(80)
	return s
}

func scheduleRequests(spec ScheduleSpec) (create, edit *TaskControlRequest) {
	create = &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ScheduleCreate, RequestId: ^uint32(0)}}
	create.SetScheduleCreate(ScheduleCreateRequest{Spec: spec})
	edit = &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ScheduleEdit, RequestId: ^uint32(0)}}
	edit.SetScheduleEdit(ScheduleEditRequest{Id: schedID(9), Spec: spec})
	return
}

func TestRealisticScheduleRequestsFitOneDatagram(t *testing.T) {
	create, edit := scheduleRequests(realisticSpec(2))
	for name, r := range map[string]*TaskControlRequest{"create": create, "edit": edit} {
		n := encodedLen(t, r)
		if n > controlDatagramBudget {
			t.Errorf("realistic schedule %s is %d bytes, budget %d: %d over", name, n, controlDatagramBudget, n-controlDatagramBudget)
		} else {
			t.Logf("realistic schedule %s: %d bytes, %d bytes of margin", name, n, controlDatagramBudget-n)
		}
	}
}

func TestOversizedScheduleRequestIsRefusedByTheClientCheck(t *testing.T) {
	create, _ := scheduleRequests(realisticSpec(60))
	n := encodedLen(t, create)
	if n <= controlDatagramBudget {
		t.Fatalf("60 scope ids encode to %d bytes; expected over %d", n, controlDatagramBudget)
	}
	// The check cli.sendChecked applies before a send.
	if err := CheckControlMessage("udp", controlDatagramBudget, n); err == nil {
		t.Fatalf("CheckControlMessage accepted a %d-byte udp request over %d", n, controlDatagramBudget)
	}
	if err := CheckControlMessage("ws", controlDatagramBudget, n); err != nil {
		t.Fatalf("ws is not datagram-bound: %v", err)
	}
}

func TestScheduleSpecInitialSizeAndDetailRecordSet(t *testing.T) {
	var got ScheduleSpec
	if err := got.DecodeExact(func() []byte { s := realisticSpec(0); return s.MustAppend(nil) }()); err != nil {
		t.Fatal(err)
	}
	if !got.SizePresent() || got.InitialRows() == nil || *got.InitialRows() != 24 || *got.InitialCols() != 80 {
		t.Fatalf("initial size lost: %+v", got)
	}
	var none ScheduleSpec
	var back ScheduleSpec
	if err := back.DecodeExact(none.MustAppend(nil)); err != nil || back.SizePresent() {
		t.Fatalf("absent size: %v present=%v", err, back.SizePresent())
	}
	// record_set separates "server default" from "off".
	var def, off ScheduleDetail
	off.SetRecordSet(true)
	for name, d := range map[string]ScheduleDetail{"default": def, "off": off} {
		var g ScheduleDetail
		if err := g.DecodeExact(d.MustAppend(nil)); err != nil {
			t.Fatal(err)
		}
		if g.RecordSet() != (name == "off") || g.Record() {
			t.Fatalf("%s: record_set=%v record=%v", name, g.RecordSet(), g.Record())
		}
	}
}
