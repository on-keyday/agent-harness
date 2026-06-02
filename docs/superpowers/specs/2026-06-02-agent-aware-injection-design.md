# Agent-aware injection (cross-tool instructions + skills) — design

- 日付: 2026-06-02
- 対象: `runner/agentskill.go`（writer 拡張 + `claudeMdMinimal` 更新）, `runner/agentinjected.go`（`HarnessInjectedPaths` 追加）, `runner/agentskills/harness-cli/SKILL.md`（doc note + peer-identity caveat 更新）
- 種別: ランナー注入ロジックの拡張（プロトコル変更なし・`agent_bin` 分岐なし）

## 1. 問題

harness はタスク worktree に **claude 専用の設定だけ**注入している（`.claude/settings.json`(hooks) + `.claude/skills/<name>/SKILL.md` + 最小 `CLAUDE.md` ポインタ）。`--claude-bin` には codex / gemini / 等も入り得るが、それらは別のファイルを読む:

- **AGENTS.md** が Linux Foundation(AAIF)配下のクロスツール標準として確立し、Codex / Cursor / Gemini CLI / Aider / Copilot / Amp / opencode 等が読む（Claude Code は未対応で `CLAUDE.md` 必須）。
- **Skills は `SKILL.md` 形式が共通**だが、Claude は `.claude/skills/`、Codex は `.agents/skills/`（`$REPO_ROOT/.agents/skills` 等）を読む。
- **hooks**（harness の auto-inbox の土台）は Claude 固有の `.claude/settings.json`。Codex にも hooks はあるが本件スコープ外。

非 claude エージェントは現状この skill も指示も受け取れない。これを「指示＋skills」レベルでクロスツール化する。

## 2. ゴール / 非ゴール

### ゴール
- 指示（AGENTS.md/GEMINI.md/CLAUDE.md）と skills（`.agents/skills/` + `.claude/skills/`）をクロスツールで注入し、claude 以外（codex/gemini 等）も harness-cli skill を読めるようにする。
- 既存プロジェクトのファイル（自前の CLAUDE.md 等）は**不可侵**（上書きしない・触らない）。

### 非ゴール
- **auto-inbox / hooks の非 claude 移植**（codex hooks 等）。claude 固有のまま。別途設計。
- **git 自動除外機構**（`.git/info/exclude` 等）。harness worktree では `info/exclude` が共有 common dir を指し main checkout を汚すため不採用。注入物の非 commit は**現状どおり agent の自己判断**（自分の成果でないと認識して commit しない）に委ね、doc note で補助する（§6）。
- **`agent_bin` による出し分け**。形式が衝突しないので union を常に注入する（分岐不要）。`agent_bin`(peer-identity) は表示用途のまま。
- codex/`.agents/skills` の live 機能検証（検証アカウント無し。同形式で best-effort 注入）。

## 3. 注入内容（union, 分岐なし）

worktree 注入時（既存ガード `!NoWorktree || ForceInjectHarnessSettings` は不変）、常に以下を書く:

### 3.1 指示ポインタ（3ファイル、各「無ければ書く」）
`CLAUDE.md` / `AGENTS.md` / `GEMINI.md` に**同一の最小ポインタ内容**を書く。各ファイルは**既存なら書かない**（プロジェクト自前の物を保護＝現行 CLAUDE.md と同じ only-if-not-exists を3ファイルへ一般化）。

ポインタ内容（`claudeMdMinimal` を更新。全 agent が読める形に）:
```
This task runs inside a harness-managed worktree.

- `harness-cli` is on PATH; `HARNESS_*` env vars are pre-set by the runner.
- Read the harness-cli skill for agent-to-agent messaging on the agentboard:
  run `harness-cli skill harness-cli` (works in any agent), or open
  `.claude/skills/harness-cli/SKILL.md` / `.agents/skills/harness-cli/SKILL.md`.
- Reserved well-known topic for the initial handshake: `harness.hello`.

Harness-injected files in this worktree are NOT your work — do not commit them
as your own: this file (CLAUDE.md/AGENTS.md/GEMINI.md), `.claude/`, and
`.agents/skills/`. If you intentionally add project-specific content to one of
them, that addition IS legitimate work and may be committed.
```
`harness-cli skill harness-cli` 経由が**全 agent 共通の取得手段**（skills 機構を持たない gemini でもコマンドで全文を読める。§ harness-cli skill embed の成果を活用）。

