# Agent Comms P2: Runner Ticket Plumbing + settings.json Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** runner が server から受け取った `auth_ticket` を claude プロセスに env (`HARNESS_*`) として注入し、worktree 内に `.claude/settings.json` (UserPromptSubmit hook 設定) を生成する。

**Architecture:** `Session.handleAssign` / `handleOpenExec` で `req.AuthTicket` を読み、worktree 作成後に `WorktreeManager.WriteAgentSettings` で settings.json を出力する。`Process.Run` に `Env []string` を追加し、`exec.Cmd.Env` に `HARNESS_*` を一式渡す。

**Tech Stack:** Go 1.25.7、`os/exec`、`encoding/json`、既存 `runner/` パッケージ、P1 で導入された `protocol.AssignTask.AuthTicket` フィールド。Spec: `docs/superpowers/specs/2026-04-28-agent-comms-design.md` §5.2, §6.3, §10.3, §10.4。

---

## Reference for implementers

### 前提

- P1 が完了している (auth_ticket フィールドが AssignTask / OpenExecRunnerRequest に存在)
- runner.Config に `AllowedRoots []string`, `MaxTasks int`, `Hostname string` がある (multi-task / multi-roots merge 後)

### env contract (P2 で実装する必須セット)

| env name | 値 | 由来 |
|---|---|---|
| `HARNESS_SERVER_CID` | `runner.Config.ServerCID.String()` | runner config |
| `HARNESS_RUNNER_ID` | runner の `pc.Connection().ConnectionID()` を `objproto.ConnectionID.String()` 化 | peer.Conn |
| `HARNESS_TASK_ID` | hex(req.TaskId.Id[:]) | AssignTask / OpenExec |
| `HARNESS_REPO_PATH` | string(req.RepoPath) | AssignTask / OpenExec |
| `HARNESS_HOSTNAME` | runner.Config.Hostname (空ならスキップ) | runner config |
| `HARNESS_WS_PATH` | `cli.WebSocketPath` | global var |
| `HARNESS_AUTH_TICKET` | hex(req.AuthTicket[:]) | AssignTask / OpenExec |

### settings.json layout

`<worktree>/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "harness-cli agent inbox --since-last --json" }
        ]
      }
    ]
  }
}
```

### 既存 Process 仕様

- `Process.Run(ctx, prompt, sink)` は `exec.CommandContext` を使う。env は現状 inherit (デフォルト)
- env を上書きすると親 env が引き継がれない。`os.Environ()` に追加する形で `cmd.Env = append(os.Environ(), HARNESS_*...)` を組む

### 既存 ExecuteCommand (interactive) 仕様

interactive PTY 経路は `agentexec.ExecuteCommand(ctx, stream, log, claudeBin, extraArgs, dir, alloc)`。env を渡す引数を追加するか、`os.Setenv` で global にするか。**`os.Setenv` は同一 runner 内で複数 task が並行するため使えない。** `ExecuteCommand` の signature 拡張が必要。

---

## File structure

### Create

```
runner/agentenv.go        # build []string of HARNESS_* env from config + req
runner/agentenv_test.go
runner/settings.go        # write .claude/settings.json into worktree dir
runner/settings_test.go
```

### Modify

```
runner/process.go                        # Process.Env []string field; cmd.Env wiring
runner/process_test.go                   # cover Env propagation
runner/session.go                        # build env, pass to Process; call settings writer
runner/session_test.go                   # cover env propagation + settings.json generation
runner/worktree.go                       # (no change; settings is separate file)
exec/exec.go (or wherever ExecuteCommand lives) # accept extraEnv []string for interactive PTY claude
runner/connect.go                        # pass cfg.ServerCID into Session for env
```

---

## Tasks

### Task 1: Process accepts Env

**Files:**
- Modify: `runner/process.go`
- Modify: `runner/process_test.go`

- [ ] **Step 1: Write failing test**

`runner/process_test.go` に追加:

```go
func TestProcess_RunSetsEnv(t *testing.T) {
    // Use a fake binary that prints HARNESS_TASK_ID from env.
    fake := writeFakeClaude(t, `#!/usr/bin/env bash
