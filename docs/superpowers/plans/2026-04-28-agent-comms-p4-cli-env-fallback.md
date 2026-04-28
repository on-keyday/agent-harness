# Agent Comms P4: Existing harness-cli env Fallback Retrofit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 既存 `harness-cli` の `submit`, `interactive`, `ls`, `watch`, `cancel`, `prune`, `prune-local`, `logs` の各 subcommand に env fallback (`HARNESS_SERVER_CID`, `HARNESS_WS_PATH`, `HARNESS_REPO_PATH`) を入れ、人間 user が flag 省略で叩けるようにする。

**Architecture:** P3 で導入した `cli/cliopts.ResolveServerCID` 等の helper を `cmd/harness-cli/main.go` の各 subcommand から再利用する。既存テストは flag 渡しのまま壊れないことを保証する (flag > env の優先順位)。

**Tech Stack:** Go 1.25.7、`flag`、既存 `cli/cliopts` パッケージ (P3 完了済)、既存 `cli/` subcommand 群。Spec: `docs/superpowers/specs/2026-04-28-agent-comms-design.md` §9.4。

---

## Reference for implementers

### 前提

- P3 が完了している (`cli/cliopts/` パッケージが利用可能)

### スコープ

このプランは **既存 subcommand の挙動拡張のみ**。新 subcommand 追加・wire protocol 変更・packaging 変更はしない。**flag 渡しのみで動いていた既存ユーザのワークフローは何も変わってはいけない**。

### 対応 env list

| env | 同名 flag | 適用 subcommand |
|---|---|---|
| `HARNESS_SERVER_CID` | `--server-cid` | 全 subcommand |
| `HARNESS_WS_PATH` | `--ws-path` | 全 subcommand (現状 global flag) |
| `HARNESS_REPO_PATH` | `--repo` | submit / interactive / prune-local |

`HARNESS_TASK_ID`, `HARNESS_RUNNER_ID`, `HARNESS_AUTH_TICKET` などは **agent CLI 専用**で、submit/interactive 等の人間 CLI とは無関係なので **追加しない**。

### resolve helper API (P3 で実装済の前提)

```go
cliopts.ResolveServerCID(flagVal string) (objproto.ConnectionID, error)
cliopts.ResolveString(flagVal, envName string) string
```

`HARNESS_REPO_PATH` の resolve はこの汎用 helper で十分:

```go
repo := cliopts.ResolveString(*repoFlag, "HARNESS_REPO_PATH")
if repo == "" { /* error */ }
```

`HARNESS_WS_PATH` も同様。

---

## File structure

### Modify

```
cmd/harness-cli/main.go      # 全 subcommand で resolveServerCID + env fallback 適用
```

### Create

```
cmd/harness-cli/main_test.go # subcommand 起動の integration スモーク (env vs flag 優先順位)
```

---

## Tasks

### Task 1: Replace `parseCID()` closure with `cliopts.ResolveServerCID`

**Files:**
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: Remove inline parseCID, replace with cliopts call**

現状:

```go
serverCID := flag.String("server-cid", "ws:127.0.0.1:8539-*", "...")

parseCID := func() objproto.ConnectionID {
    peerCID, err := objproto.ParseConnectionID(*serverCID, ...)
    if err != nil {
        die(fmt.Errorf("server-cid: %w", err))
    }
    return peerCID
}
```

これを下記に置換:

```go
serverCID := flag.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID; default ws:127.0.0.1:8539-*)")

parseCID := func() objproto.ConnectionID {
    val := *serverCID
    if val == "" && os.Getenv("HARNESS_SERVER_CID") == "" {
        val = "ws:127.0.0.1:8539-*"  // backward-compatible default
    }
    cid, err := cliopts.ResolveServerCID(val)
    if err != nil {
        die(err)
    }
    return cid
}
```

注: 既存 default 値 `ws:127.0.0.1:8539-*` を維持するため、flag も env も空のときのみ default を当てる。env が指定されていれば env を尊重 (cliopts.ResolveServerCID の挙動)。

- [ ] **Step 2: Add cliopts import**

```go
import (
    // ... existing ...
    "github.com/on-keyday/agent-harness/cli/cliopts"
)
```

- [ ] **Step 3: Build**

```bash
go build ./cmd/harness-cli/
```

Expected: クリーン。

- [ ] **Step 4: Smoke**

```bash
# default (no flag, no env) — should still work as before
./harness-cli ls 2>&1 | head -3
# env override
HARNESS_SERVER_CID=ws:127.0.0.1:8539-* ./harness-cli ls 2>&1 | head -3
# flag override beats env
HARNESS_SERVER_CID=ws:bad-host:1-1 ./harness-cli --server-cid=ws:127.0.0.1:8539-* ls 2>&1 | head -3
```

