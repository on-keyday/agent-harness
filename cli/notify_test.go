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
