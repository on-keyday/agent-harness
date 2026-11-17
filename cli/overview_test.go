package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var ovNow = time.UnixMilli(1_000_000)

func ovTask(id byte, repo string, st protocol.LifecycleState, createdAt uint64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = id
	t.Kind = protocol.TaskKind_Interactive
	t.Status = protocol.TaskStatus_Running
	t.SetRepoPath([]byte(repo))
	t.CreatedAt = createdAt * 1e6 // ns; TaskActivityAt reads ms
	if st != protocol.LifecycleState_Unset {
		t.Lifecycle = st
		t.LifecycleAt = 1
	}
	return t
}

func TestBuildOverviewNeedsYouAndLive(t *testing.T) {
	in := OverviewInput{Tasks: []protocol.TaskInfo{
		ovTask(1, "/r/a", protocol.LifecycleState_Waiting, 10),
		ovTask(2, "/r/a", protocol.LifecycleState_Error, 40),
		ovTask(3, "/r/b", protocol.LifecycleState_Waiting, 30),
		ovTask(4, "/r/b", protocol.LifecycleState_Waiting, 20),
		ovTask(5, "/r/c", protocol.LifecycleState_Working, 50),
		ovTask(6, "/r/c", protocol.LifecycleState_Idle, 60),
	}}
	o := BuildOverview(in, ovNow)
	if o.NeedsYouCount != 4 || len(o.NeedsYou) != OverviewRowCap {
		t.Fatalf("needs you count %d rows %d", o.NeedsYouCount, len(o.NeedsYou))
	}
	if o.NeedsYou[0].ID[:2] != "02" || o.NeedsYou[1].ID[:2] != "03" || o.NeedsYou[2].ID[:2] != "04" {
		t.Errorf("needs you not by activity: %+v", o.NeedsYou)
	}
	if o.NeedsYou[0].Group != "needs you" || o.NeedsYou[0].Repo != "a" || o.NeedsYou[0].Badge == "" || o.NeedsYou[0].Now != "<interactive>" {
		t.Errorf("row fields: %+v", o.NeedsYou[0])
	}
	if len(o.Live) != 2 || o.Live[0].ID[:2] != "06" || o.Live[1].ID[:2] != "05" {
		t.Errorf("live rows: %+v", o.Live)
	}
	if o.GroupCounts["working"] != 1 || o.GroupCounts["ready"] != 1 {
		t.Errorf("group counts: %v", o.GroupCounts)
	}
	if _, ok := o.GroupCounts["needs you"]; ok {
		t.Errorf("needs you is its own section, not a group count: %v", o.GroupCounts)
	}
}

func TestBuildOverviewZeroCountsPresent(t *testing.T) {
	o := BuildOverview(OverviewInput{}, ovNow)
	for _, k := range []string{"ready", "working", "no report", "queued", "finished"} {
		if v, ok := o.GroupCounts[k]; !ok || v != 0 {
			t.Fatalf("%s missing: %v", k, o.GroupCounts)
		}
	}
	// JSON readers (the WebUI, `overview --json`) get [] for an empty section, not null.
	if o.NeedsYou == nil || o.Live == nil || o.Hosts == nil || o.Forwards == nil || o.Topics == nil {
		t.Fatalf("nil row slice: %+v", o)
	}
}

func TestBuildOverviewFinishedIsOnlyCounted(t *testing.T) {
	f := ovTask(1, "/r/a", protocol.LifecycleState_Error, 10)
	f.Status = protocol.TaskStatus_Failed
	o := BuildOverview(OverviewInput{Tasks: []protocol.TaskInfo{f}}, ovNow)
	if o.NeedsYouCount != 0 || len(o.Live) != 0 || o.GroupCounts["finished"] != 1 {
		t.Fatalf("failed task: needs %d live %d counts %v", o.NeedsYouCount, len(o.Live), o.GroupCounts)
	}
}

