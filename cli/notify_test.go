package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The request carries no identity, whatever the environment says: the server
// derives it from the principal (spec 2026-10-19-task-progress D6).
func TestNewNotifyRequest_IgnoresEnv(t *testing.T) {
	t.Setenv("HARNESS_TASK_ID", "0f0d4dd6")
	t.Setenv("HARNESS_HOSTNAME", "host1")
	nr := newNotifyRequest(protocol.NotifyLevel_Warn, "title", "body")
	var got protocol.NotifyRequest
	if err := got.DecodeExact(nr.MustAppend(nil)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Level != protocol.NotifyLevel_Warn || string(got.Title) != "title" || string(got.Text) != "body" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestMtuGuardNotify_Truncates(t *testing.T) {
	long := strings.Repeat("あ", 2000) // ~6 KB UTF-8, far over budget
	nr := newNotifyRequest(protocol.NotifyLevel_Info, "", long)
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
	nr := newNotifyRequest(protocol.NotifyLevel_Info, "", "short")
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

