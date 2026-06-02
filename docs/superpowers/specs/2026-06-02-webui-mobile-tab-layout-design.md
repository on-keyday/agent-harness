# WebUI モバイル向けタブレイアウト 設計

- 日付: 2026-06-02
- 対象: `webui/index.html`, `webui/static/style.css`, `webui/static/main.js`
- 種別: フロントエンドのみ（Go / WASM の変更なし）

## 1. 問題

現在の WebUI は全6セクション（Runners / Tasks / Compose / Command / Files / Interactive）を縦1列に積む単一ページ。PC では一望できて問題ないが、スマホでは次の摩擦がある:

1. **端末まで遠い**: Interactive（端末, 70vh）が最下段。端末で対話するのが主用途なのに、到達まで5セクション分スクロールする。端末＋ソフトキーでビューポートが埋まると上の各セクションは完全に画面外になり、「端末」と「それ以外」がスクロールで物理的に分断される。
2. **ファイル操作で行き過ぎる**: Files が縦積みの途中にあり、毎回スクロールで通り過ぎる。
3. **タスクIDの手コピペがつらい**: Tasks 一覧は非インタラクティブな `<pre>` テキスト。reattach / resume / file 操作は32桁hexのIDを要するが、ユーザがテキストから目で読んで手でコピペするしかない。スマホでの範囲選択コピペは特に苦痛。

主用途は「スマホから端末で claude と対話」。

## 2. ゴール / 非ゴール

### ゴール
- スマホで端末まで0スクロールで到達できる。
- 端末 ↔ 一覧/起動 ↔ ファイルの往復を最小操作（タブ1タップ）にする。
- ファイルを独立させ「行き過ぎスクロール」を解消する。
- タスクIDの手コピペを撲滅する（行タップ駆動）。
- PC（広幅）の体験は現状から不変更。

### 非ゴール
- Go / WASM / プロトコル / スキーマの変更。本件は既存 `window.harness.*` API のみで完結する。
- 端末エミュレーション（xterm）の挙動変更、single-writer ガード等の既存ロジック変更。
- 機能追加（新しい操作の導入）。あくまで既存機能の再配置とタップ駆動化。

## 3. 設計

### 3.1 レイアウト方針: 狭幅でのみタブ化

- ブレークポイント `≤600px`（既存のモバイル用 media query と一致, `style.css:111,126`）でのみタブ UI を有効化する。
- **広幅（>600px）では現状どおり全セクションを縦積み表示**。タブバーは `display:none`、各セクションは常時表示。
- タブによる表示切替は CSS の `.is-hidden-tab` クラスで行うが、その `display:none` 効果を `@media (max-width:600px)` の内側に閉じ込める。これにより JS がタブ状態クラスを付与しても広幅では一切影響しない。

### 3.2 タブ構成（3タブ）

上部 sticky のタブバー。既定タブ = **タスク**（接続直後はセッションが無いため、空の端末より一覧が有用。実機検証で変更）。

タブ切替時は `window.scrollTo(0, 0)`（狭幅 `matchMedia("(max-width:600px)")` のときのみ）でスクロールをトップへ戻す。長いタスク一覧の途中からファイルタブ等へ切替えたとき、前のスクロール位置が残って新タブ内容が見切れるのを防ぐ。広幅では全セクション同時表示なので適用しない（タスク行のアクション操作でページが飛ばないように）。

| タブ | 含むセクション | 補足 |
|------|----------------|------|
| 端末 | `#interactive`（端末本体） | 接続/attached表示・切断ボタン・xterm・タッチキー。スクロール最小。 |
| タスク | `#runners` `#tasks` `#compose` `#cmdline` ＋起動ボタン群 | 状態確認と「起動」の場所。 |
| ファイル | `#files` | 独立タブ。 |

### 3.3 セクションの再配置

現状 `#interactive` 内にある起動系コントロール（Open one-shot / Open detachable / reattach 用の task-id 入力）を **タスクタブ側（`#compose` 近傍）へ移す**。

- **端末タブ (`#interactive`)** に残すもの: `attached-task` 表示、切断（detach/stop streaming）ボタン、`#terminal`、タッチキー。
- **タスクタブ (`#compose`)** に集約するもの: Repo / Host / args、`Submit`、`端末を開く`（one-shot / detachable）、（フォールバック用の）task-id 手入力＋Reattach。

### 3.4 touch-keys を端末の下へ

現状 `#touch-keys` は `#terminal` の**前**（上）にある（`index.html:75-85`）。これを `#terminal` の**後**（下）へ移動する。

