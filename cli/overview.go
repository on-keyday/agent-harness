package cli

import (
	"bytes"
	"encoding/hex"
	"sort"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The overview is one screen: every section shows its count and at most
// OverviewRowCap rows (2026-11-17-overview-design.md).
const (
	OverviewRowCap    = 3
	OverviewHeadRunes = 60
)

// OverviewInput is everything the overview is computed from. Each surface
// fills it from what it already holds plus what it fetched; an Err field
// carries the failure of the call that would have filled the slice beside it.
type OverviewInput struct {
	Runners     []protocol.RunnerInfo
	Tasks       []protocol.TaskInfo
	Conns       []protocol.ConnInfo
	ConnsErr    error
	Forwards    []protocol.PortForwardInfo
	ForwardsErr error
	Topics      []BoardTopicRow
	TopicsErr   error
	// Newest is each topic's newest message, for the topics whose head was
	// fetched (OverviewHeadTopics); a topic absent here has no head.
	Newest map[string]*BoardMessage
}

type OverviewTask struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"`
	Badge      string `json:"badge"`
	Now        string `json:"now"`
	Group      string `json:"group"`
	ActivityAt int64  `json:"activity_at_ms"`
}

type OverviewHost struct {
	Hostname    string `json:"hostname"`
	Goos        string `json:"goos"`
	Idle        int    `json:"idle"`
	Busy        int    `json:"busy"`
	Offline     int    `json:"offline"`
	ActiveTasks int    `json:"active_tasks"`
	MaxTasks    int    `json:"max_tasks"`
}

type OverviewForward struct {
	ID     uint64 `json:"id"`
	Dir    string `json:"dir"`
	Spec   string `json:"spec"`
	TaskID string `json:"task_id"`
	Repo   string `json:"repo"`
}

type OverviewTopic struct {
	Name          string `json:"name"`
	MsgCount      int    `json:"msg_count"`
	LastPublishMs uint64 `json:"last_publish_ms"`
	// Age is LastPublishMs as OverviewAge writes it at build time, so no
	// surface re-derives "2m ago".
	Age string `json:"age"`
	// Head is the newest message's first line, escaped for a terminal; ""
	// when it was not fetched, "(withdrawn)" when that message is retracted.
	Head string `json:"head"`
}

// Overview is the summary every surface prints as given. Counts are uncapped;
// row slices hold at most OverviewRowCap entries. An *Err string is set when
// that section's source call failed — the section's other fields still hold
// whatever the remaining sources gave.
type Overview struct {
	NeedsYou      []OverviewTask `json:"needs_you"`
	NeedsYouCount int            `json:"needs_you_count"`
	Live          []OverviewTask `json:"live"`
	// GroupCounts holds every state group except "needs you", zeros included.
	GroupCounts map[string]int `json:"group_counts"`

	Hosts           []OverviewHost `json:"hosts"`
	HostCount       int            `json:"host_count"`
	SlotCount       int            `json:"slot_count"`
	OperatorClients int            `json:"operator_clients"`
	HostsErr        string         `json:"hosts_err,omitempty"`

	Forwards     []OverviewForward `json:"forwards"`
	ForwardCount int               `json:"forward_count"`
	ForwardsErr  string            `json:"forwards_err,omitempty"`

	Topics     []OverviewTopic `json:"topics"`
	TopicCount int             `json:"topic_count"`
	MsgTotal   int             `json:"msg_total"`
	BoardErr   string          `json:"board_err,omitempty"`
}

// TaskStateGroup is the state group the task list's `state` order puts t in:
// "needs you", "ready", "working", "no report", "queued" or "finished".
func TaskStateGroup(t protocol.TaskInfo) string { return stateGroup(t) }

// TaskStateBadge is a task's state badge as the TUI's Life column shows it,
// minus that column's emphasis: the lifecycle badge, or the program's when the
// hooks never reported.
func TaskStateBadge(t *protocol.TaskInfo) string {
	b := LifecycleBadge(t)
	if b == "lc:-" {
		if p := ProgramBadge(t); p != "" {
			return p
		}
	}
	return b
}

// BoardHead is the overview's line for a board message: the payload's first
// line, at most OverviewHeadRunes runes, escaped for a terminal. The payload
// is agent-written, so the escape happens here, once, for every surface.
func BoardHead(m *BoardMessage) string {
	if m == nil {
		return ""
	}
	if m.Retracted {
		return "(withdrawn)"
	}
	line := m.Payload
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	// Cut by runes before escaping, so an escape sequence is never split.
	n := 0
	for i := range string(line) {
		if n == OverviewHeadRunes {
			line = line[:i]
			break
		}
		n++
	}
	return EscapeForTerminal(line)
}

// OverviewHeadTopics names the topics whose newest message the overview
// shows: the first OverviewRowCap by last publish, newest first, name
// ascending on ties.
func OverviewHeadTopics(topics []BoardTopicRow) []string {
	ts := sortedTopics(topics)
	names := make([]string, 0, OverviewRowCap)
	for i := 0; i < len(ts) && i < OverviewRowCap; i++ {
		names = append(names, ts[i].Name)
	}
	return names
}

