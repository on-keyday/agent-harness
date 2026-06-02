# TUI one-key reattach / resume — design

- 日付: 2026-06-02
- 対象: `tui/app.go`（キーハンドラ + ヒント行）, `tui/taskaction.go`（新規・純関数ヘルパ）, `tui/taskaction_test.go`（新規）
- 種別: TUI キーバインド追加 + `i` の reattach 分岐除去（プロトコル/サーバ変更なし）

## 1. 問題

TUI で終了タスクを resume するには cmdline/popup で `session new --resume <id> --claude-arg --continue` を手打ちする必要があり面倒。reattach は `i`（Detached 選択時）で出来るが、resume にはワンキーが無い。`--continue` を毎回打つのが特に手間で、しかも `--continue` 不要なケース（まっさらな claude で同一 identity を再開）もあるので両方に素早く到達したい。

## 2. ゴール / 非ゴール

### ゴール
- タスクパネルで選択中のタスクに対し、**ワンキーで reattach / resume**。
- resume は **`r`=`--continue` 継続 / `R`=まっさら** を分けて即実行。

### スコープに含む（追記）
- **`i` から reattach 分岐を除去** — `i` は常に「新規（非 detachable）interactive を開く」に簡素化。reattach は `r` に一本化（`i` と `r` の重複・紛らわしさを解消）。

### 非ゴール
- `S`（detachable 新規）は**触らない**。
- **非 detachable interactive 自体の廃止**（`i` の open 先を detachable に変える等）は別件。今回は `i` の reattach 分岐を消すだけ。
- Running セッションの takeover reattach（reattach は Detached のみを対象）。

## 3. キーバインド設計

### 3.0 `i` の簡素化（reattach 除去）
現状 `i` は「Detached 選択時 reattach / それ以外は新規 interactive」だが、reattach は `r` に移すため、`i` は**常に新規（非 detachable）interactive を開く**だけにする。`tui/app.go` の `i` ハンドラから内側の Detached→`DoAttachSession` 分岐を削除し、`DoOpenInteractive(a.client, a.defaultRepo)` のみ残す。`S`（detachable 新規）は不変。

### 3.1 追加キー（`r` / `R`）

タスクパネル focus かつ選択タスクがあるとき:

- **`r`**（便利系・コンテキスト依存）:
  - `Detached` かつ `Detachable()` → **reattach**。
  - 終了状態（`Succeeded`/`Failed`/`Cancelled`）→ **resume + `--continue`**。
  - それ以外（Running/Queued 等）→ 何もせず cmdresult にヒント。
- **`R`**（fresh 系）:
  - 終了状態 → **resume（`--continue` 無し）**。
  - `Detached` かつ `Detachable()` → **reattach**（`--continue` は reattach に無関係なので `r` と同義）。
  - それ以外 → ヒント。

意味: 「**r/R どちらも選択セッションに再入。終了タスクのときだけ r=claude 記憶を継続 / R=まっさら**」。

## 4. 純関数ヘルパ（テスト容易化）

`tui/taskaction.go`:
```go
type taskActionKind int

const (
	actionNone taskActionKind = iota
	actionReattach
	actionResume
)

type taskAction struct {
	Kind       taskActionKind
	ResumeArgs []string // claude args for actionResume (["--continue"] or nil)
	Hint       string   // shown for actionNone
}

// resumeReattachAction decides what `r`/`R` should do for the selected task.
// withContinue is true for `r`, false for `R` (only affects the resume case).
func resumeReattachAction(t *protocol.TaskInfo, withContinue bool) taskAction {
	if t == nil {
		return taskAction{Kind: actionNone, Hint: "no task selected"}
	}
	if t.Status == protocol.TaskStatus_Detached && t.Detachable() {
		return taskAction{Kind: actionReattach}
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		var args []string
		if withContinue {
			args = []string{"--continue"}
		}
		return taskAction{Kind: actionResume, ResumeArgs: args}
	}
	return taskAction{Kind: actionNone,
		Hint: "r/R: pick a detached session (reattach) or a finished task (resume)"}
}
```

## 5. キーハンドラの配線（`tui/app.go`）

`c` のハンドラ付近（tasks-focus 用キー群）に追加:
```go
if a.focus == focusTasks && (msg.String() == "r" || msg.String() == "R") {
	act := resumeReattachAction(a.tasks.SelectedTask(), msg.String() == "r")
	switch act.Kind {
	case actionReattach:
		return a, DoAttachSession(a.client, a.tasks.SelectedID())
	case actionResume:
		// repo is irrelevant on resume — server reuses the task's RepoPath/branch.
		return a, DoOpenDetachableSession(a.client, "", cli.SelectorOpts{}, act.ResumeArgs, a.tasks.SelectedID())
	case actionNone:
		a.cmdresult.Append(WarnStyle.Render(act.Hint))
		return a, nil
	}
}
```
- `DoOpenDetachableSession`/`DoAttachSession` は既存（`tui/interactive.go`）。`ExtraArgs` は claude 引数そのもの（`--claude-arg` 相当の wrap は不要 — cmdline の `--claude-arg --continue` も最終的に `["--continue"]` になる）。

## 6. ヒント行

`tui/app.go:645` のヒント文字列に簡潔に追記（例）:
```
... · i interactive · r reattach/resume · R resume-fresh · F file picker · d detail · c cancel · q quit
```

## 7. テスト

`tui/taskaction_test.go`: `resumeReattachAction` の表駆動テスト:
- `nil` → actionNone。
- Detached+Detachable → actionReattach（withContinue 両方）。
- Succeeded/Failed/Cancelled × withContinue=true → actionResume, ResumeArgs=["--continue"]。
- 同上 × withContinue=false → actionResume, ResumeArgs=nil。
- Running/Queued → actionNone。
- （`Detachable()` は bitfield アクセサ。テストで `TaskInfo` に `SetDetachable(true)` 等で設定。）

手動: TUI 起動 → 終了タスク選択 → `r` で resume+continue、`R` で fresh resume、Detached タスクで `r` reattach、を確認。

## 8. スコープ外

- `i`/`S` の変更、非 detachable interactive の廃止、Running takeover reattach、popup/cmdline の resume UX 改修。