- 狙い: 自前キー列を「端末 ↔ OSソフトキーボードの間」に置き、OSキーボード直上のアクセサリ行にする。親指移動が最小化され、Ctrl→文字キーの連続入力が自然になる。
- **visualViewport によるキーボード追従（実機検証で判明・必須）**: iOS/Android はソフトキーボードを**コンテンツに被せる**挙動で、`dvh` も縮まない。そのため `height: calc(100dvh - …)` 固定だと、端末下のタッチキー列も、入力中の行も、キーボード裏に隠れて別途スクロールが要る。解決として、端末タブの `#interactive` の高さを **`window.visualViewport` の可視領域に動的に合わせる**:
  - `height = visualViewport.height − (#interactive の可視上端)`（`getBoundingClientRect().top − visualViewport.offsetTop`）。
  - `visualViewport` の `resize`/`scroll` を `requestAnimationFrame` で1フレーム1回に間引いて再計算し、毎回 `fit.fit()` + `resizeInteractive()`。
  - 端末タブは flex 縦並び（端末 `flex:1` ＋ タッチキー下部）のままなので、コンテナがキーボード上の可視領域ぴったりになることで、端末もタッチキーも自動的にキーボードの上に収まる（position:fixed は使わない）。
  - フォールバック: `visualViewport` 非対応時・広幅・端末タブ以外では inline height を空にし、CSS の `calc(100dvh - 4rem)` に戻す。

### 3.5 タスク行のタップ駆動（コピペ撲滅の核）

タスクタブの Tasks 一覧を、非インタラクティブな `<pre>` から**行クリック可能なリスト**に変更する（既存の File picker の `<ul>`/`<li>` クリック実装 `main.js:208-243` が手本）。

- 行タップ → アクションシート表示。項目と表示条件（snapshot の `status` / `kind` 文字列で判定）:
  - **Reattach** → `window.harness.attachSession(id)` を呼び、**端末タブへ自動遷移**。再接続後に `term.scrollToBottom()` で端末を最下部へスクロールし、最新出力位置に合わせる（後述 3.5.1）。表示条件: `kind === "Interactive"` かつ `status ∈ {Running, Detached}`（再接続可能な生きた対話セッション）。
  - **Resume** → `window.harness.startInteractive({repo:"", host:"", claudeArgs:[], resumeTaskId: id, detachable:true})` で当該タスクの worktree を再開。実行後**端末タブへ自動遷移**。表示条件: `status ∈ {Succeeded, Failed, Cancelled}`（終了済みタスクの worktree 再開）。
  - **このタスクのファイル** → **ファイルタブへ自動遷移**し、`file-task-select.value` を当該IDに設定して `change` を発火（既存 `refreshFilePicker()` 経路）。表示条件: 常時。
  - **Cancel** → `window.harness.cancel(id)`（破壊的操作は `confirm` ガード）。表示条件: `status ∈ {Queued, Running, Detached}`（非終了状態）。
- IDは内部（クリックされた行の `task.id`）から補完するため、**ユーザは一度もIDをコピペしない**。
- status 文字列の定義: 非終了 = `Queued`/`Running`/`Detached`、終了 = `Succeeded`/`Failed`/`Cancelled`（`runner/protocol/message.go` の `TaskStatus.String()`）。kind = `Oneshot`/`Interactive`。

### 3.5.1 Reattach 後の最下部スクロール

現状の Reattach はリングバッファのリプレイで過去スクロールバックを書き戻すため、再接続直後にビューポートが上方に残り、ユーザが毎回手で最下部までスクロールする必要がある。これを解消する。

- 機構: Reattach 経路（タスク行タップの Reattach、および手入力ID用の Reattach ボタン）で、`attachSession` 解決後に `term.scrollToBottom()` を呼ぶ。
- **非同期リプレイの考慮**: リプレイフレームは `attachSession` の解決後に `recvPump`（wasm側）経由で遅れて届き得る。`attachSession` 直後の1回だけでは末尾フレーム到着前にスクロールが確定し最下部に揃わない可能性がある。そこで、直後の1回に加えて `requestAnimationFrame` で1回、さらに後続フレームを取りこぼさないよう短い遅延（120ms）でもう1回 `scrollToBottom()` を呼ぶ三段構えとする。alt-screen を使うフルTUIでは scrollToBottom が無効（スクロールバックなし）だが副作用は無いので無害。
- 対象外: Resume / Open one-shot / Open detachable はフレッシュ出力で自然に最下部に追従するため変更しない。

### 3.6 自動タブ遷移のまとめ

- タスクタブで `端末を開く`（one-shot / detachable）を実行 → **端末タブへ**。
- **`Submit` は背景タスク投入で端末を開かないため、タブ遷移しない**（タスクタブに留まり、投入したタスクが一覧に現れるのを見られる）。
- 行タップ Reattach / Resume → 端末タブへ。
- 行タップ「このタスクのファイル」→ ファイルタブへ（Task 選択済み）。
- 広幅ではタブが無いので、これらの「遷移」はノーオペ（全セクション可視のまま）。`setActiveTab()` は body の data 属性を更新するだけで、広幅では CSS media query により非表示効果が無効。

