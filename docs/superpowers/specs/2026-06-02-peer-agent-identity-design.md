# Peer agent identity over the protocol — design

- 日付: 2026-06-02
- 対象: `runner/protocol/message.bgn`（+ 生成 `message.go`）, `runner/connect.go`, `server/runner_handler.go`, `server/task_handler.go`（RunnerEntry + toRunnerInfo）, `cli/list.go`, `cmd/harness-webui-wasm/main.go`, `runner/agentskills/harness-cli/SKILL.md`; （§9 関連機能）`runner/agentskills/embed.go`（新規）, `runner/agentskill.go`, `cmd/harness-cli/main.go`
- 種別: プロトコル拡張（schema 変更あり、要 `make protoregen`）

## 1. 問題

`harness-cli ls` / snapshot からは peer（タスク）が何で動いているか分からない。runner の `--claude-bin` は `claude` だけでなく `gemini` / `codex` / `bash` 等にもなり得るし、`--no-worktree`（`--force-inject-harness-settings` 無し）だと `.claude/{settings.json,skills}` が注入されず、その peer はこの skill も auto-inbox フックも持たない。現状そのどれも wire に出ないため、エージェントは peer の正体を**振る舞い（handshake に応じるか）でしか**推測できない（`SKILL.md` の "Peers may not be claude" caveat 参照）。これを**明示的に判別可能**にする。

## 2. ゴール / 非ゴール

### ゴール
- runner が走らせている agent バイナリ名と、harness 注入の有無を wire に載せ、`ls`/snapshot から peer 単位で判別できるようにする。
- `claude` 決め打ちにしない（gemini/codex/bash 等を等しく扱える）。

### 非ゴール
- agent バイナリの自動分類（"これは agent か shell か"）を wire でやること。**生の basename を載せ、分類・表示はクライアント/人間側**に委ねる。
- `--claude-bin` フラグ自体のリネーム（agent-agnostic 化）は別件。本件は wire/表示のみ。
- クロスバージョン互換（個人ドッグフード方針 — server/runner は一緒に再ビルドする）。
- claude 以外（gemini/codex）向けの設定・フック注入の実装。本件は「何が動いているか」を可視化するだけ。

## 3. データモデル（schema は1箇所に集約）

`runner/protocol/message.bgn`。新 enum は作らない。**`RunnerHello`（runner→server）と `RunnerInfo`（server→client）の両方の末尾**に同じ2項目を追加する:

```
# basename of the agent binary the runner runs (--claude-bin), e.g.
# "claude" / "gemini" / "codex" / "bash". Empty = unknown.
    agent_bin_len :u8
    agent_bin :[agent_bin_len]u8
# whether the runner injects .claude/{settings.json,skills} for its tasks
# (= the inbox hook AND the harness-cli skill are present). False means the
# peer follows none of the skill conventions even if it is an agent CLI.
    skills_injected :u1
    reserved :u7
```

- `RunnerHello` は現状 `... allowed_roots :[allowed_roots_len]AllowedRoot` で終わる → その後に上記4行を追加。
- `RunnerInfo` は現状 `... last_seen :u64` で終わる → その後に同じ4行を追加。
- 既存の enum（`ClientKind`/`TaskKind`）パターンは踏襲しない（今回は文字列で十分、決め打ち回避）。`agent_bin` 空文字 = 不明（安全既定）。

## 4. runner 側の導出（`runner/connect.go` の hello 構築、~208-232）

```go
hh.SetAgentBin([]byte(filepath.Base(cfg.ClaudeBin)))   // "claude" / "gemini" / "bash" ...
hh.SetSkillsInjected(!cfg.NoWorktree || cfg.ForceInjectHarnessSettings)
```

- `cfg.ClaudeBin` / `cfg.NoWorktree` / `cfg.ForceInjectHarnessSettings` は hello 時点で参照可能（`runner/connect.go:23-52`）。
- `filepath.Base` で basename 化（フルパス指定でも `claude` 等になる）。空なら空のまま（クライアントが「unknown」表示）。
- 注入判定 `!NoWorktree || ForceInjectHarnessSettings` は実際の注入ガードと**完全一致**（確認済み: `runner/session.go:398` handleAssign / `:589` handleOpenExec の `if !s.NoWorktree || s.ForceInjectHarnessSettings`）。同じ式を1箇所のヘルパに切り出して hello 構築と将来の参照で共有してもよい。

## 5. サーバ

- **`server/runner_handler.go`（~66）**: `hello := msg.Hello()` から `hello.AgentBin()` / `hello.SkillsInjected()` を読み、`RunnerEntry` に保存（`RunnerEntry` に `AgentBin string` / `SkillsInjected bool` を追加）。
- **`server/task_handler.go:913 toRunnerInfo(r RunnerEntry)`**: `info.SetAgentBin(...)` / `info.SetSkillsInjected(...)` を echo。
- これで `ListResultBody.Runners[i]` に常に含まれる。タスク側（`TaskInfo`）は変更せず、`assigned_to :RunnerID` で join する。

## 6. クライアント表示

