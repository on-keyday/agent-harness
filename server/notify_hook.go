package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyHookTimeout bounds a hook process; a slow/hung sink must not pile up.
const notifyHookTimeout = 10 * time.Second

// notifyHookPayload is the JSON written to the hook's stdin. Worker fields are
// empty for origin=external. conn_id + ts are server-injected.
type notifyHookPayload struct {
	Level    string `json:"level"`
	Origin   string `json:"origin"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	TaskID   string `json:"task_id,omitempty"`
	RunnerID string `json:"runner_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	ConnID   string `json:"conn_id"`
	Ts       int64  `json:"ts"`
}

// runNotifyHook launches hookCmd (no shell), passing payload as stdin JSON and
// HARNESS_NOTIFY_* env. It does NOT wait for completion: Start success →
// accepted (launched, not delivered); the process is reaped + timeout-killed in
// a background goroutine. Empty hookCmd → no_hook; Start failure → spawn_failed.
func runNotifyHook(hookCmd string, payload notifyHookPayload) protocol.NotifyStatus {
	if hookCmd == "" {
		return protocol.NotifyStatus_NoHook
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("notify hook: marshal payload", "err", err)
		return protocol.NotifyStatus_SpawnFailed
	}
	cctx, cancel := context.WithTimeout(context.Background(), notifyHookTimeout)
	cmd := exec.CommandContext(cctx, hookCmd)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"HARNESS_NOTIFY_LEVEL="+payload.Level,
		"HARNESS_NOTIFY_ORIGIN="+payload.Origin,
		"HARNESS_NOTIFY_TITLE="+payload.Title,
	)
	// Bound Wait() even if the hook leaves inheriting children holding the
	// I/O pipes (mirrors runner/process.go). Force-kill I/O after the deadline.
	cmd.WaitDelay = notifyHookTimeout
	if err := cmd.Start(); err != nil {
		cancel()
		slog.Error("notify hook: spawn failed", "cmd", hookCmd, "err", err)
		return protocol.NotifyStatus_SpawnFailed
	}
	go func() {
		defer cancel()
		if err := cmd.Wait(); err != nil {
			slog.Warn("notify hook: nonzero/timeout", "cmd", hookCmd, "err", err)
		}
	}()
	return protocol.NotifyStatus_Accepted
}
