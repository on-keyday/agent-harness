# Parallel AI Coding Agent Harness — Design (v1)

Status: draft, pending user review
Date: 2026-04-25

## 1. Goal

複数の Claude Code CLI インスタンスを並列で起動・管理し、user が「タスクを投げる」「状況を眺める」「成果物を拾う」を 1 つの窓口から行える harness を作る。

v1 は single-host / local-only の **task dispatcher** として動くところまで。

## 2. Non-goals (v1)

- Interactive session の attach/detach (v2 で B 型として追加)
- Remote host への runner 分散 (transport は準備済、v2 で endpoint 切替のみ)
- `objproto.Proxy` / `Probe` 機能の活用 (v2)
- Claude 以外の agent (Codex / Cursor / OpenHands) への対応
- Runner auto-spawn / process pool supervisor
- Server remote kick (ssh で runner を起こす等)

## 3. Existing assets

既存コードで以下は実装済み。harness はこれらの上に乗る:

| Layer | 責務 | MVP での用途 |
|---|---|---|
| `objproto` | ECDH + AES-GCM 暗号化 secure session (1 WS = 1 connection) | runner / cli ↔ server の下敷き |
| `trsf` | QUIC 風 stream 多重化 + flow/congestion/MTU/ACK | log / control channel の運搬 |
| `trsf/wire.ApplicationPayloadKind` | `Pubsub` / `TaskControl` / `RunnerControl` / `RelayControl` / Ping 枠 | `TaskControl` / `RunnerControl` を埋める |
| `pubsub` | topic-based JOIN/LEAVE、topic 毎の trsf bidirectional stream | event broadcast の主機構 |

`RelayControl` は v1 未使用 (v2 分散化向けに予約)。

## 4. Architecture

```
┌──────────────┐         ┌───────────────────┐         ┌──────────────────┐
│ harness-cli  │─ WS ───▶│ harness-server    │◀── WS ──│ agent-runner     │ × N
│  submit /    │         │                   │         │  (spawn claude,  │
│  ls / logs / │         │  TaskQueue        │         │   stream logs)   │
│  watch       │         │  RunnerRegistry   │         └──────────────────┘
└──────────────┘         │  PubSub broker    │
                         │  AppendLog (WAL)  │
                         └───────────────────┘
```

### 配置の意図

- **server を唯一の truth source** にして runner / cli を stateless に近づける。v2 分散時に runner を mobile にできる。
- **runner は worker machine 1 プロセス = 1 タスク同時実行**。並列度は runner process 数で稼ぐ (user が N 回起動)。
- **worktree per task** で pain point D (リソース衝突) を構造で排除。

## 5. Components

### 5.1 harness-server (hub)

- 1 台で 1 プロセス、`ws://host:port` を listen
- 接続ごとに「cli」か「runner」かを `Hello` の中身で識別 (後述)
- 保持する state:
  - `RunnerRegistry`: `RunnerID -> Runner`
  - `TaskQueue`: `TaskID -> Task`, pending queue は status=Queued の FIFO
  - `PubSub broker`: 既存 `pubsub.PubSub` をそのまま利用
- scheduler: Queued task を、`repo_path` が一致する Idle runner に FIFO で assign。同 `repo_path` の Idle runner が複数いる場合は接続順で古い方を優先。

### 5.2 agent-runner (worker)

- 起動時引数: `--server ws://...`, `--repo /abs/path`
- 起動後は `Hello { repo_path, version }` を送って idle 状態に入り `AssignTask` を待つ
- 1 task 同時実行上限 1。実行中に `AssignTask` が来ても server 側 scheduler でそもそも assign されない前提
- task 実行フロー (per-task goroutine):
  1. `git worktree add <repo>/.harness-worktrees/<task-id> -b harness/<task-id>` (base は **task assign 時点**の `HEAD`、runner 起動時ではない)
  2. pubsub JOIN `task.<id>.log`
  3. `exec claude -p "<prompt>"` を worktree cwd で起動 (stdin close、env は runner 継承)
  4. stdout / stderr 2 goroutine で読み、各行に `[out]` / `[err]` prefix を付けて topic にそのまま publish
  5. process exit 待ち (timeout 固定 30min、超過で SIGTERM → 5s 後 SIGKILL。submit 側からの override は v1 なし)
  6. `TaskFinished { exit_code, diff_sha? }` を返す。`diff_sha` の扱いは §9 参照
  7. pubsub LEAVE
- worktree は **自動削除しない**。`harness prune` で明示削除 (v1 minimal)

### 5.3 harness-cli (user frontend)

- subcommand:
  - `harness submit --repo <path> --task "<prompt>"` → `task_id` を print
  - `harness ls` → runner 一覧 + task 一覧 (最新 N)
  - `harness logs <task-id> [-f]` → 該当 topic subscribe、tail 風に表示
  - `harness watch` → `tasks.status` + `runners.status` を subscribe して流れる表示 (v1 は TUI 凝らず素 stream で可)
  - `harness cancel <task-id>`
  - `harness prune [--before=7d]` → 完了済み task の worktree 削除

## 6. Data model

```go
type RunnerID   string // = objproto.ConnectionID の文字列表現
type TaskID     string // ULID, server 発番

type RunnerStatus int // Offline / Idle / Busy
type TaskStatus   int // Queued / Running / Succeeded / Failed / Cancelled

type Runner struct {
    ID          RunnerID
    RepoPath    string       // 不変 (runner 起動時に宣言)
    Status      RunnerStatus
    CurrentTask TaskID       // Busy のとき非空
    ConnectedAt time.Time
    LastSeen    time.Time
}

type Task struct {
    ID          TaskID
    RepoPath    string        // runner マッチング用
    Prompt      string
    Status      TaskStatus
    AssignedTo  RunnerID      // Running 以降で set
    WorktreeDir string        // runner からの絶対 path
    CreatedAt   time.Time
    StartedAt   *time.Time
    EndedAt     *time.Time
    ExitCode    *int
    DiffSHA     string        // §9 の扱い次第で空のまま
}
```