### `cli/list.go`（`ls` テキスト）
- **RUNNERS 行**: 末尾に `agent=<bin> skills=<yes|no>` を追加。`agent_bin` 空なら `agent=?`。
- **TASKS 行**: 各タスクの `AssignedTo` を RUNNERS から引いて、行に `agent=<bin>[+skills]` を追加（例 `agent=claude+skills` / `agent=gemini` / `agent=bash`）。`+skills` は注入時のみ付与、未注入は付けない。join 先 runner が見つからない（タスク未割当等）の場合は省略。
- これによりエージェントが `ls` テキストだけで peer 種別を判別できる。

### WebUI（`cmd/harness-webui-wasm/main.go` の snapshot）
- runner JSON（~330-338）に `"agentBin": string(r.AgentBin)` と `"skillsInjected": bool(r.SkillsInjected)` を追加。
- 最低限 runner 一覧表示に反映。タスク行への join 表示は WebUI 側で任意（snapshot に出ていれば JS で join 可能）。

### SKILL.md
- 直近で追記した「Peers may not be claude — or skill-injected」節の「**there is currently no way to tell from `harness-cli`**」を、「`ls` の各タスク行 `agent=<bin>[+skills]` で判別できる（ただし注入無し/非agentバイナリは依然 convention 非追従）」に更新。判別不可前提のくだりを実態に合わせる。

## 7. 互換性（個人ドッグフード）

- フィールドは両メッセージの**末尾追加**。`agent_bin` 空 / `skills_injected=false` が安全既定。
- 末尾追加でも固定長デコードのため、**未再ビルドの旧 runner ↔ 新 server は hello デコードで失敗**する。クロスバージョンは**サポートしない**: `server` と `runner` を一緒に再ビルドする（`scripts/build_and_restart_all.py` で全再起動）。これは外部ユーザ無しの方針上、移行レイヤを足さず品質修正として扱う。
- `RunnerHello.version` は触らない（条件分岐デコードはしない）。

## 8. テスト

- runner: basename 導出（フルパス → basename）と `skills_injected = !NoWorktree || ForceInject` の単体テスト。
- server: `toRunnerInfo` が `AgentBin`/`SkillsInjected` を echo することのテスト（既存 runner_handler/task_handler テストに倣う）。
- codegen: `make protoregen`（brgen api server 必要。ユーザーは brgen 作者なので手元で起動可）。生成後 `go build ./...` と既存テストが通ること。
- 手動: 別 runner を `--claude-bin bash` や `--no-worktree` で立て、`harness-cli ls` に `agent=bash` / `agent=claude`（`+skills` 無し）が出ることを確認。

## 9. （関連機能）harness-cli への SKILL embed と `skill` サブコマンド

peer-identity で「この peer は skill 未注入」と見えても、その peer / オペレータが skill 本文を取得できるように、`harness-cli` 自体に SKILL.md を embed する。CLI さえあれば skill が手に入る。

### 共有 embed パッケージ
`runner/agentskills/embed.go`（新規, `package agentskills`）:
```go
//go:embed all:harness-cli
var FS embed.FS

// Skill returns the SKILL.md bytes for a named skill (e.g. "harness-cli").
func Skill(name string) ([]byte, error) { return FS.ReadFile(name + "/SKILL.md") }
```
`embed` のみ import の軽量パッケージ。重い `runner` 依存は引かない。import cycle 無し（agentskills は stdlib のみ）。

### runner 側（注入挙動は不変）
`runner/agentskill.go`: 自前の `//go:embed all:agentskills` / `var agentSkillsFS` を廃し、`agentskills.FS` を使う。`WriteAgentSkills` の walk root を `"agentskills"` → `"."` に調整（FS 直下が `harness-cli/` になるため）。materialize 先・上書き挙動・CLAUDE.md 生成は変更しない。

### harness-cli 側
`cmd/harness-cli/main.go` に `skill [name]` サブコマンド追加（既定 `harness-cli`）:
```go
case "skill":
    name := "harness-cli"
    if len(args) > 0 { name = args[0] }
    md, err := agentskills.Skill(name)
    if err != nil { die(err) }
    os.Stdout.Write(md)
```
`usage()` に `skill [NAME]   print the embedded agent skill (default: harness-cli)` を1行追記。

### テスト
- `agentskills.Skill("harness-cli")` が非空を返す / 未知名でエラーの単体テスト。
- runner の既存スキルテスト（`runner/agentskill_test.go`）が refactor 後も通ること。
- `go build ./...` 通過。

## 10. スコープ外 / 既知の限界（→ 後続設計）

- **`skills_injected` は現状 claude 専用の意味**: 実体は `!NoWorktree || ForceInject` ＝「`.claude/{settings.json,skills}` 注入をしたか」。harness の注入は claude 形式のみで、codex（`.agent/`・`AGENTS.md`）/ gemini（`GEMINI.md`）等は置き場・形式が異なる。よって `--claude-bin codex` で worktree モードだと `skills_injected=true`（`agent=codex+skills`）になるが、codex は `.claude/` を読まないため**実際には skill 非追従**。`+skills` は `agent_bin==claude` のときのみ意味を持つ、と SKILL.md に明記済み。
- **agent 別注入**（`agent_bin` で `.claude` / `.agent`・`AGENTS.md` / `GEMINI.md` を振り分け、`skills_injected` を agent 適合の意味へ拡張）は**別途設計する後続機能**。今回追加した `agent_bin` がその駆動信号になる。
- 本件では行わない: `--claude-bin` フラグのリネーム、上記 agent 別注入、agent/shell 自動分類の wire 化、`TaskInfo` への denormalize。
