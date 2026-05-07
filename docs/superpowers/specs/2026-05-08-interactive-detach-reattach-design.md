# Interactive PTY Detach / Reattach — Design

Status: draft, pending user review
Date: 2026-05-08

## 1. Goal

`harness-cli interactive` (PTY claude session) は client 切断時に runner 側 SIGHUP→SIGTERM→SIGKILL ladder で claude を kill する設計のため、user は対話継続のためにターミナルを開きっぱなしにする必要がある。3 ホスト分散構成 (Windows client / Raspberry Pi server / gmkhost runner) では Windows PC の起動継続が運用負荷になっている。

本 spec は v1 design (`2026-04-25-parallel-agent-harness-design.md` §11) で v2 ロードマップとして言及されていた **Session multiplexer (B 型)** を実装する。tmux 的な detach / reattach を interactive PTY セッションに与える。

## 2. Non-goals

- 既存 `harness-cli interactive` (kill-on-disconnect) の挙動変更。完全保存。
- 複数 client の同時 read/write attach (broadcast attach)。
- VT エミュレータベースの screen snapshot 機構。
- runner-side PTY multiplexer。
- session の runner 間 migration (1 セッションは 1 runner に固定)。
- auth ticket 寿命の厳密化 (`project_protocol_stack_scope` の toy-scope 前提を維持)。
- 段階的 rollout / capability 切替 (個人 dogfood、サーバ更新時に他全コンポーネントも置き換える運用)。

## 3. Existing architecture (要点)

`server/task_handler.go:handleOpenInteractive` は **既に server-as-proxy 構造**:

```
client ──tuiStream──▶ server ──runnerStream──▶ runner ──PTY──▶ claude
       (bidi trsf)            (bidi trsf)
                       │
                       └─ spliceBidi: 双方向 byte ポンプ
```

`spliceBidi` (`server/task_handler.go:512`) は片方の stream EOF で teardown→両方 close→runner 側の `exec.ExecuteCommand` (`exec/exec.go:205-221`) が `io.Copy(p, pipeOut)` の EOF を検知して SIGHUP→SIGTERM→SIGKILL ladder で claude を reap する。本設計はこの "片方閉じたら全部閉じる" の中で **server 側で runnerStream を閉じない**ように変える: runner にとっては stream が生きたままなので EOF が来ず、SIGHUP ladder が発火しない。**よって runner 側に functional 変更は要らない**。

## 4. UX

### 4.1 Subcommand (CLI)

```
harness-cli session new --repo PATH [--runner SEL] [--claude-arg ...]
    detachable な PTY claude を起動。そのまま attach (= 既存 interactive と同じ起動体験)。
    プリント: session id (= task id) と "attached"。

harness-cli session attach <session-id>
    detached / running な session に接続。ring buffer replay → live splice。
    既存 attach client がいれば takeover (server が旧 client を切る)。

harness-cli session ls
    JSON Lines: id, status (Running/Detached/...), is_attached, repo, runner,
    age, ring_buffer_bytes 等。

harness-cli session kill <session-id>
    `harness-cli cancel <id>` の alias。SIGTERM → Cancelled。
```

既存 `harness-cli interactive` は不変。`session` 系統は新 namespace。

### 4.2 TUI

- 既存 Tasks リストに `Detached` ステータスをそのまま表示 (Section 5 の status state machine)。
- attach は既存の interactive キー (`enter` など) で `Detached` task を選んだとき `AttachSession` 経路に分岐。
- `session new` 相当は新キー (例: `S` 大文字)。既存 `i` は legacy interactive (kill-on-disconnect)。

## 5. State machine

```
              session new                  client connect
   (none) ─────────────▶ Running ◀─────────────────────┐
                            │ client disconnect         │
                            ▼                           │
                         Detached ─── session attach ───┘
                            │
                  ┌─────────┴────────┐
                  │ claude exits     │ session kill / cancel
                  ▼                  ▼
            Succeeded/Failed     Cancelled
```

