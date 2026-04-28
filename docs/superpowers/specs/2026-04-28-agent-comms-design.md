# Agent-to-Agent Communication via Harness — Design (v1)

Status: draft, pending user review
Date: 2026-04-28

## 1. Goal

Harness 配下で動く複数の claude (agent) が、harness 経由で相互に message を交換できる仕組みを v1 として導入する。あわせて、agent (claude) が自分の所属する harness server / runner / task を自己認識できる env contract を整備する。

これによって以下 4 種の use case を一つの primitive で扱えるようにする:

| | Use case | パターン |
|--|---|---|
| **A** | Coordinator / workers | 1 つの coordinator agent が複数の worker agent に作業を振り、結果を集約する |
| **B** | Peer chat | 2 体以上の agent が同 conversation で turn 制対話する |
| **C** | Blackboard | 共有 topic に facts を書き、後着 agent が現状を読む |
| **D** | Status / presence | 各 agent が自身の `status/<id>` topic に最新状態を retain する |

## 2. Non-goals (v1)

- MCP server による rich tool surface (v2: CLI を MCP で wrap)
- per-publish の `--no-retain` flag (一律 retain)
- per-topic の retention policy CLI (`harness-cli agentboard set-policy`、v2)
- Cross-runner-restart の persistent agent identity (agent ≈ task の ephemeral 性を仕様として受容)
- 同 host 上のプロセス間での identity 偽装防御 (memory: dogfood scope, no external users)
- 暗号学的認証 (PSK / HMAC sign)。auth ticket は network identity 偽装防御に限定
- non-claude agent (Codex / Cursor 等) への対応

## 3. Existing assets

| Layer | 責務 | 本設計での扱い |
|---|---|---|
| `objproto` / `trsf` / `peer` | secure session + multi-stream + Dial/Hello | 既存利用 (新 wire payload kind を 1 つ足す) |
| `pubsub` | topic JOIN/LEAVE + ephemeral fan-out (no retain) | **不変条件保持**。agentboard とは別 component |
| `runner.Config` (`AllowedRoots`, `MaxTasks`, `Hostname`) | multi-task / multi-repo runner 構成 | identity の構成要素として参照 |
| `protocol.RunnerID` (`transport+ip+port+unique16`) | runner connection 識別子 | identity の片側 |
| `protocol.TaskID` (16 byte) | task 識別子 | identity のもう片側 |
| `protocol.AssignTask` / `OpenExecRunnerRequest` | server → runner の task 投入 | `auth_ticket :[16]u8` を新規追加 |
| `WorktreeManager.Create` | task ごとの worktree | 直後に `.claude/settings.json` を生成 (hook 注入) |

## 4. Architecture

```
host (runner マシン)
└── agent-runner プロセス
    └── claude プロセス (= 1 task = 1 agent)
        ├── env: HARNESS_*
        ├── <worktree>/.claude/settings.json (hooks.UserPromptSubmit)
        └── shell (Bash tool):
              harness-cli agent send|wait|inbox|subscribe|dispatch
                ↓ wire.ApplicationPayloadKind = AgentMessage  [新規]
                ↓ peer.Dial → 新規 WebSocket connection per agent CLI invocation
                ↓
agent-harness server
├── 既存 pubsub (ephemeral, retain なし) [変更なし]
└── agentboard (新 component)
    ├── in-memory ring buffer per topic (N=64, TTL=30m default)
    ├── auth ticket registry: (runner_id, task_id) → ticket
    └── topic dispatch + wait queue
```

### 配置の意図

- claude は host 上の subprocess、harness server は別 host (memory: cross-OS / cross-arch is design intent)。CLI は dogfood scope で十分軽量、MCP の rich tool surface は v2 で wrap で足せるので preserve しやすい。
- agentboard は **server に内蔵**、runner には置かない。理由: runner 跨ぎの message routing が必要 (例: runner X の task が runner Y の task に話しかける)、server が唯一の routing point。
- 既存 pubsub layer に retention を被せる案も検討したが、`task.<id>.log` 等の既存 ephemeral 前提を破壊するため不採用 (Q4)。agentboard は完全に独立した layer。

## 5. Identity & Lifecycle

### 5.1 Agent identity

```
identity = (runner_id: protocol.RunnerID, task_id: protocol.TaskID)
display  = hostname (string, optional, equality 判定には用いない)
```

