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
	_ = got.DecodeExact(with.MustAppend(nil))
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

// A create with every field at its bound is one datagram (the brief's
// worst case): cron and zone at their maxima, the agent and selector at the
// widest the wire lets them be, the authority block fully present.
func worstScheduleCreate(extraArgBytes, repo int) *TaskControlRequest {
	var s ScheduleSpec
	s.SetCronPresent(true)
	s.SetCron(bytes.Repeat([]byte("c"), ScheduleCronMax))
	s.SetTzPresent(true)
	s.SetTz(bytes.Repeat([]byte("z"), ScheduleZoneMax))
	s.SetRepoPresent(true)
	s.SetRepoPath(bytes.Repeat([]byte("r"), repo))
	s.SetTaskPresent(true)
	s.SetTaskId(TaskID{})
	s.SetAgentPresent(true)
	s.SetAgentProfile(bytes.Repeat([]byte("a"), 255))
	var sel RunnerSelector
	sel.Kind = RunnerSelectorKind_ByConnId
	var cid ConnID
	cid.SetTransport(bytes.Repeat([]byte("t"), 255)) // the widest a selector can be on the wire
	cid.SetIpAddr(make([]byte, 16))
	sel.SetConnId(cid)
	s.SetSelectorPresent(true)
	s.SetSelector(sel)
	s.SetExtraArgsPresent(true)
	s.SetExtraArgs(AgentArgsFromStrings([]string{string(bytes.Repeat([]byte("x"), extraArgBytes))}))
	s.SetRequestedCapsPresent(true)
	s.SetRequestedCaps(Capability_All)
	s.SetResumeConversationSet(true)
	s.SetResumeConversation(true)
	s.SetOneshotSet(true)
	s.SetOneshot(true)
	s.SetRecordSet(true)
	s.SetRecord(true)
	s.SetPtySet(true)
	s.SetPty(true)
	s.SetScopePresent(true)
	s.SetScope(TaskScope{})
	s.SetDelegateCapsPresent(true)
	s.SetDelegateCaps(Capability_All)
	s.SetDelegateScopePresent(true)
	s.SetDelegateScope(TaskScope{})
	s.SetSpawnDepthPresent(true)
	s.SetSpawnDepth(SpawnLimit{})
	s.SetMaxDescendantsPresent(true)
	s.SetMaxDescendants(SpawnLimit{})
	s.SetConditionsPresent(true)
	s.SetConditions(BackgroundConditions{})
	s.SetPromptPresent(true)
	s.SetPromptStreamId(^uint64(0))
	req := &TaskControlRequest{Header: TaskControlRequestHeader{Kind: TaskControlKind_ScheduleCreate, RequestId: ^uint32(0)}}
	req.SetScheduleCreate(ScheduleCreateRequest{Spec: s})
	return req
}

func TestScheduleCreateAtItsBoundsFitsOneDatagram(t *testing.T) {
	n := encodedLen(t, worstScheduleCreate(ScheduleExtraArgsMax, ScheduleRepoMax))
	t.Logf("worst schedule_create: %d bytes (budget %d)", n, controlDatagramBudget)
	if n > controlDatagramBudget {
		t.Errorf("worst schedule_create is %d bytes, budget %d", n, controlDatagramBudget)
	}
}

// Negative control: unbounded repo and args do not fit, so the test above can fail.
func TestScheduleCreateWithoutBoundsWouldNotFit(t *testing.T) {
	if n := encodedLen(t, worstScheduleCreate(1024, 1024)); n <= controlDatagramBudget {
		t.Errorf("1024/1024 encode to %d bytes; expected over %d", n, controlDatagramBudget)
	}
}