### 3.6.1 フォーカス（キーボード）ポリシー

WebUI は端末を**一切自動 focus しない**（モバイルでソフトキーボードが勝手に開くのを避ける）。`setActiveTab()` のタブ切替、Open / Reattach / Resume の初期状態、さらに**タッチキー（`sendSeq`）押下でも** `term.focus()` を呼ばない。タッチキー（Ctrl/Shift/Esc/Tab/矢印等）は `sendInteractive(seq)` で PTY へ直接送るため focus 不要で、これにより softkey 単独操作（例: Shift+Tab で auto mode 切替）でキーボードが開かない。OS ソフトキーボードが開く唯一の経路は、**ユーザが端末を明示的にタップしたとき**（xterm 自前の click→focus）。

## 4. 影響範囲とデータフロー

- **`index.html`**: タブバー要素の追加、`#interactive` から起動系コントロールを `#compose` へ移設、`#touch-keys` を `#terminal` の後ろへ移動、Tasks 表示を `<pre>` からクリック可能リストへ。
- **`style.css`**: タブバーのスタイル、`≤600px` 内での `.is-hidden-tab { display:none }`、端末タブの「端末可変高＋タッチキー下部固定」、タスク行のタップ可視覚（hover/active）。
- **`main.js`**: タブ状態管理（active tab、切替関数、`setActiveTab()`）、`renderTasks` をDOM生成＋行クリックハンドラへ刷新（現状は文字列を `taskList.textContent` に入れている `main.js:148,765-771`）、アクションシートの生成と各 `window.harness.*` 呼び出し、起動ボタンのハンドラ移設、自動遷移フック。
- **Go / WASM**: 変更なし。必要なデータ（runners/tasks/各種操作）はすべて既存 `window.harness` API（`snapshot` / `attachSession` / `startInteractive` / `cancel` / `fileLs` 等）で取得・実行可能。

## 5. エラー処理 / フィードバック

- 既存のフィードバック経路（`attachedTask` 行、`cmd-output`、`file-result`、`confirm`/`alert`）を踏襲。タブ化で「操作した結果がどのタブに出るか」がズレないよう、行タップ起点の結果表示はそのアクションの遷移先タブ内（端末タブ or ファイルタブ）に出す。
- snapshot エラー時は既存同様タスクタブ内にエラー文を出す（`main.js:135`）。

## 6. テスト / 検証

- 自動テストは無い領域（静的フロント）。**実機/ブラウザでの手動検証**を `--webui-dir` ホットリロードモード（`07aa307`）で行う。
  - `make webui-build` → サーバを `--webui-dir webui` で起動 → ブラウザ更新。
- 検証観点:
  1. 広幅(>600px)で見た目・挙動が現状と完全一致（リグレッションなし）。
  2. 狭幅(≤600px)で3タブが出て、既定が端末、切替が機能する。
  3. 端末タブで0スクロール到達、タッチキーが端末下＆キーボード直上に残る。
  4. タスク行タップ→各アクション→正しいタブへ自動遷移し、IDコピペ不要で動く。
  5. ファイルタブ独立で行き過ぎが起きない。
- xterm の `fit.fit()` はタブ表示切替（`display:none`→可視）時に再計算が要る。タブ遷移で端末タブを表示した直後に `fit.fit()` ＋ `resizeInteractive()` を呼ぶ（既存の open/reattach 後処理 `main.js:628-629,655-656` と同じパターン）。

## 7. リスク / 留意点

- **`display:none` と xterm**: 非表示タブの端末は寸法0になる。端末タブを再表示した瞬間に必ず `fit.fit()` を呼ばないとグリッドが崩れる。最重要の実装注意点。
- **sticky タブ × OSキーボード**: 上部 sticky タブは通常キーボードに干渉しないが、`100vh` 系の高さ指定は iOS で誤差が出る。端末タブの高さは `vh` 直値より flex ベースの可変高を推奨。
- **タッチキー下部固定の高さ計算**: OSキーボード表示時にタッチキー列が隠れない配置を実機確認する（既存メモリの sticky×keyboard 地雷に近い領域）。
- **広幅判定の単一しきい値(600px)依存**: タブCSSと既存モバイルCSSの境界を一致させる。中間幅での崩れに注意。

## 8. スコープ外（やらないこと）

- タスク一覧のページング/検索、新規操作コマンドの追加、端末の複数同時セッション、デスクトップUIの作り替え。本件はモバイル摩擦3点（端末到達・ファイル独立・IDコピペ）の解消に限定する。