echo "TASK_ID=$HARNESS_TASK_ID"`)
    p := &Process{
        ClaudeBin: fake,
        CWD:       t.TempDir(),
        Env:       []string{"HARNESS_TASK_ID=deadbeef"},
    }
    var out []byte
    sink := func(data []byte) { out = append(out, data...) }
    code, err := p.Run(context.Background(), "ignored", sink)
    if err != nil {
        t.Fatal(err)
    }
    if code != 0 {
        t.Fatalf("exit = %d, want 0", code)
    }
    if !strings.Contains(string(out), "TASK_ID=deadbeef") {
        t.Errorf("env not propagated; output = %q", out)
    }
}
```

`writeFakeClaude` は既存 test fixture から流用 (process_test.go の最上部にあるはず)。

- [ ] **Step 2: Run test (fails)**

```bash
go test ./runner/ -run TestProcess_RunSetsEnv -v
```

Expected: build error: `Env` field undefined.

- [ ] **Step 3: Add Env field and wire to cmd.Env**

`runner/process.go`:

```go
type Process struct {
    ClaudeBin string
    CWD       string
    Timeout   time.Duration
    ExtraArgs []string
    Env       []string  // additional env vars to merge with os.Environ()
}
```

`Run` 内、`cmd.Dir = p.CWD` の直後に:

```go
if len(p.Env) > 0 {
    cmd.Env = append(os.Environ(), p.Env...)
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./runner/ -run TestProcess_RunSetsEnv -v
```

Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add runner/process.go runner/process_test.go
git commit -m "runner/process: add Env []string for HARNESS_* injection"
```

---

### Task 2: agentenv builder

**Files:**
- Create: `runner/agentenv.go`
- Create: `runner/agentenv_test.go`

- [ ] **Step 1: Write failing test**

```go
package runner

import (
    "encoding/hex"
    "strings"
    "testing"

    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBuildAgentEnv_AllFields(t *testing.T) {
    var taskID protocol.TaskID
    copy(taskID.Id[:], []byte{0xde, 0xad, 0xbe, 0xef})
    var ticket [16]byte
    copy(ticket[:], []byte{0xfe, 0xed, 0xfa, 0xce})

    spec := AgentEnvSpec{
        ServerCID:  mustParseCID(t, "ws:127.0.0.1:8539-12345"),
        RunnerID:   mustParseCID(t, "ws:1.2.3.4:9999-42"),
        TaskID:     taskID,
        RepoPath:   "/home/u/repo",
        Hostname:   "dev-pi-01",
        WSPath:     "/ws",
        AuthTicket: ticket,
    }
    env := BuildAgentEnv(spec)
    want := map[string]string{
        "HARNESS_SERVER_CID":  "ws:127.0.0.1:8539-12345",
        "HARNESS_RUNNER_ID":   "ws:1.2.3.4:9999-42",
        "HARNESS_TASK_ID":     hex.EncodeToString(taskID.Id[:]),
        "HARNESS_REPO_PATH":   "/home/u/repo",
        "HARNESS_HOSTNAME":    "dev-pi-01",
        "HARNESS_WS_PATH":     "/ws",
        "HARNESS_AUTH_TICKET": hex.EncodeToString(ticket[:]),
    }
    got := envMap(env)
    for k, v := range want {
        if got[k] != v {
            t.Errorf("env[%q] = %q, want %q", k, got[k], v)
        }
    }
}

func TestBuildAgentEnv_OmitsEmptyHostname(t *testing.T) {
    spec := AgentEnvSpec{
        ServerCID: mustParseCID(t, "ws:127.0.0.1:8539-1"),
        RunnerID:  mustParseCID(t, "ws:1.2.3.4:9999-1"),
        WSPath:    "/ws",
    }
    env := BuildAgentEnv(spec)
    for _, e := range env {
        if strings.HasPrefix(e, "HARNESS_HOSTNAME=") {
            t.Errorf("hostname should be omitted when empty, got %q", e)
        }
    }
}

func envMap(env []string) map[string]string {
    out := make(map[string]string)
    for _, e := range env {
        if i := strings.IndexByte(e, '='); i > 0 {
            out[e[:i]] = e[i+1:]
        }
    }
    return out
}

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
    t.Helper()
    cid, err := objproto.ParseConnectionID(s, 0)
    if err != nil { t.Fatal(err) }
    return cid
}
```

- [ ] **Step 2: Run test (fails)**

```bash
go test ./runner/ -run TestBuildAgentEnv -v
```

Expected: build error.

- [ ] **Step 3: Implement `runner/agentenv.go`**

```go
package runner