func TestBuildOverviewHosts(t *testing.T) {
	mk := func(host, goos string, st protocol.RunnerStatus, max uint16, active int) protocol.RunnerInfo {
		var r protocol.RunnerInfo
		r.SetHostname([]byte(host))
		r.SetGoos([]byte(goos))
		r.Status = st
		r.MaxTasks = max
		r.SetActiveTasks(make([]protocol.ActiveTaskRef, active))
		return r
	}
	in := OverviewInput{
		Runners: []protocol.RunnerInfo{
			mk("zeta", "linux", protocol.RunnerStatus_Idle, 4, 0),
			mk("alpha", "windows", protocol.RunnerStatus_Busy, 2, 2),
			mk("zeta", "linux", protocol.RunnerStatus_Busy, 4, 1),
		},
		Conns: []protocol.ConnInfo{{Role: protocol.ConnRole_Cli}, {Role: protocol.ConnRole_Tui}, {Role: protocol.ConnRole_Webui}, {Role: protocol.ConnRole_Agent}, {Role: protocol.ConnRole_Runner}},
	}
	o := BuildOverview(in, ovNow)
	if o.HostCount != 2 || o.SlotCount != 3 || o.OperatorClients != 3 {
		t.Fatalf("counts host %d slot %d clients %d", o.HostCount, o.SlotCount, o.OperatorClients)
	}
	z := o.Hosts[1]
	if o.Hosts[0].Hostname != "alpha" || z.Hostname != "zeta" || z.Goos != "linux" || z.Idle != 1 || z.Busy != 1 || z.Offline != 0 || z.ActiveTasks != 1 || z.MaxTasks != 8 {
		t.Errorf("hosts: %+v", o.Hosts)
	}
	in.ConnsErr = errors.New("caps: conns")
	o = BuildOverview(in, ovNow)
	if o.HostsErr != "caps: conns" || o.HostCount != 2 {
		t.Errorf("conns error: %q, hosts %d", o.HostsErr, o.HostCount)
	}
}

func TestBuildOverviewForwards(t *testing.T) {
	task := ovTask(7, "/r/web", protocol.LifecycleState_Working, 1)
	mk := func(id uint64, last uint64) protocol.PortForwardInfo {
		f := protocol.PortForwardInfo{ForwardId: id, LastActivityUnixMs: last, TaskId: task.Id}
		f.SetBindAddr([]byte("127.0.0.1"))
		f.BindPort = 8000 + uint16(id)
		f.SetTargetHost([]byte("localhost"))
		f.TargetPort = 3000
		return f
	}
	in := OverviewInput{
		Tasks:    []protocol.TaskInfo{task},
		Forwards: []protocol.PortForwardInfo{mk(1, 10), mk(2, 40), mk(3, 30), mk(4, 20)},
	}
	o := BuildOverview(in, ovNow)
	if o.ForwardCount != 4 || len(o.Forwards) != OverviewRowCap {
		t.Fatalf("count %d rows %d", o.ForwardCount, len(o.Forwards))
	}
	if o.Forwards[0].ID != 2 || o.Forwards[1].ID != 3 || o.Forwards[2].ID != 4 {
		t.Errorf("order: %+v", o.Forwards)
	}
	f := o.Forwards[0]
	if f.Repo != "web" || f.Spec == "" || f.Dir == "" || !strings.HasPrefix(f.TaskID, "07") {
		t.Errorf("row: %+v", f)
	}
	in.ForwardsErr = errors.New("caps: forwards")
	if o := BuildOverview(in, ovNow); o.ForwardsErr != "caps: forwards" {
		t.Errorf("forwards error %q", o.ForwardsErr)
	}
}