`Detached` は terminal でない中間状態。`Detached ⇄ Running` を attach/disconnect で往復する。terminal 状態 (`Succeeded`/`Failed`/`Cancelled`) になれば既存の `ResumeTaskId` 経路で worktree 流用の "新 task" 起動が引き続き可 (既存挙動変更なし)。

### 5.1 Detailed state transitions

| Event | Running (attached) | Detached |
|---|---|---|
| client tuiStream EOF | → Detached | n/a |
| `attach_session` 受信 | takeover: 旧 close → ok | replay → Running |
| runnerStream EOF (claude exit normal) | → Succeeded | → Succeeded |
| runnerStream EOF (claude exit non-zero) | → Failed | → Failed |
| runner connection lost | → Failed | → Failed |
| `cancel` / `session kill` | claude SIGTERM → Cancelled | claude SIGTERM → Cancelled |
| idle TTL 経過 (timeout > 0) | n/a | → Cancelled |
| server shutdown | runner SIGHUP ladder → Failed | Cancelled マーク後 shutdown |

## 6. Architecture (変更後)

```
                    ┌── ring buffer (per session, raw ANSI)
                    │
client ──tuiStream──▶ server ──runnerStream──▶ runner ──PTY──▶ claude
       (replaceable)         (永続)
                       │
                       └─ session-mux goroutine
                          (= 旧 spliceBidi の発展形)
```

要点:

- `runnerStream` は session lifetime で永続。client 切断とは独立。
- server に `SessionMux` (新規構造) を 1 session に 1 個。
  - `runnerStream.Read` を常時 pump、ring buffer に append、tui がいれば `tuiStream.Write` に forward。
  - `tuiStream.Read` を pump、`runnerStream.Write` に forward (stdin)。
  - 接続 client が無くなったら ring buffer 蓄積のみ。新 client が attach したら ring buffer dump → live forward に合流。
  - 新 attach 時に既存 client がいれば takeover: 旧 `tuiStream` に detach reason 通知後 close。

### 6.1 Detach 検知

```
旧:
  spliceBidi() が EOF/error で全部 close → runner SIGHUP

新:
  tuiStream EOF/error 時:
    if !task.Detachable: 旧挙動 (close runnerStream → runner SIGHUP)
    else:                detach (tuiStream のみ close、runnerStream 維持、status=Detached)
```

`runnerStream` 側 EOF (claude 自然終了 / runner クラッシュ) は従来どおり全閉じ → terminal 遷移。

### 6.2 Reattach

```
client          server                       runner
  │  attach_session(id)                       │
  │ ───TaskControl(AttachSession)──▶          │
  │                                           │
  │       ◀──新 tuiStream allocate──          │
  │       ◀──ring buffer replay (raw)──       │
  │  既存 attach あり? → 旧 tuiStream.Close() │
  │                                           │
  │       ◀──live forward (runnerStream)─────▶│
```

### 6.3 Ring buffer

- 場所: server プロセスのメモリ内、`SessionMux` が所有。
- 形式: 固定サイズ ring (`[]byte` + read/write index、`O(1)` insert、wrap-around で古いの破棄)。
- サイズ: default `1 MiB`、`harness-server --detach-ring-buffer-size=BYTES` で調整可。
- 永続化なし: server 再起動で消える。WAL に乗せない。
- attach 時の replay 順: ring buffer が wrap している場合は古い→新しい順に dump、その後 live forward 開始。

### 6.4 並行性

- session 数の上限は runner の `--max-tasks` で既に効く (interactive は queue できない、capacity gate fail-fast)。detached も 1 capacity slot を消費。
- ring buffer 1MB × 並列 N session ≒ 数 MB オーダー、Pi server でも問題なし。

## 7. Protocol additions (`runner/protocol/message.bgn`)

### 7.1 `TaskStatus` 拡張