- `runner_id` は ephemeral (再接続で変わる) だが、agent ≈ task の寿命と一致するため identity 切れは発生しない。runner disconnect 時 task は Failed 化 (既存 `OnRemove`) → 同じ identity で再接続することはない。
- `hostname` は表示専用 metadata。frame の `from_hostname` field に optional 添付。equality / lookup には使わない。

### 5.2 Lifecycle

```
1. server が task を runner に dispatch
   → AssignTask (or OpenExecRunnerRequest) に auth_ticket を含める
2. runner は worktree 作成後、env と .claude/settings.json を整える
   - env: HARNESS_SERVER_CID / RUNNER_ID / TASK_ID / REPO_PATH / HOSTNAME / WS_PATH / AUTH_TICKET
   - settings.json: hooks.UserPromptSubmit に harness-cli agent inbox --since-last
3. runner が claude を spawn
4. claude が必要に応じて Bash tool で harness-cli agent ... を実行
   - CLI は env 読み取り → peer.Dial → AgentBridgeHello を送信
   - server agentboard は (runner_id, task_id, ticket) を registry と照合
   - 不一致なら connection 拒否 (Hello response に reject status)
5. claude / task 終了
   → server agentboard は task 終了通知 (既存 TaskFinished) を契機に
     当該 (runner_id, task_id) に紐づく Hello 済 connection を強制 close
     auth ticket を registry から削除
6. agent が retain した topic はそのまま残る (ring + TTL で自然消滅)
```

## 6. Wire protocol

### 6.1 New `wire.ApplicationPayloadKind`

```
existing: TaskControl, RunnerControl, Pubsub, RelayControl
new:      AgentMessage          // 新値、agentboard 専用
```

### 6.2 New format (`agentboard.bgn` 提案、ebm2go で codec 自動生成)

```bgn
config.go.package = "agentboard"

enum AgentMessageKind:
    :u8
    Hello
    HelloResponse
    Send
    Subscribe
    Unsubscribe
    Wait
    WaitResponse
    Inbox
    InboxResponse
    Deliver         # server → agent への push (subscribed topic への新着)

format AgentBridgeHello:
    runner_id   :RunnerID            # 既存 protocol から再利用
    task_id     :TaskID              # 既存 protocol から再利用
    auth_ticket :[16]u8
    hostname_len :u8
    hostname     :[hostname_len]u8

enum HelloStatus:
    :u8
    ok
    bad_ticket
    unknown_task
    runner_mismatch

format AgentBridgeHelloResponse:
    status :HelloStatus

format SendRequest:
    request_id :u32
    topic_len  :u16
    topic      :[topic_len]u8
    payload_len :u32
    payload    :[payload_len]u8     # JSON-encoded message frame (§7.2)

format SubscribeRequest:
    request_id :u32
    pattern_len :u16
    pattern    :[pattern_len]u8     # glob pattern, e.g. "task/<id>/result/*"

format WaitRequest:
    request_id :u32
    pattern_len :u16
    pattern    :[pattern_len]u8
    since      :u64                 # cursor (server-assigned monotonic seq)
    timeout_ms :u32

format InboxRequest:
    request_id :u32
    since      :u64

format DeliveredMessage:
    seq        :u64                 # monotonic per server-process
    topic_len  :u16
    topic      :[topic_len]u8
    payload_len :u32
    payload    :[payload_len]u8     # JSON frame (§7.2)

format WaitResponse:
    request_id :u32
    timed_out  :u8                  # 0 or 1
    msgs_len   :u16
    msgs       :[msgs_len]DeliveredMessage

format InboxResponse:
    request_id :u32
    msgs_len   :u16
    msgs       :[msgs_len]DeliveredMessage
    next_cursor :u64                # client が次回 --since に渡す
```

`Deliver` は server → agent の push (live subscribe された topic の新着)。CLI 側は基本 request-response で済ませるため、active push 経路は v1 では使わなくても良い (Wait の long-poll で代用可)。spec には残すが実装は v1 必須ではない。

### 6.3 Auth ticket flow

`AssignTask` / `OpenExecRunnerRequest` に `auth_ticket :[16]u8` を追加:

```bgn
format AssignTask:
    task_id     :TaskID
    repo_path_len :u16
    repo_path   :[repo_path_len]u8
    auth_ticket :[16]u8              # NEW
    prompt_len  :u32
    prompt      :[prompt_len]u8
```

server 側:
- task 発行時に `auth_ticket = crypto/rand.Read(16)` を生成
- `agentboard.RegisterTicket(runner_id, task_id, ticket)` を呼ぶ
- AssignTask payload で runner に渡す
- TaskFinished を受信したら `agentboard.RevokeTicket(runner_id, task_id)`