成果物 (log / blob) は server DB に保存しない。log は append-only WAL へ、blob は worktree に git object として残す。

## 7. Protocol

### 7.1 wire kind 割当

| Kind | 用途 | 形 |
|---|---|---|
| `RunnerControl` | runner 登録 / task accept・finish 通知 / heartbeat | 単発 request / response |
| `TaskControl` | cli からの submit / list / cancel | 単発 request / response |
| `Pubsub` | log / status event 配信 | topic stream (既存) |
| `RelayControl` | v1 未使用 | — |

Schema 記述は **`.bgn` (brgen)** で統一 (`pubsub/protocol/message.bgn` と同じ流儀)。

### 7.2 RunnerControl messages

Runner → Server:
```
Hello          { repo_path: string, version: string }
TaskAccepted   { task_id: string }
TaskFinished   { task_id: string, exit_code: i32, diff_sha: string }
Heartbeat      { ts: u64 }
```

Server → Runner:
```
AssignTask     { task_id: string, prompt: string }
CancelTask     { task_id: string }
```

### 7.3 TaskControl messages

CLI → Server:
```
Submit         { repo_path: string, prompt: string }  → { task_id: string }
List           { limit?: u32 }                        → { runners: []Runner, tasks: []Task }
Cancel         { task_id: string }                    → { ok: bool, reason?: string }
```

Log follow 系は TaskControl には置かず pubsub に委譲。

### 7.4 Pubsub topic 命名

```
runners.status              全 runner の Idle/Busy/Offline 遷移
tasks.status                全 task の状態遷移 event
task.<task-id>.log          byte stream (行 prefix "[out]"/"[err]" 付き plain text)
task.<task-id>.status       該当 task の細粒度 event (started / ended)
```

- 全 topic は UTF-8 文字列、`.` 区切り。
- `task.<id>.log` は **fanout**: runner が publisher、server (persist 用) と cli (follow 用) が subscriber。
- 他 topic は server が publisher。

## 8. State & fault tolerance

### 8.1 Persistence (v1)

- Runner / Task registry は in-memory map
- `<data_dir>` は server 起動時引数 `--data-dir`、既定 `./harness-data/`
- Task の発番と状態遷移だけ **JSONL WAL** に append (`<data_dir>/events.log`)
- Log chunks は server ローカルに `<data_dir>/logs/<task-id>.log` として追記保存 (pubsub subscriber 経由)
- `runners.status` / `tasks.status` event の payload schema も `.bgn` で定義 (実装計画側で詳細)
- server 再起動: WAL replay で Queued/Running を再構築、Running は `Failed("server restart")` にマーク

### 8.2 障害ケース

| ケース | 振る舞い |
|---|---|
| runner disconnect (task 実行中) | task を `Failed("runner disconnected")` に。worktree は runner disk に残る |
| runner disconnect (idle) | Offline 扱い。再接続 (= 新 ConnectionID) は別 runner として見える |
| claude hang | runner task timeout → SIGTERM → SIGKILL → `TaskFinished(exit=-1)` |
| server 再起動 | 全 runner 切断 → 再接続で再登録。走行中 task は Failed |
| pubsub subscriber 詰まり | trsf flow window で自然に back-pressure、publish 側 block |

### 8.3 監視

- `harness ls` / `harness watch` のみ。server の自己 metrics は v1 不要。

## 9. Open questions

### 9.1 Worktree への auto-commit 扱い

runner が task 終了時に `git add -A && git commit` を自動で打つかは未決。

選択肢:
- **(a) auto-commit する**: `diff_sha` が常に得られる、user はあとで cherry-pick / reset しやすい。ただし user が意図しない commit を踏んでしまうリスク。
- **(b) 触らない**: 変更は worktree に dirty のまま残す。user 側で確認して commit。`diff_sha` は空。
- **(c) opt-in フラグ**: submit 時に `--auto-commit` を渡したときだけ (a) 相当。

v1 としてはどれにするか、実装開始前に決める。

## 10. Testing strategy

- **Unit**: server scheduling、runner worktree 操作、protocol bgn encode/decode の round-trip
- **Integration**: server + runner 2 個を同一プロセスで立ち上げ、submit → 成果物まで走らせる E2E。`claude` は fake binary (shell script) を `--claude-bin` で差し替え可能に
- **Smoke**: 本物の `claude -p` を 1 task 投げて通しで確認

## 11. v2 roadmap (参考)

- **Session multiplexer (B 型)**: runner に PTY 多重化レイヤを追加、`harness attach <task-id>` で interactive session を引き継げる
- **LAN 分散**: server endpoint を外部に晒し、minipc 等に runner を常駐。`RelayControl` を使った agent 間 messaging も解禁
- **Runner auto-spawn**: `harness-pool --size N` supervisor
- **SSH/remote kick**: server から runner を自動起動

## 12. Implementation order (hint for planning)

1. bgn schema を `RunnerControl` / `TaskControl` 用に書く、`stream.bgn` の kind と整合
2. server: `RunnerControl` / `TaskControl` の dispatch を埋める、registry + queue を実装
3. runner: worktree 作成 + claude exec + log publish の単体動作
4. cli: submit / ls / logs / cancel
5. server: WAL + log persist
6. cli: watch (pubsub consumer)
7. E2E integration test