func sortedTopics(topics []BoardTopicRow) []BoardTopicRow {
	ts := append([]BoardTopicRow(nil), topics...)
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].LastPublishedAtMs != ts[j].LastPublishedAtMs {
			return ts[i].LastPublishedAtMs > ts[j].LastPublishedAtMs
		}
		return ts[i].Name < ts[j].Name
	})
	return ts
}

// BuildOverview computes the overview from in. It is pure: every surface calls
// it on the inputs it holds, so none restates a rule.
func BuildOverview(in OverviewInput, now time.Time) Overview {
	o := Overview{
		GroupCounts: map[string]int{},
		NeedsYou:    []OverviewTask{},
		Live:        []OverviewTask{},
		Hosts:       []OverviewHost{},
		Forwards:    []OverviewForward{},
		Topics:      []OverviewTopic{},
	}
	for _, g := range stateGroupOrder {
		if g != groupNeedsYou {
			o.GroupCounts[g] = 0
		}
	}

	repos := repoLabels(in.Tasks)
	tasks := append([]protocol.TaskInfo(nil), in.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool { return TaskActivityAt(tasks[i], now) > TaskActivityAt(tasks[j], now) })
	for i := range tasks {
		t := &tasks[i]
		g := stateGroup(*t)
		if g == groupNeedsYou {
			o.NeedsYouCount++
		} else {
			o.GroupCounts[g]++
		}
		if g == groupFinished {
			continue
		}
		row := OverviewTask{
			ID:         hex.EncodeToString(t.Id.Id[:]),
			Repo:       repos[string(t.RepoPath)],
			Badge:      TaskStateBadge(t),
			Now:        TaskNowText(*t),
			Group:      g,
			ActivityAt: TaskActivityAt(*t, now),
		}
		if g == groupNeedsYou {
			if len(o.NeedsYou) < OverviewRowCap {
				o.NeedsYou = append(o.NeedsYou, row)
			}
		} else if len(o.Live) < OverviewRowCap {
			o.Live = append(o.Live, row)
		}
	}

	o.SlotCount = len(in.Runners)
	byHost := map[string][]protocol.RunnerInfo{}
	for _, r := range in.Runners {
		h := string(r.Hostname)
		byHost[h] = append(byHost[h], r)
	}
	for h, rs := range byHost {
		host := OverviewHost{Hostname: h, Goos: RunnerGOOSStr(rs[0].Goos)}
		host.Idle, host.Busy, host.Offline = RunnerStatusCounts(rs)
		for _, r := range rs {
			host.ActiveTasks += len(r.ActiveTasks)
			host.MaxTasks += int(r.MaxTasks)
		}
		o.Hosts = append(o.Hosts, host)
	}
	sort.Slice(o.Hosts, func(i, j int) bool { return o.Hosts[i].Hostname < o.Hosts[j].Hostname })
	o.HostCount = len(o.Hosts)
	if len(o.Hosts) > OverviewRowCap {
		o.Hosts = o.Hosts[:OverviewRowCap]
	}
	for _, c := range in.Conns {
		switch c.Role {
		case protocol.ConnRole_Cli, protocol.ConnRole_Tui, protocol.ConnRole_Webui:
			o.OperatorClients++
		}
	}
	if in.ConnsErr != nil {
		o.HostsErr = in.ConnsErr.Error()
	}

	fws := append([]protocol.PortForwardInfo(nil), in.Forwards...)
	sort.SliceStable(fws, func(i, j int) bool { return fws[i].LastActivityUnixMs > fws[j].LastActivityUnixMs })
	repoByTask := map[[16]byte]string{}
	for _, t := range in.Tasks {
		repoByTask[t.Id.Id] = repos[string(t.RepoPath)]
	}
	o.ForwardCount = len(fws)
	for i := 0; i < len(fws) && i < OverviewRowCap; i++ {
		f := &fws[i]
		o.Forwards = append(o.Forwards, OverviewForward{
			ID:     f.ForwardId,
			Dir:    PortForwardDirFlag(f.Direction),
			Spec:   PortForwardSpecString(f),
			TaskID: hex.EncodeToString(f.TaskId.Id[:]),
			Repo:   repoByTask[f.TaskId.Id],
		})
	}
	if in.ForwardsErr != nil {
		o.ForwardsErr = in.ForwardsErr.Error()
	}

	o.TopicCount = len(in.Topics)
	for _, t := range in.Topics {
		o.MsgTotal += t.MsgCount
	}
	for i, t := range sortedTopics(in.Topics) {
		if i == OverviewRowCap {
			break
		}
		o.Topics = append(o.Topics, OverviewTopic{
			Name:          t.Name,
			MsgCount:      t.MsgCount,
			LastPublishMs: t.LastPublishedAtMs,
			Age:           OverviewAge(t.LastPublishedAtMs, now),
			Head:          BoardHead(in.Newest[t.Name]),
		})
	}
	if in.TopicsErr != nil {
		o.BoardErr = in.TopicsErr.Error()
	}
	return o
}