```diff
 enum TaskStatus:
     :u8
     Queued
     Running
     Succeeded
     Failed
     Cancelled
+    Detached
```

### 7.2 `OpenInteractiveRequest` / `OpenExecRunnerRequest` 拡張

```diff
 format OpenInteractiveRequest:
     repo_path_len :u16
     repo_path :[repo_path_len]u8
     selector :RunnerSelector
     extra_args :ClaudeArgs
+    detachable :u1   # 1 = session new (detach on disconnect),
+                     # 0 = legacy interactive (kill on disconnect)
+    reserved :u7
     resume_task_id :TaskID
```

```diff
 format OpenExecRunnerRequest:
     task_id :TaskID
     auth_ticket :[16]u8
     repo_path_len :u16
     repo_path :[repo_path_len]u8
     stream_id :u64
     extra_args :ClaudeArgs
+    detachable :u1   # 1 = stream EOF → keep claude alive,
+                     # 0 = stream EOF → SIGHUP ladder (legacy)
+    reserved :u7
```

### 7.3 `TaskControlKind` 拡張

```diff
 enum TaskControlKind:
     :u8
     submit
     list
     cancel
     prune_tasks
     get_task_log
     open_interactive
     client_hello
+    attach_session
```

```diff
 format TaskControlRequest:
     kind :TaskControlKind
     request_id :u32
     match kind:
         TaskControlKind.submit => submit :SubmitRequest
         TaskControlKind.list => list :ListQuery
         TaskControlKind.cancel => cancel :CancelTask
         TaskControlKind.prune_tasks => prune :PruneTasksRequest
         TaskControlKind.get_task_log => get_log :GetTaskLogRequest
         TaskControlKind.open_interactive => open_interactive :OpenInteractiveRequest
         TaskControlKind.client_hello => client_hello :ClientHello
+        TaskControlKind.attach_session => attach :AttachSessionRequest
         .. => error("Unexpected task")

 format TaskControlResponse:
     kind :TaskControlKind
     request_id :u32
     match kind:
         TaskControlKind.submit => submit :SubmitResponse
         TaskControlKind.list => list :ListResult
         TaskControlKind.cancel => cancel :CancelStatus
         TaskControlKind.prune_tasks => prune :PruneTasksResponse
         TaskControlKind.get_task_log => get_log :GetTaskLogResponse
         TaskControlKind.open_interactive => open_interactive :OpenInteractiveResponse
         TaskControlKind.client_hello => client_hello :ClientHelloResponse
+        TaskControlKind.attach_session => attach :AttachSessionResponse
```

### 7.4 `AttachSession*` 新設

```bgn
format AttachSessionRequest:
    task_id :TaskID

enum AttachSessionStatus:
    :u8
    ok                  = "ok"
    not_found           = "not_found"            # task_id 未知 (or pruned)
    not_interactive     = "not_interactive"      # oneshot を attach しようとした
    not_detachable      = "not_detachable"       # detachable=0 で起動された interactive
    already_terminal    = "already_terminal"     # Succeeded/Failed/Cancelled
    runner_unreachable  = "runner_unreachable"   # runner 側が落ちて SessionMux 喪失
    internal_error      = "internal_error"

format AttachSessionResponse:
    status :AttachSessionStatus
    stream_id :u64           # status==ok のとき: server が新規確保した bidi
                             # tuiStream の id。client は
                             # Transport.GetBidirectionalStream(id) で取得し
                             # exec.NewCommandExecutionStream にラップ。
                             # server は ring buffer を replay → live forward へ。
    replay_bytes :u64        # info: replay 予定 bytes 数。0 = buffer 空。
                             # client は stderr に "[replaying NNN bytes]" 等を表示可。
```

Takeover は server 内部処理: 新 attach の `ok` 応答後、旧 `tuiStream` に EOF を送って close。client 側はエラーではなく "応答待ち中に切れた" 形で終了し、CLI が短い detach reason を stderr に出す。