3 ケースすべて、server に到達するところまで進む (server 未起動なら接続エラーで止まるが、parse には成功している) ことを目視確認。

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "harness-cli: --server-cid env fallback via cliopts"
```

---

### Task 2: env fallback for `--ws-path`

**Files:**
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: Update wsPath resolve**

```go
wsPath := flag.String("ws-path", "", "WebSocket path (env: HARNESS_WS_PATH; default /ws)")
```

`flag.Parse()` の直後:

```go
resolved := cliopts.ResolveString(*wsPath, "HARNESS_WS_PATH")
if resolved == "" {
    resolved = "/ws"
}
cli.WebSocketPath = resolved
```

- [ ] **Step 2: Build + smoke**

```bash
go build ./cmd/harness-cli/
HARNESS_WS_PATH=/custom ./harness-cli ls 2>&1 | head -3
```

Expected: server 接続時に path `/custom` が使われる (server 側でログを見るか、対応 server を立てて確認)。

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "harness-cli: --ws-path env fallback (HARNESS_WS_PATH)"
```

---

### Task 3: env fallback for `--repo` on submit/interactive/prune-local

**Files:**
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: submit case**

```go
case "submit":
    fs := flag.NewFlagSet("submit", flag.ExitOnError)
    repo := fs.String("repo", "", "repo identifier (env: HARNESS_REPO_PATH)")
    task := fs.String("task", "", "prompt text")
    fs.Parse(args)
    if *task == "" {
        fmt.Fprintln(os.Stderr, "submit: --task is required")
        os.Exit(2)
    }
    repoVal := cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")
    if repoVal == "" {
        fmt.Fprintln(os.Stderr, "submit: --repo or HARNESS_REPO_PATH required")
        os.Exit(2)
    }
    // ... use repoVal in c.Submit(ctx, repoVal, *task) ...
```

- [ ] **Step 2: interactive case**

```go
case "interactive":
    fs := flag.NewFlagSet("interactive", flag.ExitOnError)
    repo := fs.String("repo", "", "repo identifier (env: HARNESS_REPO_PATH)")
    fs.Parse(args)
    repoVal := cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")
    if repoVal == "" {
        fmt.Fprintln(os.Stderr, "interactive: --repo or HARNESS_REPO_PATH required")
        os.Exit(2)
    }
    // ... use repoVal in c.Interactive(ctx, repoVal) ...
```

- [ ] **Step 3: prune-local case**

```go
case "prune-local":
    fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
    repo := fs.String("repo", ".", "repo to prune (env: HARNESS_REPO_PATH)")
    before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
    fs.Parse(args)
    repoVal := *repo
    if repoVal == "." {
        if env := os.Getenv("HARNESS_REPO_PATH"); env != "" {
            repoVal = env
        }
    }
    abs, err := filepath.Abs(repoVal)
    // ... rest unchanged ...
```

注: prune-local は default が `.` なので、env を見るのは flag が default のときのみ。flag が明示指定されていれば env を見ない (cliopts.ResolveString と微妙に違うので inline で書く)。

- [ ] **Step 4: Build + smoke**

```bash
go build ./cmd/harness-cli/
HARNESS_REPO_PATH=/tmp/foo ./harness-cli submit --task "hi"  # → "/tmp/foo" が使われる
./harness-cli submit --repo /tmp/bar --task "hi"             # → "/tmp/bar" (flag 優先)
HARNESS_REPO_PATH=/tmp/foo ./harness-cli submit --repo /tmp/bar --task "hi"  # → "/tmp/bar"
```

stderr ログ等で repo path が反映されているか確認。

- [ ] **Step 5: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "harness-cli: --repo env fallback (HARNESS_REPO_PATH) for submit/interactive/prune-local"
```

---

### Task 4: Integration test for env vs flag priority

**Files:**
- Create: `cmd/harness-cli/main_test.go`

- [ ] **Step 1: Write tests**

```go
package main_test

import (
    "os/exec"
    "strings"
    "testing"
)

// TestCLI_ServerCIDFlagBeatsEnv: with conflicting flag and env, flag wins.
func TestCLI_ServerCIDFlagBeatsEnv(t *testing.T) {
    cmd := exec.Command("go", "run", ".", "--server-cid=ws:127.0.0.1:99999-1", "ls")
    cmd.Env = append(cmd.Env, "HARNESS_SERVER_CID=ws:bad-host-from-env:1-1")
    cmd.Dir = "."
    out, _ := cmd.CombinedOutput()
    // We only need to confirm flag was parsed; connection will fail since no server is running.
    // The error message should mention the flag value, not the env value.
    if strings.Contains(string(out), "bad-host-from-env") {
        t.Errorf("env value leaked into resolved CID: %s", out)
    }
}

// TestCLI_ServerCIDEnvFallback: with no flag, env is used.
func TestCLI_ServerCIDEnvFallback(t *testing.T) {
    cmd := exec.Command("go", "run", ".", "ls")
    cmd.Env = append(cmd.Env, "HARNESS_SERVER_CID=ws:127.0.0.1:8539-*")
    cmd.Dir = "."
    out, _ := cmd.CombinedOutput()
    // Should not error with "server-cid required" since env supplies it.
    if strings.Contains(string(out), "server-cid required") {
        t.Errorf("env fallback not applied: %s", out)
    }
}

