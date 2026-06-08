package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRunNotifyHook_NoHook(t *testing.T) {
	if got := runNotifyHook("", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_NoHook {
		t.Fatalf("status = %v, want no_hook", got)
	}
}

func TestRunNotifyHook_SpawnFailed(t *testing.T) {
	if got := runNotifyHook("/nonexistent/notify-hook-xyz", notifyHookPayload{Text: "x"}); got != protocol.NotifyStatus_SpawnFailed {
		t.Fatalf("status = %v, want spawn_failed", got)
	}
}

func TestRunNotifyHook_Accepted_DeliversPayload(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\ncat > " + outFile + "\necho \"LEVEL=$HARNESS_NOTIFY_LEVEL\" >> " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	status := runNotifyHook(script, notifyHookPayload{Level: "warn", Text: "hello", Origin: "external"})
	if status != protocol.NotifyStatus_Accepted {
		t.Fatalf("status = %v, want accepted", status)
	}
	var data []byte
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			data = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := string(data)
	if !strings.Contains(s, `"text":"hello"`) || !strings.Contains(s, "LEVEL=warn") {
		t.Fatalf("hook did not receive payload/env, got: %q", s)
	}
}

func TestNotifyRing_AppendEvicts(t *testing.T) {
	r := newNotifyRing(3)
	for i := 0; i < 5; i++ {
		r.append(protocol.NotifyEvent{Ts: uint64(i)})
	}
	snap := r.snapshot()
	if len(snap) != 3 {
		t.Fatalf("ring len = %d, want 3", len(snap))
	}
	if snap[0].Ts != 2 || snap[2].Ts != 4 {
		t.Fatalf("ring kept wrong entries: first=%d last=%d", snap[0].Ts, snap[2].Ts)
	}
}