func TestBuildOverviewTopics(t *testing.T) {
	in := OverviewInput{
		Topics: []BoardTopicRow{
			{Name: "b", MsgCount: 2, LastPublishedAtMs: 50},
			{Name: "a", MsgCount: 3, LastPublishedAtMs: 50},
			{Name: "old", MsgCount: 9, LastPublishedAtMs: 1},
			{Name: "c", MsgCount: 1, LastPublishedAtMs: 40},
		},
		Newest: map[string]*BoardMessage{"a": {Payload: []byte("hello\nworld")}},
	}
	o := BuildOverview(in, ovNow)
	if o.TopicCount != 4 || o.MsgTotal != 15 || len(o.Topics) != OverviewRowCap {
		t.Fatalf("topics %d msgs %d rows %d", o.TopicCount, o.MsgTotal, len(o.Topics))
	}
	if o.Topics[0].Name != "a" || o.Topics[1].Name != "b" || o.Topics[2].Name != "c" {
		t.Errorf("order: %+v", o.Topics)
	}
	if o.Topics[0].Head != "hello" || o.Topics[1].Head != "" || o.Topics[0].MsgCount != 3 || o.Topics[0].LastPublishMs != 50 {
		t.Errorf("row: %+v", o.Topics)
	}
	in.TopicsErr = errors.New("caps: board")
	if o := BuildOverview(in, ovNow); o.BoardErr != "caps: board" {
		t.Errorf("board error %q", o.BoardErr)
	}
}

func TestBuildOverviewBoardHeadMissing(t *testing.T) {
	o := BuildOverview(OverviewInput{Topics: []BoardTopicRow{{Name: "t", MsgCount: 2, LastPublishedAtMs: 5}}}, ovNow)
	if len(o.Topics) != 1 || o.Topics[0].Head != "" || o.BoardErr != "" {
		t.Fatalf("%+v %q", o.Topics, o.BoardErr)
	}
}

func TestOverviewHeadTopics(t *testing.T) {
	got := OverviewHeadTopics([]BoardTopicRow{{Name: "z", LastPublishedAtMs: 1}, {Name: "b", LastPublishedAtMs: 5}, {Name: "a", LastPublishedAtMs: 5}, {Name: "m", LastPublishedAtMs: 3}})
	if strings.Join(got, ",") != "a,b,m" {
		t.Fatalf("got %v", got)
	}
}

func TestBoardHeadEscapes(t *testing.T) {
	got := BoardHead(&BoardMessage{Payload: []byte("hi\x1b[31m\xffx\nsecond")})
	if got != `hi\x1b[31m\xffx` {
		t.Fatalf("%q", got)
	}
}

func TestBoardHeadWithdrawnAndCap(t *testing.T) {
	if got := BoardHead(&BoardMessage{Payload: []byte("secret"), Retracted: true}); got != "(withdrawn)" {
		t.Errorf("withdrawn: %q", got)
	}
	if got := BoardHead(nil); got != "" {
		t.Errorf("nil: %q", got)
	}
	long := strings.Repeat("あ", OverviewHeadRunes+10)
	if got := BoardHead(&BoardMessage{Payload: []byte(long)}); got != strings.Repeat("あ", OverviewHeadRunes) {
		t.Errorf("cap: %d runes", len([]rune(got)))
	}
}

// The overview's badge is the TUI Life column's: the program's when the hooks
// never reported.
func TestTaskStateBadge(t *testing.T) {
	w := ovTask(1, "/r", protocol.LifecycleState_Waiting, 1)
	w.LifecycleAgeMs = 3 * 60 * 1000
	if got := TaskStateBadge(&w); got != "wait:3m" {
		t.Errorf("lifecycle: %q", got)
	}
	p := ovTask(2, "/r", protocol.LifecycleState_Unset, 1)
	p.SetLiveTerminal(true)
	p.ProgramState = protocol.ProgramState_Blocked
	if got := TaskStateBadge(&p); got != "p:blocked" {
		t.Errorf("program: %q", got)
	}
}

// Hosts are capped like every other section; the count stays whole.
func TestBuildOverviewHostsCapped(t *testing.T) {
	var rs []protocol.RunnerInfo
	for _, h := range []string{"e", "d", "c", "b", "a"} {
		var r protocol.RunnerInfo
		r.SetHostname([]byte(h))
		rs = append(rs, r)
	}
	o := BuildOverview(OverviewInput{Runners: rs}, ovNow)
	if o.HostCount != 5 || len(o.Hosts) != OverviewRowCap || o.Hosts[0].Hostname != "a" {
		t.Fatalf("count %d rows %+v", o.HostCount, o.Hosts)
	}
}