runner 側:
- AssignTask を受信したら `os.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(ticket))` 相当を claude spawn 時の env に injection (process-global setenv は不可、`exec.Command{Env: ...}` で注入)

agent CLI 側:
- `os.Getenv("HARNESS_AUTH_TICKET")` を hex decode
- `AgentBridgeHello.auth_ticket` に格納し送信

server agentboard 側:
- Hello 受信 → `(runner_id, task_id)` で registry lookup → ticket 照合
- 不一致 → `HelloResponse{status=bad_ticket}` を返して `pc.Close()`

## 7. Message frame & topic conventions

### 7.1 Topic naming convention

agentboard 内で意味分けするための慣習。CLI は任意の string topic を受け付けるが、ドキュメントでこれらを推奨:

| Pattern | 用途 | Producer | Consumer |
|---|---|---|---|
| `task/<id>/dispatch` | A coordinator → workers の指示 | coordinator | workers (subscribe) |
| `task/<id>/result/<worker_task_id>` | A workers → coordinator の reply | each worker | coordinator (wait) |
| `conv/<id>/messages` | B peer chat | participants | participants |
| `topic/<arbitrary>` | C blackboard 共有 | anyone | anyone |
| `status/<runner_id>/<task_id>` | D status presence | self | observers |

(`<id>` は 16 byte hex string、`<arbitrary>` は任意 string)

既存 pubsub 名空間 (`task.<id>.log` 等の "." 区切り) と衝突しないよう、agentboard topic は **"/" 区切り** に揃える。

### 7.2 Message frame

agentboard の `payload` には UTF-8 JSON を入れる:

```json
{
  "from": {
    "runner_id": "ws:1.2.3.4:9000-12345",
    "task_id": "abcdef0123456789..."
  },
  "from_hostname": "dev-pi-01",
  "conversation_id": "conv-abc123",
  "in_reply_to": "msg-xyz" | null,
  "msg_id": "msg-uniq",
  "ts_unix_ms": 1735574400000,
  "payload": "<application body, free string or nested JSON>"
}
```

- `msg_id`: client (agent CLI) が生成する uuid。dedup / `in_reply_to` 用
- `from`: agent CLI が env から導出、改竄不可 (server は Hello で確認済の runner_id/task_id と一致するか validate して reject 可)
- `payload`: 中身の自由形式。複雑な構造は agent 同士の application-level 合意に委ねる

server 側 validation: `Send` 受信時に `frame.from.runner_id == hello.runner_id && frame.from.task_id == hello.task_id` を確認。不一致なら error response。

## 8. Retention & limits

server 起動 flag (新規):

| flag | default | 意味 |
|---|---|---|
| `--agentboard-ring` | 64 | topic ごとの ring buffer 件数 |
| `--agentboard-ttl` | 30m | topic 最終 publish からの破棄猶予 |
| `--agentboard-max-topics` | 1024 | 同時保持 topic 数の上限 (LRU evict) |
| `--agentboard-max-payload` | 64K | 1 message の payload 最大 byte 数 |

evict 規則:
- ring overflow → 古い順に drop
- topic 全 subscriber 0 + 最終 publish から TTL 経過 → topic ごと破棄
- topic 総数が `max-topics` を超える → 最終 publish 古い順で破棄

`Wait` / `Inbox` の挙動:
- subscribe / wait 開始時、ring 内既存 message を `since` cursor 以降について flush して返す
- その後 timeout まで block (wait の場合) / 即 return (inbox の場合)

per-topic policy 上書き CLI (`harness-cli agentboard set-policy --topic 'conv/*' --ring 256 --ttl 2h`) は v2。

## 9. CLI surface

### 9.1 New: `harness-cli agent` subcommand 群

すべて env primary、flag override (§9.3 参照、ticket は env-only)。

| subcommand | 用途 | block / non-block | use case |
|---|---|---|---|
| `harness-cli agent send --topic T --data ...` | message を post | non-block | A,B,C,D |
| `harness-cli agent wait --topic T [--since CURSOR] [--timeout DUR]` | 次の message を block して取る | block (timeout) | A coordinator reply 待ち |
| `harness-cli agent inbox [--since CURSOR]` | 全 subscribe topic の pending を一括 dump | non-block | β hook (UserPromptSubmit) |
| `harness-cli agent subscribe --pattern PAT` | この task が興味ある topic pattern を server に登録 | non-block | β / wait の事前準備 |
| `harness-cli agent unsubscribe --pattern PAT` | 解除 | non-block | |
| `harness-cli agent dispatch --topic T --data ... [--reply-pattern PAT] --wait --timeout DUR` | send + 同 conversation の reply を block で取る糖衣 | block (timeout) | A coordinator |

