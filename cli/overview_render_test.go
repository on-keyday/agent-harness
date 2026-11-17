package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The five sections, in the spec's order, each with its count — an empty one
// included (a zero is never elided).
func TestWriteOverviewEmptyPrintsEveryHeader(t *testing.T) {
	var b bytes.Buffer
	WriteOverview(&b, BuildOverview(OverviewInput{}, ovNow), ovNow)
	out := b.String()
	want := []string{"needs you: 0", "tasks: 0 ready, 0 working, 0 no report, 0 queued, 0 finished", "hosts: 0 (0 slots, 0 operator clients)", "forwards: 0", "board: 0 topics, 0 messages"}
	last := -1
	for _, w := range want {
		i := strings.Index(out, w)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", w, out)
		}
		if i < last {
			t.Fatalf("%q out of order in:\n%s", w, out)
		}
		last = i
	}
}

func TestWriteOverviewRows(t *testing.T) {
	task := ovTask(7, "/r/web", protocol.LifecycleState_Waiting, 1)
	var r protocol.RunnerInfo
	r.SetHostname([]byte("gmk"))
	r.SetGoos([]byte("linux"))
	fw := protocol.PortForwardInfo{ForwardId: 3, TaskId: task.Id}
	fw.SetBindAddr([]byte("127.0.0.1"))
	fw.BindPort = 8080
	fw.SetTargetHost([]byte("localhost"))
	fw.TargetPort = 3000
	in := OverviewInput{
		Tasks:    []protocol.TaskInfo{task},
		Runners:  []protocol.RunnerInfo{r},
		Forwards: []protocol.PortForwardInfo{fw},
		Topics:   []BoardTopicRow{{Name: "chat.x", MsgCount: 2, LastPublishedAtMs: uint64(ovNow.UnixMilli()) - 120000}},
		Newest:   map[string]*BoardMessage{"chat.x": {Payload: []byte("hello there")}},
	}
	var b bytes.Buffer
	WriteOverview(&b, BuildOverview(in, ovNow), ovNow)
	out := b.String()
	for _, w := range []string{"needs you: 1", "07000000", "web", "gmk", "linux", "8080", "3000", "chat.x", "2 msgs", "2m ago", "hello there"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// A caller whose caps do not cover conns, forwards or the board still gets the
// task sections; each failed section names its error under its own header.
func TestOverviewWithPartialFailure(t *testing.T) {
	in := OverviewInput{
		Tasks:       []protocol.TaskInfo{ovTask(1, "/r/a", protocol.LifecycleState_Working, 1)},
		ConnsErr:    errors.New("conns denied"),
		ForwardsErr: errors.New("forwards denied"),
		TopicsErr:   errors.New("board denied"),
	}
	var b bytes.Buffer
	WriteOverview(&b, BuildOverview(in, ovNow), ovNow)
	out := b.String()
	for _, pair := range [][2]string{{"hosts:", "conns denied"}, {"forwards:", "forwards denied"}, {"board:", "board denied"}} {
		h, e := strings.Index(out, pair[0]), strings.Index(out, pair[1])
		if h < 0 || e < h {
			t.Errorf("%q not under %q in:\n%s", pair[1], pair[0], out)
		}
	}
	if !strings.Contains(out, "1 working") || !strings.Contains(out, "01000000") {
		t.Errorf("task sections lost:\n%s", out)
	}
}

// Topic names, hostnames, specs and errors are not ours: an agent picks a
// topic name, and the server accepts control bytes in one. Every line the
// overview prints is one terminal line with no raw control byte.
func TestWriteOverviewEscapesEveryField(t *testing.T) {
	var r protocol.RunnerInfo
	r.SetHostname([]byte("h\x1b]0;x\x07"))
	in := OverviewInput{
		Runners:   []protocol.RunnerInfo{r},
		Topics:    []BoardTopicRow{{Name: "evil\x1b[2J\ntopic", MsgCount: 1, LastPublishedAtMs: 1}},
		TopicsErr: errors.New("read evil\x1b[2J\ttopic"),
	}
	var b bytes.Buffer
	WriteOverview(&b, BuildOverview(in, ovNow), ovNow)
	out := b.String()
	if strings.ContainsAny(out, "\x1b\x07\t") {
		t.Fatalf("raw control byte in:\n%q", out)
	}
	if !strings.Contains(out, `evil\x1b[2J\x0atopic`) {
		t.Errorf("topic name not escaped onto one line:\n%s", out)
	}
}
