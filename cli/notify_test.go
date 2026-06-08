package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestNewNotifyRequestFromEnv_Worker(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "0f0d4dd6")
	t.Setenv("HARNESS_RUNNER_ID", "ws:10.0.0.1:1-2")
	t.Setenv("HARNESS_REPO_PATH", "/repo")
	t.Setenv("HARNESS_HOSTNAME", "host1")

	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Warn, "title", "body")
	if nr.Origin != protocol.NotifyOrigin_Worker {
		t.Fatalf("origin = %v, want worker", nr.Origin)
	}
	w := nr.Worker()
	if w == nil {
		t.Fatal("Worker() is nil for worker origin")
	}
	if string(w.TaskId) != "0f0d4dd6" || string(w.Hostname) != "host1" {
		t.Fatalf("worker fields wrong: task_id=%q hostname=%q", w.TaskId, w.Hostname)
	}
	if string(nr.Text) != "body" || string(nr.Title) != "title" {
		t.Fatalf("title/text wrong: %q / %q", nr.Title, nr.Text)
	}
}

func TestNewNotifyRequestFromEnv_External(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "")
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", "hi")
	if nr.Origin != protocol.NotifyOrigin_External {
		t.Fatalf("origin = %v, want external", nr.Origin)
	}
	if nr.Worker() != nil {
		t.Fatal("Worker() must be nil for external origin")
	}
}

func TestMtuGuardNotify_Truncates(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "")
	long := strings.Repeat("あ", 2000) // ~6 KB UTF-8, far over budget
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", long)
	mtuGuardNotify(nr)

	if encodedNotifyWireLen(nr) > notifyWireBudget {
		t.Fatalf("after guard encoded len %d > budget %d", encodedNotifyWireLen(nr), notifyWireBudget)
	}
	if !strings.HasSuffix(string(nr.Text), "…") {
		t.Fatalf("truncated text must end with ellipsis, got tail %q", tail(string(nr.Text)))
	}
	if !utf8.Valid(nr.Text) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}

func TestMtuGuardNotify_ShortNoop(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "")
	nr := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", "short")
	mtuGuardNotify(nr)
	if string(nr.Text) != "short" {
		t.Fatalf("short text must be untouched, got %q", nr.Text)
	}
}

func TestTruncateRunes_ZeroMax(t *testing.T) {
	if got := truncateRunes("hello", 0); got != "" {
		t.Fatalf("truncateRunes with maxBytes=0 = %q, want empty", got)
	}
}

func tail(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[len(s)-12:]
}

func TestNotifyRequest_RoundTrip(t *testing.T) {
	// worker origin: worker block must survive the round trip
	t.Setenv("HARNESS_TASK_ID", "task42")
	t.Setenv("HARNESS_RUNNER_ID", "ws:host:1-2")
	t.Setenv("HARNESS_REPO_PATH", "/repo")
	t.Setenv("HARNESS_HOSTNAME", "gmkhost")
	w := newNotifyRequestFromEnv(protocol.NotifyLevel_Warn, "t", "body")

	var gotW protocol.NotifyRequest
	if err := gotW.DecodeExact(w.MustAppend(nil)); err != nil {
		t.Fatalf("worker decode: %v", err)
	}
	if gotW.Origin != protocol.NotifyOrigin_Worker {
		t.Fatalf("worker origin lost: %v", gotW.Origin)
	}
	if wi := gotW.Worker(); wi == nil || string(wi.TaskId) != "task42" || string(wi.Hostname) != "gmkhost" {
		t.Fatalf("worker block not round-tripped: %+v", gotW.Worker())
	}
	if string(gotW.Title) != "t" || string(gotW.Text) != "body" {
		t.Fatalf("title/text lost: %q / %q", gotW.Title, gotW.Text)
	}

	// external origin: worker block must be ABSENT after round trip
	t.Setenv("HARNESS_TASK_ID", "")
	e := newNotifyRequestFromEnv(protocol.NotifyLevel_Info, "", "hi")
	var gotE protocol.NotifyRequest
	if err := gotE.DecodeExact(e.MustAppend(nil)); err != nil {
		t.Fatalf("external decode: %v", err)
	}
	if gotE.Origin != protocol.NotifyOrigin_External {
		t.Fatalf("external origin lost: %v", gotE.Origin)
	}
	if gotE.Worker() != nil {
		t.Fatal("external NotifyRequest must have no worker block after round trip")
	}
	if string(gotE.Text) != "hi" {
		t.Fatalf("text lost: %q", gotE.Text)
	}
}
