package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestAwaitIdleStatusString_CoversEveryStatus(t *testing.T) {
	for s, want := range map[protocol.AwaitIdleStatus]string{
		protocol.AwaitIdleStatus_Fired:          "fired",
		protocol.AwaitIdleStatus_Armed:          "armed",
		protocol.AwaitIdleStatus_SessionStopped: "session_stopped",
		protocol.AwaitIdleStatus_NotFound:       "not_found",
		protocol.AwaitIdleStatus_BadRequest:     "bad_request",
		protocol.AwaitIdleStatus_Cancelled:      "cancelled",
	} {
		if got := AwaitIdleStatusString(s); got != want {
			t.Errorf("%v -> %q, want %q", s, got, want)
		}
	}
}

func TestAwaitIdleWatcherLines_EmptyIsSaid(t *testing.T) {
	got := AwaitIdleWatcherLines(nil)
	if len(got) != 1 || got[0] != "no armed watchers" {
		t.Fatalf("got %q", got)
	}
}

func TestAwaitIdleWatcherLines_Row(t *testing.T) {
	w := protocol.AwaitIdleWatcherInfo{
		WatcherId: 7, Sink: protocol.AwaitSink_Board, ThresholdMs: 2500,
		ArmedUnixMs: uint64(time.Now().Add(-3 * time.Second).UnixMilli()),
	}
	w.TaskId.Id[0] = 0xab
	w.Requester.Id[0] = 0x12
	w.SetTopic([]byte("chat.12000000"))
	lines := AwaitIdleWatcherLines([]protocol.AwaitIdleWatcherInfo{w})
	row := lines[len(lines)-1]
	for _, want := range []string{"7", "ab000000", "sink=board", "topic=chat.12000000", "threshold=2500ms", "by=12000000"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
	var op protocol.AwaitIdleWatcherInfo
	if by := AwaitIdleWatcherBy(&op); by != "operator" {
		t.Errorf("zero requester -> %q, want operator", by)
	}
}

func TestAwaitIdleWatcherJSONLine_CarriesFullIDs(t *testing.T) {
	w := protocol.AwaitIdleWatcherInfo{WatcherId: 3, Sink: protocol.AwaitSink_Reply}
	w.TaskId.Id[0] = 0xab
	var got map[string]any
	if err := json.Unmarshal([]byte(AwaitIdleWatcherJSONLine(&w)), &got); err != nil {
		t.Fatal(err)
	}
	if got["task_id"] != "ab000000000000000000000000000000" || got["sink"] != "reply" || got["watcher_id"] != float64(3) {
		t.Fatalf("got %v", got)
	}
	if got["requester"] != "" {
		t.Fatalf("operator requester = %v, want empty string", got["requester"])
	}
}

func TestParseAwaitIdleSignal(t *testing.T) {
	for in, want := range map[string]protocol.AwaitIdleSignal{
		"":          protocol.AwaitIdleSignal_Quiet,
		"quiet":     protocol.AwaitIdleSignal_Quiet,
		"lifecycle": protocol.AwaitIdleSignal_Lifecycle,
		"program":   protocol.AwaitIdleSignal_Program,
		"any":       protocol.AwaitIdleSignal_Any,
	} {
		got, err := ParseAwaitIdleSignal(in)
		if err != nil || got != want {
			t.Errorf("ParseAwaitIdleSignal(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseAwaitIdleSignal("busy"); err == nil {
		t.Error(`ParseAwaitIdleSignal("busy") accepted an unknown word`)
	}
}

func TestAwaitIdleSignalString(t *testing.T) {
	for s, want := range map[protocol.AwaitIdleSignal]string{
		protocol.AwaitIdleSignal_Quiet:     "quiet",
		protocol.AwaitIdleSignal_Lifecycle: "lifecycle",
		protocol.AwaitIdleSignal_Program:   "program",
		protocol.AwaitIdleSignal_Any:       "any",
		protocol.AwaitIdleSignal(9):        "unknown(9)",
	} {
		if got := AwaitIdleSignalString(s); got != want {
			t.Errorf("AwaitIdleSignalString(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestAwaitIdleWatcherLines_Signal(t *testing.T) {
	w := protocol.AwaitIdleWatcherInfo{WatcherId: 4, Sink: protocol.AwaitSink_Reply, ThresholdMs: 2500,
		Condition: protocol.AwaitCondition_Idle, Signal: protocol.AwaitIdleSignal_Program}
	if lines := AwaitIdleWatcherLines([]protocol.AwaitIdleWatcherInfo{w}); !strings.Contains(lines[0], "signal=program") {
		t.Fatalf("text row = %q, want signal=program", lines[0])
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(AwaitIdleWatcherJSONLine(&w)), &row); err != nil {
		t.Fatal(err)
	}
	if row["signal"] != "program" {
		t.Fatalf("json row signal = %v, want program", row["signal"])
	}
}

func TestAwaitIdleFiredDetail(t *testing.T) {
	for _, c := range []struct {
		firedBy protocol.AwaitIdleSignal
		lc      protocol.LifecycleState
		ps      protocol.ProgramState
		pk      protocol.ProgramKind
		want    string
	}{
		{protocol.AwaitIdleSignal_Program, 0, protocol.ProgramState_Done, 0, "program done"},
		{protocol.AwaitIdleSignal_Program, 0, protocol.ProgramState_Blocked, protocol.ProgramKind_Question, "program blocked:question"},
		{protocol.AwaitIdleSignal_Lifecycle, protocol.LifecycleState_Error, 0, 0, "lifecycle error"},
		{protocol.AwaitIdleSignal_Quiet, protocol.LifecycleState_Idle, protocol.ProgramState_Done, 0, ""},
	} {
		if got := AwaitIdleFiredDetail(c.firedBy, c.lc, c.ps, c.pk); got != c.want {
			t.Errorf("AwaitIdleFiredDetail(%v,%v,%v,%v) = %q, want %q", c.firedBy, c.lc, c.ps, c.pk, got, c.want)
		}
	}
}

// One wording for the watcher's condition cell, every surface: the signal is
// always named — quiet included, a value is never hidden for what it is — and
// the threshold is shown wherever a quiet leg uses it.
func TestAwaitWatcherWaitsFor(t *testing.T) {
	for _, c := range []struct {
		cond protocol.AwaitCondition
		sig  protocol.AwaitIdleSignal
		want string
	}{
		{protocol.AwaitCondition_Progress, protocol.AwaitIdleSignal_Quiet, "progress"},
		{protocol.AwaitCondition_Idle, protocol.AwaitIdleSignal_Quiet, "idle quiet 2500ms"},
		{protocol.AwaitCondition_Idle, protocol.AwaitIdleSignal_Lifecycle, "idle lifecycle"},
		{protocol.AwaitCondition_Idle, protocol.AwaitIdleSignal_Program, "idle program"},
		{protocol.AwaitCondition_Idle, protocol.AwaitIdleSignal_Any, "idle any 2500ms"},
	} {
		w := protocol.AwaitIdleWatcherInfo{Condition: c.cond, ThresholdMs: 2500, Signal: c.sig}
		if got := AwaitWatcherWaitsFor(&w); got != c.want {
			t.Errorf("AwaitWatcherWaitsFor(%v,%v) = %q, want %q", c.cond, c.sig, got, c.want)
		}
	}
}