### 7.5 `TaskInfo` 拡張

```diff
 format TaskInfo:
     id :TaskID
     status :TaskStatus
     kind :TaskKind
     origin_kind :ClientKind
     repo_path_len :u16
     repo_path :[repo_path_len]u8
     assigned_to :RunnerID
     worktree_dir_len :u16
     worktree_dir :[worktree_dir_len]u8
     created_at :u64
     started_at :u64
     ended_at :u64
     exit_code :i32
     prompt_len :u32
     prompt :[prompt_len]u8
     error_len :u32
     error_message :[error_len]u8
+    detachable :u1     # immutable, set at OpenInteractive time
+    is_attached :u1    # client tuiStream is currently spliced
+    reserved :u6
+    ring_buffer_bytes :u64   # current bytes buffered (0 for non-detachable)
```

## 8. Component changes per layer

### 8.1 Server-side

**新規: `server/session_mux.go`**

`SessionMux` 構造。1 detachable session に 1 instance。

```
SessionMux 責務:
  - runnerStream を所有 (永続)
  - 接続中 tuiStream を 0 or 1 個保持
  - ring buffer (raw bytes) を保持、書き込みごとに append
  - runner→client 方向の goroutine: runnerStream.Read → ring buffer + tuiStream.Write
  - client→runner 方向の goroutine: tuiStream.Read → runnerStream.Write
  - tui 切断検知 → state=Detached、tuiStream を nil 化
  - 新 attach 受付 → 既存 tui があれば close + 通知、新 tuiStream を install、ring buffer dump → live forward
```

**新規: `server/session_registry.go`**

- `taskID -> *SessionMux` map (mutex 付き)。
- `SessionMux` 終了 (claude exit / cancel) でエントリ削除。
- `TaskStore` の cancel/prune と同期 (cancel 時 `SessionMux.Stop()`)。

**変更: `server/task_handler.go`**

- `handleOpenInteractive`: `req.Detachable == 1` のときは `SessionMux` を生成、`spliceBidi` の代わりに `SessionMux.Run()` 起動。`OpenExecRunnerRequest.Detachable` を runner にも伝播。`req.Detachable == 0` パスは現行 `spliceBidi` のまま (legacy 完全保存)。
- 新規 `handleAttachSession(req *AttachSessionRequest)`: `Tasks.Get(taskID)` → `Detachable / Status` 検証 → `SessionRegistry` で `*SessionMux` 索引 → `Attach(tuiStream)` → `AttachSessionResponse` 返却。各種エラーは `AttachSessionStatus` に対応マップ。
- `dispatch.go` (`handleTaskControl`) に `attach_session` 分岐を追加。

**変更: `server/taskstore.go`**

- `TaskStatus_Detached` の遷移ルール:
  - `Running → Detached` (client 切断 + detachable=1)
  - `Detached → Running` (新 attach 成立)
  - `Detached → Succeeded/Failed` (claude 自然終了; runner からの TaskFinished で遷移)
  - `Detached → Cancelled` (cancel)
- WAL に `task_detached` を載せない (server 再起動で SessionMux が消える以上、Detached を復元できない)。server 起動時に走っていた Detached task は `Cancelled` としてマーク。

**変更: `cmd/harness-server/main.go`**

- 新 flag:
  - `--detach-ring-buffer-size=BYTES` (default `1048576` = 1MiB)
  - `--detach-idle-timeout=DUR` (default `0` = 無制限)

### 8.2 Runner-side

**変更: `runner/session.go:handleOpenExec`**

- `oer.Detachable == 1` を `taskEntry` および log フィールドに保存 (診断用)。
- functional な挙動変更は無い。`agentexec.ExecuteCommandWithOption` の呼び出しはそのまま。

**`exec/exec.go` 変更不要**

