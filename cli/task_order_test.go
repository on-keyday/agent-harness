package cli

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var orderNow = time.Unix(1_000_000, 0)

// ot builds a task: id byte, status, created / started / ended (seconds).
func ot(id byte, st protocol.TaskStatus, created, started, ended int64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = id
	t.Status = st
	t.CreatedAt = uint64(created * 1e9)
	t.StartedAt = uint64(started * 1e9)
	t.EndedAt = uint64(ended * 1e9)
	t.RepoPath = []byte("/w/a")
	return t
}

func withLife(t protocol.TaskInfo, s protocol.LifecycleState) protocol.TaskInfo {
	t.Lifecycle, t.LifecycleAt = s, 1
	return t
}

func withProg(t protocol.TaskInfo, s protocol.ProgramState) protocol.TaskInfo {
	t.SetLiveTerminal(true)
	t.ProgramState = s
	return t
}

// groupIDs flattens groups to "label:id,id|label:id" for compact asserts.
func groupIDs(gs []TaskGroup) string {
	var parts []string
	for _, g := range gs {
		var ids []string
		for _, t := range g.Tasks {
			ids = append(ids, string(rune('a'+t.Id.Id[0])))
		}
		parts = append(parts, g.Label+":"+strings.Join(ids, ","))
	}
	return strings.Join(parts, "|")
}

func TestOrderCreatedIsNewestFirstWithoutHeaders(t *testing.T) {
	ts := []protocol.TaskInfo{ot(0, protocol.TaskStatus_Running, 10, 11, 0), ot(1, protocol.TaskStatus_Succeeded, 30, 31, 40), ot(2, protocol.TaskStatus_Queued, 20, 0, 0)}
	if got := groupIDs(OrderTasks(ts, OrderCreated, orderNow)); got != ":b,c,a" {
		t.Errorf("created = %q", got)
	}
}

func TestOrderActivityUsesLiveOutputThenTimestamps(t *testing.T) {
	quiet := ot(0, protocol.TaskStatus_Detached, 10, 11, 0)
	quiet.LastOutputAt, quiet.OutputIdleMs = 1, 500_000_000 // idle for ~5.8 days
	busy := ot(1, protocol.TaskStatus_Running, 5, 6, 0)
	busy.LastOutputAt, busy.OutputIdleMs = 1, 1000
	ended := ot(2, protocol.TaskStatus_Failed, 1, 2, 900_000)
	if got := groupIDs(OrderTasks([]protocol.TaskInfo{quiet, busy, ended}, OrderActivity, orderNow)); got != ":b,c,a" {
		t.Errorf("activity = %q", got)
	}
}

func TestOrderLiveGroupsLiveByStartThenFinishedByEnd(t *testing.T) {
	ts := []protocol.TaskInfo{
		ot(0, protocol.TaskStatus_Running, 1, 50, 0),
		ot(1, protocol.TaskStatus_Succeeded, 2, 3, 70),
		ot(2, protocol.TaskStatus_Queued, 60, 0, 0), // never started: create time
		ot(3, protocol.TaskStatus_Held, 1, 40, 0),
		ot(4, protocol.TaskStatus_Cancelled, 2, 3, 90),
	}
	if got := groupIDs(OrderTasks(ts, OrderLive, orderNow)); got != "live:c,a,d|finished:e,b" {
		t.Errorf("live = %q", got)
	}
}

func TestOrderStateGroupsInFixedOrderAndSkipsEmpty(t *testing.T) {
	ts := []protocol.TaskInfo{
		withLife(ot(0, protocol.TaskStatus_Running, 1, 1, 0), protocol.LifecycleState_Working),
		withLife(ot(1, protocol.TaskStatus_Detached, 2, 2, 0), protocol.LifecycleState_Idle),
		withLife(ot(2, protocol.TaskStatus_Running, 3, 3, 0), protocol.LifecycleState_Waiting),
		withProg(ot(3, protocol.TaskStatus_Detached, 4, 4, 0), protocol.ProgramState_Blocked),
		ot(4, protocol.TaskStatus_Running, 5, 5, 0),
		ot(5, protocol.TaskStatus_Succeeded, 6, 6, 7),
	}
	want := "needs you:d,c|ready:b|working:a|no report:e|finished:f"
	if got := groupIDs(OrderTasks(ts, OrderState, orderNow)); got != want {
		t.Errorf("state = %q, want %q (queued is empty and must not appear)", got, want)
	}
}

