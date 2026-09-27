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
		WatcherId: 7, Sink: protocol.AwaitIdleSink_Board, ThresholdMs: 2500,
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
	w := protocol.AwaitIdleWatcherInfo{WatcherId: 3, Sink: protocol.AwaitIdleSink_Reply}
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