理由: SIGHUP ladder は `io.Copy(p, pipeOut)` の EOF (`exec/exec.go:205`) で発火する。`pipeOut` は bidi stream の Read 側から来る pipe で、server が runnerStream を閉じなければ runner 側で EOF が起きない。detachable session では server-side `SessionMux` が runnerStream を保持し続けるため、runner はあくまで「stream が生きていて単に stdin が来ない」状態として正常動作する。claude プロセスは stdin 入力なし状態で blocking を続けるが、これは通常の挙動。

claude の stdout 側は server が常に runnerStream を read しているため、書き込み詰まりは起きない (出力は ring buffer に吸収)。

### 8.3 Client-side

**新規: `cli/agent/session.go`** (subcommand 群)

```
harness-cli session new      → OpenInteractive(Detachable=1) → splice
harness-cli session attach   → AttachSession → splice
harness-cli session ls       → ListResult を filter (TaskKind=interactive、Detachable=1)
harness-cli session kill     → CancelTask の alias
```

**新規: `cli/attach.go`**

- `AttachSessionRequest` の RoundTrip wrapper。
- `peer.WaitForBidirectionalStream` で取得した stream を `agentexec.NewCommandExecutionStream` でラップ → `RemoteShell()` 起動。
- `replay_bytes > 0` のとき stderr に `[replaying NNN bytes]` を出してから raw 状態へ。

**変更: `cli/open_interactive_native.go`**

- `OpenInteractiveWithSelectorAndArgs` に `detachable bool` 引数を追加。既存呼び出し = `false`、`session new` 経由 = `true`。

**変更: `cmd/harness-cli/main.go`**

- `session` サブコマンド系のディスパッチ (`new` / `attach` / `ls` / `kill`)。

**変更: `tui/`**

- 既存 Tasks リストに `Detached` ステータス表示を追加。
- attach キー: 既存 interactive キーの分岐に `Detached` task のとき `AttachSession` を叩くロジックを追加。
- `session new` 相当キー (大文字 `S` を予定)。既存 `i` は legacy interactive のまま。

### 8.4 影響ファイル一覧

| ファイル | 変更タイプ | 概要 |
|---|---|---|
| `runner/protocol/message.bgn` | 拡張 | §7 全項 |
| `runner/protocol/message.go` | 自動再生成 | `protoregen.sh` |
| `server/task_handler.go` | 変更 | OpenInteractive 分岐、AttachSession ハンドラ |
| `server/session_mux.go` | 新規 | SessionMux + ring buffer |
| `server/session_registry.go` | 新規 | taskID → *SessionMux |
| `server/taskstore.go` | 変更 | Detached 遷移 |
| `server/dispatch.go` | 変更 | attach_session ディスパッチ |
| `cmd/harness-server/main.go` | 変更 | 新 flag |
| `runner/session.go` | 変更 (軽微) | handleOpenExec で Detachable を taskEntry / log に保存 (診断用) |
| `cli/open_interactive_native.go` | 変更 | detachable param |
| `cli/attach.go` | 新規 | AttachSession round-trip |
| `cli/agent/session.go` | 新規 | session subcommand 群 |
| `cmd/harness-cli/main.go` | 変更 | session ディスパッチ |
| `tui/` 各ファイル | 変更 | Detached 表示 + attach キー |

## 9. Edge cases