`--data` は `-` で stdin 受け付け。output は JSON Lines (1 行 1 message) で stdout、CLI が消費しやすい形。

### 9.2 Cursor semantics

- agentboard は server-process-global に monotonic `seq :u64` を message に振る
- `inbox` / `wait` の response に `next_cursor` を含める
- agent CLI は `~/.cache/harness/agent-cursor-<task_id>` に next_cursor を永続化、`--since-last` は自動でこれを読む
- `--since 0` で全 ring 内容を取得

### 9.3 env / flag 結合則

| env | 同名 flag | flag 受付 |
|---|---|---|
| `HARNESS_SERVER_CID` | `--server-cid` | 可 |
| `HARNESS_TASK_ID` | `--task-id` | 可 (debug) |
| `HARNESS_RUNNER_ID` | `--runner-id` | 可 (debug) |
| `HARNESS_HOSTNAME` | `--hostname` | 可 |
| `HARNESS_REPO_PATH` | `--repo-path` | 可 |
| `HARNESS_WS_PATH` | `--ws-path` | 可 |
| `HARNESS_AUTH_TICKET` | (なし) | **不可** (env-only、process list 衛生) |

優先順位: flag > env > 既定 (なければ起動エラー)

`HARNESS_AUTH_TICKET` のみ env-only。debug でも `HARNESS_AUTH_TICKET=xxx harness-cli agent ...` を強制。

### 9.4 既存 `harness-cli` への env fallback 拡張

`submit`, `interactive`, `list`, `watch`, `cancel`, `prune`, `logs` も `--server-cid` 等の flag に **env fallback** を入れる:

- `~/.bashrc` 等で `export HARNESS_SERVER_CID=...` しておけば、人間 user が `harness submit ...` 打つ時に flag 省略可
- 内部実装は §10.1 の共通 helper を全 subcommand で使う
- 既存テストは flag 渡しのまま壊れない (flag 優先)

## 10. Implementation outline

### 10.1 共通 helper (新規 package `cli/cliopts` 等)

```go
// resolveServerCID returns CID from flag, env, or error.
func resolveServerCID(flagVal string) (objproto.ConnectionID, error) { ... }

// resolveAuthTicket returns ticket from env only.
func resolveAuthTicket() ([16]byte, error) { ... }

// resolveAgentIdentity returns (runner_id, task_id, hostname) for agent CLI.
func resolveAgentIdentity(flags *AgentFlags) (RunnerID, TaskID, string, error) { ... }
```

`harness-cli` 全 subcommand と新規 `harness-cli agent ...` の双方が利用。

### 10.2 server agentboard component

新パッケージ `agentboard/`:

```go
type Board struct { ... }

func New(cfg Config) *Board

// Hello path
func (b *Board) RegisterTicket(runnerID RunnerID, taskID TaskID, ticket [16]byte)
func (b *Board) RevokeTicket(runnerID RunnerID, taskID TaskID)
func (b *Board) ValidateHello(h *AgentBridgeHello) HelloStatus

// Send / Subscribe / Wait / Inbox
func (b *Board) Send(from Identity, topic string, payload []byte) (seq uint64, err error)
func (b *Board) Subscribe(client *Client, pattern string) error
func (b *Board) Wait(client *Client, pattern string, since uint64, timeout time.Duration) ([]DeliveredMessage, error)
func (b *Board) Inbox(client *Client, since uint64) ([]DeliveredMessage, uint64)

// Topic state
type Topic struct {
    name string
    ring *ringbuf.Ring  // size = cfg.RingN
    lastSeq uint64
    lastPublishedAt time.Time
}
```

server 側 `wire` dispatch (`server.go` の OnControl) に `case wire.ApplicationPayloadKind_AgentMessage` を追加し、agentboard.Decode → `Board` の対応 method 呼び出し。

### 10.3 runner side ticket plumbing

- `runner.Config` に `auth_ticket` を持つ必要なし (per-task で動的)
- `Session.handleAssign` / `handleOpenExec` の冒頭で `req.AuthTicket` を取得
- worktree 作成後 (§10.4) に env として記録、`Process.Run` の `exec.Cmd.Env` に注入
- `Process` は `ClaudeBin`, `ExtraArgs` に加え `Env []string` を受けるよう拡張