import (
    "encoding/hex"

    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner/protocol"
)

// AgentEnvSpec is the input bundle for BuildAgentEnv.
type AgentEnvSpec struct {
    ServerCID  objproto.ConnectionID
    RunnerID   objproto.ConnectionID
    TaskID     protocol.TaskID
    RepoPath   string
    Hostname   string
    WSPath     string
    AuthTicket [16]byte
}

// BuildAgentEnv returns "KEY=VAL" entries to merge with os.Environ() in Process.Env.
func BuildAgentEnv(s AgentEnvSpec) []string {
    env := []string{
        "HARNESS_SERVER_CID=" + s.ServerCID.String(),
        "HARNESS_RUNNER_ID=" + s.RunnerID.String(),
        "HARNESS_TASK_ID=" + hex.EncodeToString(s.TaskID.Id[:]),
        "HARNESS_REPO_PATH=" + s.RepoPath,
        "HARNESS_WS_PATH=" + s.WSPath,
        "HARNESS_AUTH_TICKET=" + hex.EncodeToString(s.AuthTicket[:]),
    }
    if s.Hostname != "" {
        env = append(env, "HARNESS_HOSTNAME="+s.Hostname)
    }
    return env
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./runner/ -run TestBuildAgentEnv -v
```

Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add runner/agentenv.go runner/agentenv_test.go
git commit -m "runner: BuildAgentEnv assembles HARNESS_* env from spec"
```

---

### Task 3: settings.json writer

**Files:**
- Create: `runner/settings.go`
- Create: `runner/settings_test.go`

- [ ] **Step 1: Write failing test**

```go
package runner

import (
    "encoding/json"
    "os"
    "path/filepath"
    "testing"
)

func TestWriteAgentSettings_CreatesFileWithHook(t *testing.T) {
    dir := t.TempDir()
    if err := WriteAgentSettings(dir); err != nil {
        t.Fatal(err)
    }
    path := filepath.Join(dir, ".claude", "settings.json")
    data, err := os.ReadFile(path)
    if err != nil {
        t.Fatalf("settings.json missing: %v", err)
    }
    var parsed map[string]any
    if err := json.Unmarshal(data, &parsed); err != nil {
        t.Fatalf("invalid json: %v", err)
    }
    hooks, ok := parsed["hooks"].(map[string]any)
    if !ok {
        t.Fatal("hooks key missing")
    }
    if _, ok := hooks["UserPromptSubmit"]; !ok {
        t.Error("UserPromptSubmit hook not present")
    }
}

func TestWriteAgentSettings_OverwritesExisting(t *testing.T) {
    dir := t.TempDir()
    sub := filepath.Join(dir, ".claude")
    if err := os.MkdirAll(sub, 0o755); err != nil {
        t.Fatal(err)
    }
    if err := os.WriteFile(filepath.Join(sub, "settings.json"), []byte("{}"), 0o644); err != nil {
        t.Fatal(err)
    }
    if err := WriteAgentSettings(dir); err != nil {
        t.Fatal(err)
    }
    data, _ := os.ReadFile(filepath.Join(sub, "settings.json"))
    if len(data) <= 2 {
        t.Errorf("expected non-empty content, got %q", data)
    }
}
```

- [ ] **Step 2: Run test (fails)**

```bash
go test ./runner/ -run TestWriteAgentSettings -v
```

Expected: build error.

- [ ] **Step 3: Implement `runner/settings.go`**

```go
package runner

import (
    "encoding/json"
    "os"
    "path/filepath"
)

// agentSettings is the schema written to <worktree>/.claude/settings.json.
type agentSettings struct {
    Hooks map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
    Hooks []hookSpec `json:"hooks"`
}

type hookSpec struct {
    Type    string `json:"type"`
    Command string `json:"command"`
}

// WriteAgentSettings creates <dir>/.claude/settings.json with the
// UserPromptSubmit hook that injects pending agentboard messages each turn.
func WriteAgentSettings(worktreeDir string) error {
    s := agentSettings{
        Hooks: map[string][]hookGroup{
            "UserPromptSubmit": {{
                Hooks: []hookSpec{{
                    Type:    "command",
                    Command: "harness-cli agent inbox --since-last --json",
                }},
            }},
        },
    }
    sub := filepath.Join(worktreeDir, ".claude")
    if err := os.MkdirAll(sub, 0o755); err != nil {
        return err
    }
    data, err := json.MarshalIndent(s, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(filepath.Join(sub, "settings.json"), data, 0o644)
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./runner/ -run TestWriteAgentSettings -v
```

Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add runner/settings.go runner/settings_test.go
git commit -m "runner: WriteAgentSettings emits .claude/settings.json with inbox hook"
```

---

### Task 4: Session passes ticket + env to Process (oneshot)

**Files:**
- Modify: `runner/session.go`
- Modify: `runner/session_test.go`
- Modify: `runner/connect.go` (pass ServerCID into Session)

- [ ] **Step 1: Add ServerCID + RunnerID to Session struct**

`runner/session.go`:

```go
type Session struct {
    AllowedRoots    []string
    ClaudeBin       string
    ExtraClaudeArgs []string
    Timeout         time.Duration
    Sender          Sender
    Streams         peer.BidirectionalStreamLookup
    Logger          *slog.Logger
    Now             func() time.Time

    // NEW: required for env injection
    ServerCID objproto.ConnectionID
    Hostname  string
    WSPath    string

    mu                  sync.Mutex
    tasks               map[string]*taskEntry
    wms                 map[string]*WorktreeManager
    testHookHandleAssign func()
}
```

`runner/connect.go` の `Session` 構築箇所で同フィールドを埋める:

```go
session := &Session{
    AllowedRoots:    cfg.AllowedRoots,
    ClaudeBin:       cfg.ClaudeBin,
    ExtraClaudeArgs: cfg.ExtraClaudeArgs,
    ServerCID:       cfg.ServerCID,
    Hostname:        cfg.Hostname,
    WSPath:          cli.WebSocketPath,
    Sender:          sender,
    Streams:         pc.Transport(),
    Logger:          cfg.Logger,
    Now:             time.Now,
}
```

- [ ] **Step 2: handleAssign extracts ticket and builds env**

`Session.handleAssign` 内、`wm.Create` 後 / `proc := &Process{...}` の前に:

```go
if err := WriteAgentSettings(dir); err != nil {
    s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
    // Non-fatal: claude still works without the hook (just no auto-inject).
}

env := BuildAgentEnv(AgentEnvSpec{
    ServerCID:  s.ServerCID,
    RunnerID:   s.Sender.ID(),
    TaskID:     req.TaskId,
    RepoPath:   repoPath,
    Hostname:   s.Hostname,
    WSPath:     s.WSPath,
    AuthTicket: req.AuthTicket,
})

proc := &Process{
    ClaudeBin: s.ClaudeBin,
    CWD:       dir,
    Timeout:   s.Timeout,
    ExtraArgs: s.ExtraClaudeArgs,
    Env:       env,
}
```

- [ ] **Step 3: Update session_test.go fixtures**

既存 `Session` 構築箇所すべてに、テスト用 `ServerCID`, `Hostname`, `WSPath` のダミー値を入れる:

```go
sess := &Session{
    // ... existing fields ...
    ServerCID: mustParseCID(t, "ws:127.0.0.1:1-1"),
    Hostname:  "test-host",
    WSPath:    "/ws",
}
```

`mustParseCID` は agentenv_test.go のものを再利用 (内部 helper)。

- [ ] **Step 4: Add new test for env propagation**

```go
func TestHandleAssign_PassesAuthTicketViaEnv(t *testing.T) {
    fake := writeFakeClaude(t, `#!/usr/bin/env bash
echo "TICKET=$HARNESS_AUTH_TICKET"
echo "TASK=$HARNESS_TASK_ID"`)
    sess := newTestSession(t, fake) // helper that fills ServerCID/Hostname/WSPath
    var taskID protocol.TaskID
    taskID.Id[0] = 0xAB
    var ticket [16]byte
    ticket[0] = 0xCD
    req := &protocol.AssignTask{
        TaskId:     taskID,
        AuthTicket: ticket,
        RepoPath:   []byte(sess.AllowedRoots[0]),
        Prompt:     []byte("hi"),
    }
    sess.handleAssign(context.Background(), req)
    out := capturedTaskLog(t, sess) // helper that pulls Sender.Publish bytes
    expectContains(t, out, "TICKET=cd"+strings.Repeat("00", 15))
    expectContains(t, out, "TASK=ab"+strings.Repeat("00", 15))
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./runner/ -v
```

Expected: 既存 + 新規テスト pass。

- [ ] **Step 6: Commit**

```bash
git add runner/session.go runner/session_test.go runner/connect.go
git commit -m "runner/session: inject HARNESS_* env + write .claude/settings.json on assign"
```

---

### Task 5: Session passes ticket + env to ExecuteCommand (interactive)

**Files:**
- Modify: `runner/session.go` (handleOpenExec)
- Modify: `exec/exec.go` (ExecuteCommand signature)

- [ ] **Step 1: Extend `agentexec.ExecuteCommand` signature**

`exec/exec.go` の `func ExecuteCommand(ctx, stream, log, claudeBin, extraArgs, cwd, alloc)` に `extraEnv []string` 引数を追加:

```go
func ExecuteCommand(
    ctx context.Context, stream Bidirectional, log *slog.Logger,
    claudeBin string, extraArgs []string,
    cwd string, alloc bool,
    extraEnv []string,  // NEW
) error {
    // ... inside, cmd.Env = append(os.Environ(), extraEnv...)
}
```

すべての caller (`handleOpenExec`, exec 単体テストなど) を更新する。

- [ ] **Step 2: Verify build (callers fail)**

```bash
go build ./...
```

Expected: caller の引数不足でエラー → 順に修正。

- [ ] **Step 3: handleOpenExec builds env + writes settings**

`Session.handleOpenExec` 内、`stream` 取得後 / `wm.Create` 後 / `ExecuteCommand` 前に:

```go
if err := WriteAgentSettings(dir); err != nil {
    log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
}

env := BuildAgentEnv(AgentEnvSpec{
    ServerCID:  s.ServerCID,
    RunnerID:   s.Sender.ID(),
    TaskID:     oer.TaskId,
    RepoPath:   repoPath,
    Hostname:   s.Hostname,
    WSPath:     s.WSPath,
    AuthTicket: oer.AuthTicket,
})

runErr := agentexec.ExecuteCommand(taskCtx, stream, log, s.ClaudeBin, s.ExtraClaudeArgs, dir, true, env)
```

- [ ] **Step 4: Add test for OpenExec env**

```go
func TestHandleOpenExec_PassesAuthTicketViaEnv(t *testing.T) {
    // 既存の OpenExec テスト fixture を流用、stream 経由で claude が
    // env 内の HARNESS_AUTH_TICKET を echo するスクリプトをテスト。
    // 既存 handleOpenExec 系テスト (session_test.go) を参考に組む。
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./runner/ ./exec/ -v
```

Expected: 全 pass。

- [ ] **Step 6: Commit**

```bash
git add runner/session.go exec/exec.go runner/session_test.go
git commit -m "runner+exec: inject HARNESS_* env into interactive PTY claude"
```

---

### Task 6: Empty hostname / WSPath edge case

**Files:**
- Modify: `runner/session.go` (defensive)
- Modify: `runner/agentenv_test.go` (cover edge case)

- [ ] **Step 1: Add test for empty hostname**

```go
func TestHandleAssign_EmptyHostnameDoesNotEmitEnvKey(t *testing.T) {
    sess := newTestSession(t, fakeEcho(t, "HARNESS_HOSTNAME=$HARNESS_HOSTNAME"))
    sess.Hostname = ""
    // ... build req, run handleAssign ...
    out := capturedTaskLog(t, sess)
    // claude prints "HARNESS_HOSTNAME=" (env unset) — confirm BuildAgentEnv didn't emit it
    if strings.Contains(out, "HARNESS_HOSTNAME=test-host") {
        t.Errorf("hostname leaked into env when empty")
    }
}
```

- [ ] **Step 2: Confirm BuildAgentEnv already handles this** (Task 2 で実装済) **→ test passes**

```bash
go test ./runner/ -run TestHandleAssign_EmptyHostname -v
```

Expected: PASS。

- [ ] **Step 3: Commit**

```bash
git add runner/session_test.go
git commit -m "runner: cover empty Hostname env behavior"
```

---

### Task 7: Smoke test full chain

**Files:**
- Modify: `runner/session_test.go` (add integration-flavor test)

- [ ] **Step 1: Add integration test that exercises Assign → settings.json + env**

```go
func TestHandleAssign_WritesSettingsJsonAndPropagatesEnv(t *testing.T) {
    sess := newTestSession(t, writeFakeClaude(t, `#!/usr/bin/env bash
ls -la .claude/settings.json
cat .claude/settings.json | head -3
echo "TICKET=$HARNESS_AUTH_TICKET"`))

    var taskID protocol.TaskID
    taskID.Id[0] = 1
    var ticket [16]byte
    ticket[0] = 0xFE; ticket[15] = 0xED
    req := &protocol.AssignTask{
        TaskId:     taskID,
        AuthTicket: ticket,
        RepoPath:   []byte(sess.AllowedRoots[0]),
        Prompt:     []byte("hi"),
    }
    sess.handleAssign(context.Background(), req)

    out := capturedTaskLog(t, sess)
    if !strings.Contains(out, ".claude/settings.json") {
        t.Error("settings.json not written")
    }
    if !strings.Contains(out, "UserPromptSubmit") {
        t.Error("settings.json content missing UserPromptSubmit hook")
    }
    if !strings.Contains(out, "TICKET=fe000000000000000000000000000000ed"[:34]) {
        t.Error("ticket env not visible to claude")
    }
}
```

- [ ] **Step 2: Run**

```bash
go test ./runner/ -run TestHandleAssign_WritesSettings -v
```

Expected: PASS。

- [ ] **Step 3: Commit**

```bash
git add runner/session_test.go
git commit -m "runner: integration test for settings.json + env injection"
```

---

## Self-review checklist

- [ ] `Process.Env` を merge する際、`os.Environ()` を捨てていないか? (PATH 等が消えると claude 自身が動かなくなる)
- [ ] `WriteAgentSettings` 失敗は **non-fatal** で warn ログ + 継続。task 自体は失敗させない (hook は加点機能)
- [ ] `os.Setenv` を使っていないか? 並行 task 安全性に違反する
- [ ] `ExecuteCommand` の caller 全箇所が新引数で更新されているか? (`go build ./...` で確認)
- [ ] interactive 経路でも settings.json が worktree に出力されているか?
- [ ] empty `Hostname` で `HARNESS_HOSTNAME=` (空値 emit) ではなく env key 自体が無いか?

---

## Done definition

- `go test ./runner/ ./exec/ -v` 全 pass
- `go build ./...` クリーン
- 手動 dogfood: agent-runner 起動 → submit → claude 内 (実際の binary) で `env | grep HARNESS_` で全 env が見え、`.claude/settings.json` が worktree 内に存在する
