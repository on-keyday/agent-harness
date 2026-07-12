package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyHookTimeout bounds a hook process; a slow/hung sink must not pile up.
const notifyHookTimeout = 10 * time.Second

// notifyHookOutputCap bounds how much hook stdout+stderr we retain for the log,
// so a misbehaving hook can't flood it.
const notifyHookOutputCap = 4096

// capBuf is an io.Writer that retains at most cap bytes and drops the rest. It
// is mutex-guarded so reading it is safe even in the cmd.WaitDelay edge case
// where the exec I/O copier may still be writing after Wait returns.
type capBuf struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.cap - len(c.buf); room > 0 {
		if room < len(p) {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil // report full consumption; overflow is intentionally dropped
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(string(c.buf))
}

// NotifyHookFileName is the well-known file under the server's data-dir that
// persists the notify-hook command line across restarts. Flag and env are
// per-invocation and get forgotten whenever the server is respawned from a
// clean shell; the data-dir survives, so a hook written there once stays
// configured forever.
const NotifyHookFileName = "notify-hook"

// ResolveNotifyHook picks the notify-hook command line: --notify-hook flag →
// HARNESS_NOTIFY_HOOK env → first non-empty, non-# line of
// <dataDir>/notify-hook. Every value is a whitespace-split command line (see
// runNotifyHook). Returns the command and its source ("flag", "env", "file")
// for logging; "" command means no hook (egress disabled). A file read error
// other than not-exist is logged, not fatal — notify egress is best-effort.
func ResolveNotifyHook(flagVal, envVal, dataDir string) (cmd, source string) {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v, "flag"
	}
	if v := strings.TrimSpace(envVal); v != "" {
		return v, "env"
	}
	path := filepath.Join(dataDir, NotifyHookFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("notify hook: config file unreadable — egress disabled", "path", path, "err", err)
		}
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, "file"
	}
	return "", ""
}

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
// HARNESS_NOTIFY_* env. hookCmd is whitespace-split into executable + args —
// no quoting, so the executable path itself must not contain spaces (wrap it
// in a script if it does). It does NOT wait for completion: Start success →
// accepted (launched, not delivered); the process is reaped + timeout-killed in
// a background goroutine. Empty hookCmd → no_hook; Start failure → spawn_failed.
func runNotifyHook(hookCmd string, payload notifyHookPayload) protocol.NotifyStatus {
	argv := strings.Fields(hookCmd)
	if len(argv) == 0 {
		return protocol.NotifyStatus_NoHook
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("notify hook: marshal payload", "err", err)
		return protocol.NotifyStatus_SpawnFailed
	}
	cctx, cancel := context.WithTimeout(context.Background(), notifyHookTimeout)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"HARNESS_NOTIFY_LEVEL="+payload.Level,
		"HARNESS_NOTIFY_ORIGIN="+payload.Origin,
		"HARNESS_NOTIFY_TITLE="+payload.Title,
	)
	// Capture the hook's combined stdout+stderr (capped) so a failure surfaces
	// WHY in the server log, not just the exit code. Setting Stdout==Stderr makes
	// exec serialize the writes (single writer), so capBuf needs no extra lock
	// for that — its mutex only guards the post-Wait read.
	out := &capBuf{cap: notifyHookOutputCap}
	cmd.Stdout = out
	cmd.Stderr = out
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
			slog.Warn("notify hook: nonzero/timeout", "cmd", hookCmd, "err", err, "output", out.String())
		}
	}()
	return protocol.NotifyStatus_Accepted
}