### 3.2 skills（2ディレクトリ、同一 SKILL.md）
`agentskills.FS` の各スキルを **`.claude/skills/<name>/...` と `.agents/skills/<name>/...` の両方**に materialize（同一 `SKILL.md` 形式なので claude と codex 双方が読める）。上書き挙動は現行どおり（runner 更新を反映するため skills は常に上書き）。

### 3.3 claude 固有（不変）
`.claude/settings.json`（inbox hook 等）は従来どおり claude 用に注入。AGENTS.md/`.agents/skills` 側に等価物は無い（hooks はスコープ外）。

## 4. 既存ファイル保護

- 3ファイルのポインタはいずれも only-if-not-exists。プロジェクトが自前の `CLAUDE.md`/`AGENTS.md`/`GEMINI.md` を持つ場合は**書かない・触らない**。
- skills は harness 名前空間（`<name>` = `harness-cli` 等）配下のみ materialize。プロジェクト自前の他スキルには干渉しない。

## 5. `HarnessInjectedPaths` 更新（dirty-check）

`runner/agentinjected.go` の一覧に新パスを追加（worktree クリーンアップの「実作業か？」判定から除外するため）。更新後:
```go
var HarnessInjectedPaths = []string{
    "CLAUDE.md",
    "AGENTS.md",
    "GEMINI.md",
    ".claude/settings.json",
    ".claude/skills/",
    ".agents/skills/",
}
```
（writer と同期させる旨の既存コメントを遵守。）

## 6. agent の自己除外を助ける doc note

§3.1 のポインタ末尾の note（「harness-injected = commit するな / 但しプロジェクト固有追記は正当」）がこれを担う。加えて **`runner/agentskills/harness-cli/SKILL.md`** にも同主旨を1段落追記（どのファイルが harness-injected か、commit 方針、プロジェクト固有化したい場合の扱い）。git 機構ではなく**規約＋明示**で対応。

## 7. peer-identity の caveat 更新

`SKILL.md` の「Peers may not be claude」節の `+skills` 説明を更新: 注入が AGENTS.md + `.agents/skills` を含むようになったため、`+skills` は「**クロスツールで指示＋skill 在り**」を意味する（claude 限定でない）。**ただし auto-inbox（hooks）は依然 claude 固有**で、非 claude peer は手動 `harness-cli agent inbox` が要る旨を残す。

## 8. テスト / 検証

- 単体: `WriteAgentSkills` 後に worktree に `AGENTS.md`/`GEMINI.md`/`CLAUDE.md`（無かった場合）と `.claude/skills/harness-cli/SKILL.md`/`.agents/skills/harness-cli/SKILL.md` が存在すること。既存 CLAUDE.md がある場合は**上書きされない**こと。`HarnessInjectedPaths` と writer の整合。
- 手動（手持ち環境）: **gemini CLI** で worktree の `AGENTS.md`/`GEMINI.md` が読まれること。**claude** で従来どおり（`.claude/` + CLAUDE.md）。`harness-cli skill harness-cli` が全文を出すこと（既出機能）。
- codex `.agents/skills` は同形式注入のみ（live 検証は環境無しで対象外、best-effort）。

## 9. スコープ外（→ 後続）

- 非 claude への auto-inbox/hooks 移植（codex hooks 等）。
- git 自動除外機構（per-worktree `core.excludesFile` 等）。
- `agent_bin` による注入の出し分け（union で足りるため不要）。
- `agents/openai.yaml`(codex skill UI/policy) の生成。