// TestCLI_RepoFlagWithEnvSet: flag wins for submit.
func TestCLI_RepoFlagWithEnvSet(t *testing.T) {
    cmd := exec.Command("go", "run", ".", "submit", "--repo=/tmp/from-flag", "--task=x")
    cmd.Env = append(cmd.Env, "HARNESS_REPO_PATH=/tmp/from-env")
    cmd.Dir = "."
    out, _ := cmd.CombinedOutput()
    // The connection will fail (no server) but the parse should accept it.
    // Server-side error should reference repo path; we want to ensure flag was used.
    if strings.Contains(string(out), "/tmp/from-env") && !strings.Contains(string(out), "/tmp/from-flag") {
        t.Errorf("env beat flag: %s", out)
    }
}

// TestCLI_RepoEnvFallback: no flag, env supplies repo.
func TestCLI_RepoEnvFallback(t *testing.T) {
    cmd := exec.Command("go", "run", ".", "submit", "--task=x")
    cmd.Env = append(cmd.Env, "HARNESS_REPO_PATH=/tmp/from-env")
    cmd.Dir = "."
    out, _ := cmd.CombinedOutput()
    if strings.Contains(string(out), "--repo or HARNESS_REPO_PATH required") {
        t.Errorf("env fallback not applied: %s", out)
    }
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cmd/harness-cli/ -v -timeout 60s
```

Expected: 4 tests pass. `go run` の起動が遅いので timeout 余裕を持たせる。

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-cli/main_test.go
git commit -m "harness-cli: integration test for flag/env priority (server-cid, repo)"
```

---

### Task 5: Update usage / help text

**Files:**
- Modify: `cmd/harness-cli/main.go` (`usage` function)

- [ ] **Step 1: Update usage strings**

```go
func usage() {
    fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] [--ws-path PATH] <subcommand> [args]")
    fmt.Fprintln(os.Stderr, "")
    fmt.Fprintln(os.Stderr, "Global flags fall back to env when omitted:")
    fmt.Fprintln(os.Stderr, "  --server-cid  HARNESS_SERVER_CID  (default ws:127.0.0.1:8539-*)")
    fmt.Fprintln(os.Stderr, "  --ws-path     HARNESS_WS_PATH     (default /ws)")
    fmt.Fprintln(os.Stderr, "")
    fmt.Fprintln(os.Stderr, "Subcommands:")
    fmt.Fprintln(os.Stderr, "  submit --repo REPO --task TEXT      enqueue a task (--repo: HARNESS_REPO_PATH)")
    fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
    fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
    fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
    fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]")
    fmt.Fprintln(os.Stderr, "                                      remove old worktrees in <repo>/.harness-worktrees/")
    fmt.Fprintln(os.Stderr, "  logs TASK_ID                        stream task log output")
    fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
    fmt.Fprintln(os.Stderr, "  interactive --repo REPO             attach an interactive PTY claude (--repo: HARNESS_REPO_PATH)")
    fmt.Fprintln(os.Stderr, "  agent {send|wait|inbox|subscribe|unsubscribe|dispatch}")
    fmt.Fprintln(os.Stderr, "                                      agent-to-agent message ops (env-primary; HARNESS_AUTH_TICKET required)")
}
```

- [ ] **Step 2: Smoke**

```bash
./harness-cli 2>&1 | head -20
```

Expected: 新しい usage が表示される。

- [ ] **Step 3: Commit**

```bash
git add cmd/harness-cli/main.go
git commit -m "harness-cli: update usage to document env fallback + agent subcommand"
```

---

## Self-review checklist

- [ ] **既存 dogfood ワークフローの非破壊**: flag のみで叩いていた使い方は何も変わっていないか? 全 subcommand で flag を渡したケースを smoke 確認
- [ ] flag が **明示的に** 渡されているとき env は無視されるか? (priority: flag > env > default)
- [ ] flag が default 値のままで env が空の場合、既存 default (`ws:127.0.0.1:8539-*`, `/ws`, `.`) が維持されているか?
- [ ] 既存 `cli` パッケージ (`cli/submit.go`, `cli/list.go` 等) には触っていないか? このプランの修正は `cmd/harness-cli/main.go` のみで完結する
- [ ] `prune-local` の `--repo` default `.` の扱いが特殊なので、test で confirm したか?

---

## Done definition

- `go test ./cmd/harness-cli/ -v` 全 pass
- `go build ./...` クリーン
- 既存 `harness-cli submit --repo X --task Y` 等のワークフローは無変更で動く (回帰なし)
- `~/.bashrc` に `export HARNESS_SERVER_CID=ws:my-host:8539-*` を入れて全 subcommand から flag 省略可能
- `harness-cli` の usage に env fallback と `agent` subcommand が記載されている