- **Ring buffer 溢れ**: 仕様通り古い bytes から捨てる (wrap)。client は attach 時に最新分しか見えない。警告は出さない。`replay_bytes` が ring サイズと一致した場合 client は "buffer was full" を推測可。
- **Detached 中 runner 再起動**: runner ↔ server stream 落ち → runnerStream EOF。SessionMux は task を `Failed` 遷移。後続 `attach` は `runner_unreachable`。
- **Detached 中 server 再起動**: WAL に Detached を載せない方針なので、起動時に走っていた Detached は `Cancelled` マーク。runner は SIGHUP で claude を畳み、再接続時に TaskFinished を送る。
- **Attach race (2 client 同時 attach)**: server は `SessionRegistry` 内 `attachMu` で逐次化。先着 = `Ok`、後着 = takeover (前者を蹴る)。短時間連続 race の場合は `internal_error` で client にリトライ余地。
- **Cancel と Detach race**: cancel が先なら `Cancelled` 確定、後続 `attach` は `already_terminal`。逆順も SessionMux.Stop で同経路。
- **Legacy interactive (Detachable=0) の client 切断**: 既存挙動 (claude SIGHUP ladder) のまま。本 spec で touch しない。

## 10. Error handling matrix (新規経路)

| 発生箇所 | 条件 | 返却 status | client UX |
|---|---|---|---|
| `OpenInteractive` (detachable=1) | runner busy / no runner | 既存 `runner_busy` / `no_runner_for_repo` | 既存どおり |
| `AttachSession` | task_id 不明 | `not_found` | `attach failed: not_found <id>` |
| `AttachSession` | TaskKind=oneshot | `not_interactive` | `attach failed: not_interactive` |
| `AttachSession` | Detachable=0 | `not_detachable` | `attach failed: not_detachable` (legacy interactive へのヒント) |
| `AttachSession` | Status terminal | `already_terminal` | `attach failed: already_terminal` (resume へのヒント) |
| `AttachSession` | SessionMux 喪失 | `runner_unreachable` | `attach failed: runner_unreachable` |
| `SessionMux` runner write 失敗 | stdin が伝わらない | client stderr `[detached: write failed]` 後切断 | reattach 可能 |

## 11. Testing strategy

### 11.1 Unit

- `server/session_mux_test.go`: ring buffer wrap-around、attach⇄detach 遷移、takeover、cancel、runner EOF を fake stream で検証。
- `server/taskstore_test.go`: Detached の遷移ルール (Running⇄Detached、Detached→{Succeeded,Failed,Cancelled})。`oncancel_test.go` の cancel 経路に Detached パスを追加。
- `runner/protocol/message_test.go`: 新 schema フィールド込みの round-trip。
- runner 側で SIGHUP ladder が発火しないことの保証は **server-side テストで cover** (`SessionMux` が runnerStream を閉じないこと)。runner 側コード変更が無いので exec/ の追加テストは不要。

### 11.2 Integration (`integration/session_detach_test.go`, build tag `integration`)

1. server + runner + fake-claude を立てる。
2. `session new` で fake-claude 起動、`marker A\n` を出力。
3. client を切断、server 上で task が `Detached` になるのを確認。
4. fake-claude が `marker B\n` を出力 (ring buffer に入る)。
5. `session attach` で再接続、`replay_bytes >= len("marker A\nmarker B\n")` を検証、stdout に両方が現れる。
6. もう一度 detach → 別 client で reattach、takeover が効くことを確認。

### 11.3 Ring buffer wrap テスト

新規 `testdata/fake-claude-loud.sh` を追加 (大量出力を吐く fake claude):

```sh
#!/usr/bin/env bash
# Emit >>1MiB to overflow the default ring buffer.
yes "loud line: $(date -u +%s.%N)" | head -c 5000000
```

使い方: integration テストで `--claude-bin testdata/fake-claude-loud.sh` を渡し、`session new` → 切断 → ring が wrap した状態を作る → `session attach` で `replay_bytes == ring_size` (= 1MiB) を検証。

### 11.4 Manual smoke

実構成 (Pi server / gmkhost runner / Windows client) で 1 セッションを通す:

1. Windows から `session new`、claude と数往復。
2. Windows のターミナルを閉じる (= ネット切断)。
3. 別端末 (例: Pi に SSH した別シェル) から `session attach <id>` で復帰。
4. やり取りが続けられること、ring buffer の内容が見えていることを確認。

これが本 spec の駆動目的そのもの。
