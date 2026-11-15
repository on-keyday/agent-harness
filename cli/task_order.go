package cli

import (
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TaskOrder is how a task list is ordered and grouped. The TUI table, `ls
// --order` and the WebUI (through the wasm bridge) all order through
// OrderTasks, so no surface restates the rules.
type TaskOrder string

const (
	OrderCreated  TaskOrder = "created"  // newest created first
	OrderActivity TaskOrder = "activity" // most recent activity first
	OrderLive     TaskOrder = "live"     // live, then finished
	OrderState    TaskOrder = "state"    // needs you / ready / working / ...
	OrderRepo     TaskOrder = "repo"     // one group per repository
)

// TaskOrders is every mode, in the order the TUI's key cycles them.
var TaskOrders = []TaskOrder{OrderCreated, OrderActivity, OrderLive, OrderState, OrderRepo}

// ParseTaskOrder reads a mode name.
func ParseTaskOrder(s string) (TaskOrder, error) {
	for _, o := range TaskOrders {
		if string(o) == s {
			return o, nil
		}
	}
	names := make([]string, len(TaskOrders))
	for i, o := range TaskOrders {
		names[i] = string(o)
	}
	return "", fmt.Errorf("unknown order %q (want one of %s)", s, strings.Join(names, ", "))
}

// Next is the mode after o in TaskOrders, wrapping.
func (o TaskOrder) Next() TaskOrder {
	for i, x := range TaskOrders {
		if x == o {
			return TaskOrders[(i+1)%len(TaskOrders)]
		}
	}
	return TaskOrders[0]
}

// TaskGroup is one run of a task list. Label "" means no header: the created
// and activity modes are a single unlabelled group.
type TaskGroup struct {
	Label string
	Tasks []protocol.TaskInfo
}

// The state groups, in display order.
const (
	groupNeedsYou = "needs you"
	groupReady    = "ready"
	groupWorking  = "working"
	groupNoReport = "no report"
	groupQueued   = "queued"
	groupFinished = "finished"
	groupLive     = "live"
)

var stateGroupOrder = []string{groupNeedsYou, groupReady, groupWorking, groupNoReport, groupQueued, groupFinished}

// TaskActivityAt is a task's latest activity in unix milliseconds: for a live
// session its last output (now minus the idle age), otherwise the latest of
// ended / started / created. The WebUI's activity order used the same rule.
func TaskActivityAt(t protocol.TaskInfo, now time.Time) int64 {
	if t.LastOutputAt > 0 {
		return now.UnixMilli() - int64(t.OutputIdleMs)
	}
	m := max(t.EndedAt, t.StartedAt, t.CreatedAt)
	return int64(m / 1e6)
}

// OrderTasks orders ts by o and splits it into groups; empty groups are left
// out and ts is not modified.
func OrderTasks(ts []protocol.TaskInfo, o TaskOrder, now time.Time) []TaskGroup {
	all := append([]protocol.TaskInfo(nil), ts...)
	byCreated := func(s []protocol.TaskInfo) {
		sort.SliceStable(s, func(i, j int) bool { return s[i].CreatedAt > s[j].CreatedAt })
	}
	byActivity := func(s []protocol.TaskInfo) {
		sort.SliceStable(s, func(i, j int) bool { return TaskActivityAt(s[i], now) > TaskActivityAt(s[j], now) })
	}
	byEnded := func(s []protocol.TaskInfo) {
		sort.SliceStable(s, func(i, j int) bool { return s[i].EndedAt > s[j].EndedAt })
	}
	switch o {
	case OrderActivity:
		byActivity(all)
		return nonEmptyGroups([]TaskGroup{{Tasks: all}})
	case OrderLive:
		var live, done []protocol.TaskInfo
		for _, t := range all {
			if isTerminalStatus(t.Status) {
				done = append(done, t)
			} else {
				live = append(live, t)
			}
		}
		started := func(t protocol.TaskInfo) uint64 {
			if t.StartedAt != 0 {
				return t.StartedAt
			}
			return t.CreatedAt
		}
		sort.SliceStable(live, func(i, j int) bool { return started(live[i]) > started(live[j]) })
		byEnded(done)
		return nonEmptyGroups([]TaskGroup{{groupLive, live}, {groupFinished, done}})
	case OrderState:
		buckets := map[string][]protocol.TaskInfo{}
		for _, t := range all {
			g := stateGroup(t)
			buckets[g] = append(buckets[g], t)
		}
		var out []TaskGroup
		for _, g := range stateGroupOrder {
			if g == groupFinished {
				byEnded(buckets[g])
			} else {
				byActivity(buckets[g])
			}
			out = append(out, TaskGroup{g, buckets[g]})
		}
		return nonEmptyGroups(out)
	case OrderRepo:
		labels := repoLabels(all)
		buckets := map[string][]protocol.TaskInfo{}
		for _, t := range all {
			l := labels[string(t.RepoPath)]
			buckets[l] = append(buckets[l], t)
		}
		names := make([]string, 0, len(buckets))
		for n := range buckets {
			names = append(names, n)
		}
		sort.Strings(names)
		var out []TaskGroup
		for _, n := range names {
			var live, done []protocol.TaskInfo
			for _, t := range buckets[n] {
				if isTerminalStatus(t.Status) {
					done = append(done, t)
				} else {
					live = append(live, t)
				}
			}
			byActivity(live)
			byEnded(done)
			out = append(out, TaskGroup{n, append(live, done...)})
		}
		return nonEmptyGroups(out)
	}
	byCreated(all)
	return nonEmptyGroups([]TaskGroup{{Tasks: all}})
}

// TaskIDGroup is a TaskGroup as hex task ids, for the WebUI's bridge.
type TaskIDGroup struct {
	Label string
	IDs   []string
}

// OrderTaskIDs is OrderTasks over the tasks of ts that ids names (hex), for a
// page that orders the rows its filter admits over the snapshot it last
// fetched. An id ts does not hold — the page's rows are newer — is kept, last,
// in an unlabelled group.
func OrderTaskIDs(ts []protocol.TaskInfo, ids []string, o TaskOrder, now time.Time) []TaskIDGroup {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var admitted []protocol.TaskInfo
	for _, t := range ts {
		id := hex.EncodeToString(t.Id.Id[:])
		if want[id] {
			admitted = append(admitted, t)
			delete(want, id)
		}
	}
	var out []TaskIDGroup
	for _, g := range OrderTasks(admitted, o, now) {
		ig := TaskIDGroup{Label: g.Label}
		for _, t := range g.Tasks {
			ig.IDs = append(ig.IDs, hex.EncodeToString(t.Id.Id[:]))
		}
		out = append(out, ig)
	}
	var unknown []string
	for _, id := range ids {
		if want[id] {
			unknown = append(unknown, id)
			delete(want, id)
		}
	}
	if len(unknown) > 0 {
		out = append(out, TaskIDGroup{IDs: unknown})
	}
	return out
}

func nonEmptyGroups(gs []TaskGroup) []TaskGroup {
	out := gs[:0]
	for _, g := range gs {
		if len(g.Tasks) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// stateGroup is t's group in the state mode, by the badge rule: a reported
// lifecycle decides; otherwise the program's own report on a live terminal.
func stateGroup(t protocol.TaskInfo) string {
	switch {
	case isTerminalStatus(t.Status):
		return groupFinished
	case t.Status == protocol.TaskStatus_Queued:
		return groupQueued
	case t.LifecycleAt != 0:
		switch t.Lifecycle {
		case protocol.LifecycleState_Waiting, protocol.LifecycleState_Error:
			return groupNeedsYou
		case protocol.LifecycleState_Idle:
			return groupReady
		case protocol.LifecycleState_Working:
			return groupWorking
		}
	case t.LiveTerminal() && t.ProgramState != protocol.ProgramState_None:
		switch t.ProgramState {
		case protocol.ProgramState_Blocked, protocol.ProgramState_Error:
			return groupNeedsYou
		case protocol.ProgramState_Idle, protocol.ProgramState_Done:
			return groupReady
		case protocol.ProgramState_Working:
			return groupWorking
		}
	}
	return groupNoReport
}

// repoLabels names each repository by its last path element, or by its full
// path when another repository in the list shares that element.
func repoLabels(ts []protocol.TaskInfo) map[string]string {
	byBase := map[string]map[string]bool{}
	for _, t := range ts {
		p := string(t.RepoPath)
		b := path.Base(strings.TrimRight(p, "/"))
		if byBase[b] == nil {
			byBase[b] = map[string]bool{}
		}
		byBase[b][p] = true
	}
	out := map[string]string{}
	for b, paths := range byBase {
		for p := range paths {
			if len(paths) > 1 {
				out[p] = p
			} else {
				out[p] = b
			}
		}
	}
	return out
}