// Review Focus 1: the lifecycle report decides, as it does for the badge.
func TestOrderStateLifecycleWinsOverProgram(t *testing.T) {
	both := withProg(withLife(ot(0, protocol.TaskStatus_Running, 1, 1, 0), protocol.LifecycleState_Idle), protocol.ProgramState_Blocked)
	if got := groupIDs(OrderTasks([]protocol.TaskInfo{both}, OrderState, orderNow)); got != "ready:a" {
		t.Errorf("lifecycle idle + program blocked = %q, want ready", got)
	}
}

func TestOrderRepoGroupsByNameLiveFirst(t *testing.T) {
	a1 := ot(0, protocol.TaskStatus_Succeeded, 1, 1, 50)
	a2 := ot(1, protocol.TaskStatus_Running, 2, 2, 0)
	b := ot(2, protocol.TaskStatus_Running, 3, 3, 0)
	b.RepoPath = []byte("/w/b")
	if got := groupIDs(OrderTasks([]protocol.TaskInfo{a1, a2, b}, OrderRepo, orderNow)); got != "a:b,a|b:c" {
		t.Errorf("repo = %q", got)
	}
}

// Review Focus 2: two repositories with one last path element stay apart.
func TestOrderRepoSameBasenameUsesFullPaths(t *testing.T) {
	x := ot(0, protocol.TaskStatus_Running, 1, 1, 0)
	x.RepoPath = []byte("/w/one/app")
	y := ot(1, protocol.TaskStatus_Running, 2, 2, 0)
	y.RepoPath = []byte("/w/two/app")
	got := OrderTasks([]protocol.TaskInfo{x, y}, OrderRepo, orderNow)
	var labels []string
	for _, g := range got {
		labels = append(labels, g.Label)
	}
	if !reflect.DeepEqual(labels, []string{"/w/one/app", "/w/two/app"}) {
		t.Errorf("labels = %v, want the full paths", labels)
	}
}

func TestOrderDoesNotMutateInput(t *testing.T) {
	ts := []protocol.TaskInfo{ot(0, protocol.TaskStatus_Running, 1, 1, 0), ot(1, protocol.TaskStatus_Running, 2, 2, 0)}
	OrderTasks(ts, OrderCreated, orderNow)
	if ts[0].Id.Id[0] != 0 {
		t.Error("OrderTasks reordered its input")
	}
}

func TestParseTaskOrderAndNext(t *testing.T) {
	for _, o := range TaskOrders {
		if got, err := ParseTaskOrder(string(o)); err != nil || got != o {
			t.Errorf("ParseTaskOrder(%q) = %q, %v", o, got, err)
		}
	}
	if _, err := ParseTaskOrder("bogus"); err == nil || !strings.Contains(err.Error(), "created") {
		t.Errorf("bogus mode: err = %v, want one naming the valid modes", err)
	}
	if OrderRepo.Next() != OrderCreated || OrderCreated.Next() != OrderActivity {
		t.Error("Next does not cycle created → … → repo → created")
	}
}

// The WebUI orders the ids its filter admits over the snapshot the wasm last
// fetched: ids outside that snapshot (Review Focus 4) come last, unlabelled;
// tasks the filter hid are left out.
func TestOrderTaskIDsKeepsUnknownIDsLast(t *testing.T) {
	a := ot(0, protocol.TaskStatus_Running, 1, 1, 0)
	b := ot(1, protocol.TaskStatus_Succeeded, 2, 2, 3)
	hidden := ot(2, protocol.TaskStatus_Running, 3, 3, 0)
	hexOf := func(t protocol.TaskInfo) string { return hex.EncodeToString(t.Id.Id[:]) }
	got := OrderTaskIDs([]protocol.TaskInfo{a, b, hidden}, []string{"ff", hexOf(b), hexOf(a)}, OrderLive, orderNow)
	want := []TaskIDGroup{{"live", []string{hexOf(a)}}, {"finished", []string{hexOf(b)}}, {"", []string{"ff"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OrderTaskIDs = %v, want %v", got, want)
	}
}

// A Windows runner reports backslash paths: the group is still named by the
// last element.
func TestOrderRepoNamesWindowsPathsByLastElement(t *testing.T) {
	w := ot(0, protocol.TaskStatus_Running, 1, 1, 0)
	w.RepoPath = []byte(`C:\work\proj`)
	if got := groupIDs(OrderTasks([]protocol.TaskInfo{w}, OrderRepo, orderNow)); got != "proj:a" {
		t.Errorf("repo = %q, want proj:a", got)
	}
}