### 10.4 settings.json injection

`WorktreeManager.Create` の直後、新メソッド `WorktreeManager.WriteAgentSettings(taskID)` を呼ぶ:

```go
// <worktree>/.claude/settings.json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "harness-cli agent inbox --since-last --json"
          }
        ]
      }
    ]
  }
}
```

claude が起動時にこの settings を pickup し、毎 user prompt 直前に hook が走る。`harness-cli agent inbox` が JSON Lines を stdout に書き、claude の context に inject される。

(`--since-last` は agent CLI が cursor file を読む。最初の turn は cursor=0 で ring 全件 dump → これが "task 開始時に subscribed topic の現状を見る" 機能を兼ねる。)

### 10.5 New `cmd/harness-cli` subcommand

`cmd/harness-cli/main.go` に `agent` subcommand を追加:
- `agent send` / `agent wait` / `agent inbox` / `agent subscribe` / `agent unsubscribe` / `agent dispatch`
- 内部実装は `cli/agent/` package (新規) に分離、各 subcommand は thin wrapper

接続フロー:
1. `cliopts.resolveAgentIdentity` で env 取得
2. `peer.Dial` で server に接続 (新規 ConnectionID)
3. `AgentBridgeHello` 送信、`HelloResponse{status=ok}` を待つ
4. 各 subcommand 固有の Send/Wait/Inbox/Subscribe を投げる
5. response を JSON Lines で stdout
6. process exit (各 invocation は短命、persistent connection は持たない)

## 11. Testing

### 11.1 Unit

- `agentboard/`:
  - `Send` で ring に積まれること
  - `Wait` が timeout までに message が来れば return
  - `Wait` が timeout で empty return
  - `Inbox` が `since` 以降の全件 + cursor を返す
  - TTL 経過で topic が evict されること
  - ring 容量超過で oldest drop
  - `RevokeTicket` 後の Hello は `bad_ticket`
- `cli/agent/`:
  - env / flag resolution の優先順位
  - `--since-last` の cursor file read/write
  - JSON output 形式

### 11.2 Integration

- `agent send` → `agent wait` (同 server で 2 process round-trip)
- A coordinator/workers シナリオ: 1 つの coordinator task が 2 worker task を spawn (server 経由 submit) し、`task/X/result/*` を wait
- B peer chat: 2 task が `conv/Y/messages` で交互 send
- C blackboard: 後着 task が `topic/foo` の retain 内容を inbox で読める
- D status: task A が `status/.../...` を send、task B が wait で見える
- bad ticket: 偽の ticket を持った Hello が reject される
- task finish 後の Hello: revoke 済 ticket は reject される

### 11.3 dogfood scenario

実 claude を 2 体起動し、片方が他方に質問を投げて返答が返る最小デモを手動で確認。

## 12. Migration / compatibility

- 既存 `pubsub` layer は変更なし → 既存 TUI/WebUI/CLI watch は影響なし
- `AssignTask` / `OpenExecRunnerRequest` への `auth_ticket` 追加は wire 拡張だが、現 user 全員 dogfood (memory: "Individual dogfood, no external users") なので server / runner の同時更新が現実的。後方互換コードは入れない
- 既存 `harness-cli` の `--server-cid` は flag のままで動き続ける。env fallback は加点で、既存運用に強制ではない

## 13. Risks & open questions

- **claude hook 未対応版**: 古い claude binary だと UserPromptSubmit hook を読まない可能性。dogfood では最新版前提で良いが、settings.json の存在自体が claude の警告を出さないか要確認 (実装前に手動確認)
- **agent CLI invocation overhead**: 1 hook ごとに `harness-cli agent inbox` が peer.Dial する → WS 接続 cost が turn ごとに発生。dogfood scope では許容範囲だが、頻度高ければ §6.2 の `Deliver` push を使った long-lived connection にする (v2)
- **JSON frame の内容自由度**: free string `payload` を agent 同士の合意で structuring することにしたが、scheme を強く推奨する標準 sub-format が必要かは use 状況を見て判断
- **conversation_id の生成**: A coordinator は task_id を流用すれば足りるが、B/C は agent application-level での合意が要る。CLI が auto-gen する API があると便利だが v1 では agent 自由

## 14. Out of scope (再掲)

§2 のとおり、MCP, per-publish retain flag, persistent agent identity, cross-host 偽装防御以上の crypto はすべて v2 以降。
