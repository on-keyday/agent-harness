# WebUI モバイルタブレイアウト 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** スマホ(≤600px)で WebUI を端末優先の3タブ（端末/タスク/ファイル）にし、タスク行タップでIDコピペを撲滅し、Reattach 後に端末を最下部へ揃える。PCの体験は不変。

**Architecture:** フロントエンドのみ（`webui/index.html` / `webui/static/style.css` / `webui/static/main.js`）。Go/WASM/プロトコルは変更しない。必要なデータと操作はすべて既存 `window.harness.*` API で揃う。タブの表示切替は `body[data-active-tab]` 属性 + `@media (max-width:600px)` 内の CSS で行い、広幅では一切効かない（＝PC不変）。

**Tech Stack:** プレーン HTML / CSS / JS（ビルド不要）。xterm.js。検証は `--webui-dir` ホットリロード + ブラウザ手動確認（この層に自動テスト基盤は無い）。

**Spec:** `docs/superpowers/specs/2026-06-02-webui-mobile-tab-layout-design.md`

---

## 実装環境の前提（重要・全タスク共通）

- **作業ディレクトリは worktree 固定**: すべての編集とコミットを
  `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/`
  配下で行う。spec/plan は既にこの worktree ブランチ（`harness/0f0d4dd…`）に乗っている。
  親リポジトリの絶対パス `/home/kforfk/workspace/remote-agent-harness/webui/...` で編集すると
  **親リポの main checkout にルーティングされ worktree に反映されない**（既知の落とし穴 Pitfall 8）。
  git は worktree を cwd にして実行する。
- **ビルド不要**: 本計画は HTML/CSS/JS のみ。WASM は変更しないので `make webui-build` は不要。
- **検証方法**（各タスク末尾で実施）: サーバを `--webui-dir webui`（または `HARNESS_WEBUI_DIR=webui`）で
  起動し、ブラウザで開く。狭幅はブラウザ devtools のデバイスツールバー（幅≤600px）で再現可。
  この層はDOM自動テストが無いため、検証は人手のブラウザ確認。各タスクは「desктоп(>600px)で
  現状と一致（リグレッション無し）」を必ず含める。
- **既存パターン踏襲**（Pitfall 3 / sibling-code grep）: タスク行のクリック式DOMは既存の
  File picker（`main.js:208-243` の `<ul>/<li>` クリック実装）を手本にする。再接続後の
  `fit.fit()`+`resizeInteractive()` は既存の open/reattach 後処理（`main.js:628-629,655-656`）と
  同じ形を使う。

---

## ファイル構成

| ファイル | 責務 | 主な変更 |
|----------|------|----------|
| `webui/index.html` | DOM構造 | タブバー追加、各 `<section>` に `data-tabgroup`、起動系コントロールを `#compose` へ移設、`#touch-keys` を `#terminal` の下へ、`#task-list` を `<pre>`→`<div>` |
| `webui/static/style.css` | スタイル | タブバー、`≤600px` の active-tab 非表示、端末タブの flex レイアウト、タスク行/アクションシートの見た目 |
| `webui/static/main.js` | 挙動 | `setActiveTab` タブ機構、`renderTaskList`/`buildTaskSheet`（行タップ→アクションシート）、起動/再接続の自動タブ遷移、Reattach 後の `scrollToBottom` |

---

## Task 1: タブ機構の足場（端末既定・狭幅のみ・PC不変）

**Files:**
- Modify: `webui/index.html`（`<main>` 直下にタブバー、6セクションに `data-tabgroup`）
- Modify: `webui/static/style.css`（タブバー + active-tab 非表示ルール）
- Modify: `webui/static/main.js`（`setActiveTab` + リスナー、`term` 初期化直後）

- [ ] **Step 1: index.html — `<main>` 直下にタブバーを追加**

`<main>`（`index.html:17`）の開きタグ直後に挿入:

```html
    <nav id="tabbar" class="tabbar" role="tablist">
      <button type="button" class="tab-btn is-active" data-tab="terminal">端末</button>
      <button type="button" class="tab-btn" data-tab="tasks">タスク</button>
      <button type="button" class="tab-btn" data-tab="files">ファイル</button>
    </nav>
```

- [ ] **Step 2: index.html — 各セクションに `data-tabgroup` を付与**

```
<section id="runners">      → <section id="runners" data-tabgroup="tasks">
<section id="tasks">        → <section id="tasks" data-tabgroup="tasks">
<section id="compose">      → <section id="compose" data-tabgroup="tasks">
<section id="cmdline">      → <section id="cmdline" data-tabgroup="tasks">
<section id="files">        → <section id="files" data-tabgroup="files">
<section id="interactive">  → <section id="interactive" data-tabgroup="terminal">
```

- [ ] **Step 3: style.css — タブバーと active-tab 非表示ルールを末尾に追加**

```css
/* --- Mobile tab bar (active only at <=600px) --- */
.tabbar { display: none; }
.tab-btn {
  flex: 1;
  background: #2a2a2a;
  color: #aaa;
  border: 1px solid #555;
  padding: 0.5rem 0.4rem;
  font-size: 0.95rem;
  border-radius: 4px;
  cursor: pointer;
}
.tab-btn.is-active { background: #2d5; color: #000; font-weight: bold; border-color: #2d5; }

@media (max-width: 600px) {
  .tabbar {
    display: flex;
    gap: 0.4rem;
    position: sticky;
    top: 0;
    z-index: 100;
    background: #1e1e1e;
    padding: 0.4rem 0;
    margin-bottom: 0.6rem;
  }
  /* Show only the sections belonging to the active tab. */
  body[data-active-tab="terminal"] [data-tabgroup]:not([data-tabgroup="terminal"]),
  body[data-active-tab="tasks"]    [data-tabgroup]:not([data-tabgroup="tasks"]),
  body[data-active-tab="files"]    [data-tabgroup]:not([data-tabgroup="files"]) {
    display: none;
  }
}
```

- [ ] **Step 4: main.js — `term` 初期化直後にタブ機構を追加**

`window.harness_xtermWrite = (uint8Array) => term.write(uint8Array);`（`main.js:500`）の直後に挿入:

```js
  // --- Mobile tab switching (active only at <=600px via CSS). On desktop
  //     this only sets a body data-attr; the media query makes it a no-op. ---
  const tabbar = document.getElementById("tabbar");
  const setActiveTab = (name) => {
    document.body.dataset.activeTab = name;
    for (const b of tabbar.querySelectorAll(".tab-btn")) {
      b.classList.toggle("is-active", b.dataset.tab === name);
    }
    if (name === "terminal") {
      // Terminal was display:none under another tab; its grid is stale.
      try { fit.fit(); } catch (_) { /* not laid out yet */ }
      window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      term.focus();
    }
  };
  tabbar.addEventListener("click", (e) => {
    const btn = e.target.closest(".tab-btn");
    if (btn) setActiveTab(btn.dataset.tab);
  });
  setActiveTab("terminal");
```

- [ ] **Step 5: 手動検証（ブラウザ）**

`--webui-dir webui` でサーバ起動 → ブラウザで開く。
- 狭幅(≤600px, devtools): 上部に `[端末][タスク][ファイル]` の固定タブ。既定で端末セクションのみ表示。`タスク`タップで Runners/Tasks/Compose/Command が表示、`ファイル`タップで Files のみ表示。接続バナー/ヘッダは常時表示。
- 広幅(>600px): タブバー非表示、全6セクションが従来どおり縦積み。挙動・見た目が現状と一致。

- [ ] **Step 6: コミット**

```bash
git add webui/index.html webui/static/style.css webui/static/main.js
git commit -m "feat(webui): mobile 3-tab scaffolding (terminal default, <=600px only)"
```

---

## Task 2: touch-keys を端末の下へ + 端末タブの flex レイアウト

**Files:**
- Modify: `webui/index.html`（`#touch-keys` を `#terminal` の後ろへ移動）
- Modify: `webui/static/style.css`（端末タブを flex 縦並びにし端末可変高 + タッチキー下部）

- [ ] **Step 1: index.html — `#touch-keys` ブロックを `#terminal` の後ろへ移動**

現在 `#interactive` 内は touch-keys が terminal の前（`index.html:75-85`）。
`<div class="touch-keys" id="touch-keys"> … </div>` ブロック全体を切り取り、
`<div id="terminal"></div>` の**直後**に貼り付ける。結果の `#interactive` 末尾の順序:

```html
      <div id="attached-task"></div>
      <div id="terminal"></div>
      <div class="touch-keys" id="touch-keys">
        <button type="button" data-mod="ctrl" id="tk-ctrl">Ctrl</button>
        <button type="button" data-mod="shift" id="tk-shift">Shift</button>
        <button type="button" data-key="esc" id="tk-esc">Esc</button>
        <button type="button" data-key="tab" id="tk-tab">Tab</button>
        <button type="button" data-key="up"   id="tk-up">↑</button>
        <button type="button" data-key="down" id="tk-down">↓</button>
        <button type="button" data-key="left" id="tk-left">←</button>
        <button type="button" data-key="right" id="tk-right">→</button>
      </div>
```

- [ ] **Step 2: style.css — `≤600px` の端末タブ flex レイアウトを追加**

既存の `@media (max-width: 600px)` ブロック（`style.css:126`）内、`#terminal { height: 70vh; … }`（`style.css:169`）の定義の後に追加:

```css
  /* Terminal tab fills the viewport below the sticky tab bar; touch-keys sit
     at the bottom as a keyboard-accessory row (just above the OS keyboard).
     dvh tracks the shrinking viewport when the soft keyboard opens. */
  body[data-active-tab="terminal"] #interactive {
    display: flex;
    flex-direction: column;
    height: calc(100dvh - 4rem); /* tab bar + page padding; tune if it clips */
  }
  body[data-active-tab="terminal"] #interactive h2 { display: none; }
  #interactive #terminal { flex: 1 1 auto; min-height: 0; height: auto; }
  #interactive #touch-keys { flex: 0 0 auto; margin: 0.4rem 0 0; }
```

- [ ] **Step 3: 手動検証（ブラウザ）**

- 狭幅・端末タブ: タッチキー列が端末の**下**に表示される。端末が縦方向に伸びてビューポートを満たす。
- 実機（可能なら）: 端末をタップしOSキーボードを出すと、タッチキー列がキーボード直上に残る（端末スクロールで流れて消えない）。**clipする場合は `calc(100dvh - 4rem)` の `4rem` を増減して調整**。
- 広幅: 変化なし（これらは media query 内）。端末は従来表示、タッチキーは端末の下に来るが PC ではアクセサリ用途なので問題なし（必要なら確認のみ）。

- [ ] **Step 4: コミット**

```bash
git add webui/index.html webui/static/style.css
git commit -m "feat(webui): move touch-keys below terminal as keyboard-accessory row"
```

---

## Task 3: 起動/再接続コントロールをタスクタブへ移設 + 自動タブ遷移

**Files:**
- Modify: `webui/index.html`（起動系を `#interactive`→`#compose` へ、`#interactive` には stop だけ残す）
- Modify: `webui/static/main.js`（`openInteractive`/reattach で端末タブへ遷移、Reattach 後 `scrollToBottom`）

- [ ] **Step 1: index.html — `#interactive` の起動系を `#compose` へ移設**

`#interactive`（`index.html:61-`）冒頭の「Open ボタン群」「task-id + Reattach」「`<small>` 説明」を切り取り、
`#compose` の args 入力（`index.html:34`）の後ろへ貼り付ける。`#compose` は次の形になる:

```html
    <section id="compose" data-tabgroup="tasks">
      <h2>Compose</h2>
      <label for="runner-select">Repo:</label>
      <select id="runner-select"><option value="">(no runners)</option></select>
      <label for="host-select">Host pin:</label>
      <select id="host-select"><option value="">(any host)</option></select>
      <br>
      <label for="claude-args-input">Extra claude args (shell-quoted):</label>
      <input id="claude-args-input" type="text" size="60" placeholder='e.g. --add-dir "C:/path"'>
      <div class="actions">
        <button id="open-oneshot">Open one-shot interactive</button>
        <button id="open-detachable">Open detachable session</button>
      </div>
      <div class="actions">
        <label for="task-id-input">Task id:</label>
        <input type="text" id="task-id-input" placeholder="32 hex chars" size="40" pattern="[0-9a-fA-F]*">
        <button id="reattach">Reattach</button>
      </div>
      <small>One task id for every id-driven action: <b>Reattach</b> to a detached (live) session, or <b>resume</b> a terminal task's worktree branch via Submit / Open one-shot / Open detachable (Repo is ignored on resume). Leave empty to open a fresh task.</small>
    </section>
```

`#interactive` は起動系を抜いて次の形（stop ボタン・attached-task・terminal・touch-keys のみ）:

```html
    <section id="interactive" data-tabgroup="terminal">
      <h2>Interactive</h2>
      <div class="actions">
        <button id="stop-streaming">Stop streaming</button>
      </div>
      <div id="attached-task"></div>
      <div id="terminal"></div>
      <div class="touch-keys" id="touch-keys"> … (Task 2 の通り) … </div>
    </section>
```

注: 要素のIDは不変（`open-oneshot` 等）。`main.js` の `getElementById` 参照はそのまま動く。DOM上の位置だけが変わる。

- [ ] **Step 2: main.js — 端末用ヘルパ `scrollTermToBottom` を追加**

Task 1 で挿入した `setActiveTab` 定義ブロックの直後（`main.js` の `term` 初期化付近）に追加:

```js
  // scrollTermToBottom pins the viewport to the latest output. Called after
  // Reattach, whose replay otherwise leaves the viewport scrolled up. Triple
  // call (now + next frame + 120ms) catches async replay frames arriving via
  // recvPump after attachSession resolves. No-op/harmless in alt-screen apps.
  const scrollTermToBottom = () => {
    term.scrollToBottom();
    requestAnimationFrame(() => term.scrollToBottom());
    setTimeout(() => term.scrollToBottom(), 120);
  };
```

- [ ] **Step 3: main.js — `openInteractive` で端末タブへ遷移**

`openInteractive`（`main.js:613`）の repo/resume バリデーション直後（`term.reset();` の前）に1行追加:

```js
  const openInteractive = async (detachable, label) => {
    const req = composeRequest();
    if (!req.repo && !req.resumeTaskId) {
      alert("select a repo or fill in Resume task id");
      return;
    }
    setActiveTab("terminal");   // <-- ADD: surface the terminal tab on mobile
    term.reset();
    try {
      const taskID = await window.harness.startInteractive({...req, detachable});
      attachedTask.textContent = `attached: ${taskID} (${label})`;
      term.focus();
    } catch (e) {
      attachedTask.textContent = "";
      alert(`startInteractive: ${e.message}`);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  };
```

- [ ] **Step 4: main.js — Reattach ボタンで端末タブ遷移 + 最下部スクロール**

reattach ハンドラ（`main.js:640-657`）を次に置き換える（`setActiveTab` と `scrollTermToBottom` を追加）:

```js
  document.getElementById("reattach").addEventListener("click", async () => {
    const id = taskIdInput.value.trim();
    if (!id) {
      attachedTask.textContent = "(session id required)";
      return;
    }
    setActiveTab("terminal");   // <-- ADD
    term.reset();
    try {
      const taskID = await window.harness.attachSession(id);
      attachedTask.textContent = `attached: ${taskID} (reattached)`;
      term.focus();
      scrollTermToBottom();     // <-- ADD: land at the latest output
    } catch (err) {
      attachedTask.textContent = "";
      showError(err);
    }
    try { fit.fit(); } catch (_) { /* element not yet laid out */ }
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
```

- [ ] **Step 5: 手動検証（ブラウザ）**

- 狭幅: `タスク`タブの Compose 内に Open ボタン群と Task id + Reattach がある。`端末を開く`(one-shot/detachable)を押すと**端末タブへ切替**わりセッション開始。`Submit` はタスクタブに留まる（投入タスクが一覧に出る）。
- Reattach: 既存の detached セッションの id を入れて Reattach → 端末タブへ切替 → **端末が最下部にスクロールして最新出力位置**に揃う（上方に残らない）。
- 広幅: 全コントロールが（Compose 内に集約された状態で）従来どおり機能。

- [ ] **Step 6: コミット**

```bash
git add webui/index.html webui/static/main.js
git commit -m "feat(webui): move launch controls to tasks tab; auto-switch + scroll-to-bottom on reattach"
```

---

## Task 4: タスク一覧をタップ式に + アクションシート（IDコピペ撲滅の核）

**Files:**
- Modify: `webui/index.html`（`#task-list` を `<pre>`→`<div>`）
- Modify: `webui/static/style.css`（タスク行・アクションシートのスタイル）
- Modify: `webui/static/main.js`（`renderTaskList`/`buildTaskSheet` 追加、描画呼び出し差し替え、グローバル `renderTasks` 削除、`list` コマンド更新）

- [ ] **Step 1: index.html — `#task-list` を div 化**

```
<pre id="task-list"></pre>   →   <div id="task-list" class="task-list"></div>
```

- [ ] **Step 2: style.css — タスク行・アクションシートのスタイルを末尾に追加**

```css
/* --- Clickable task list + action sheet --- */
.task-list { background:#111; padding:0.4rem; border-radius:4px; max-height:240px; overflow:auto; font-size:0.85rem; }
.task-row { font-family: monospace; padding:0.45rem 0.5rem; border-radius:3px; cursor:pointer; white-space:pre-wrap; word-break:break-all; }
.task-row:hover { background:#222; }
.task-row:active { background:#2a2a2a; }
.task-empty { color:#888; padding:0.4rem; }
.task-sheet { display:flex; flex-wrap:wrap; gap:0.4rem; padding:0.3rem 0.5rem 0.6rem; }
.task-action { background:#2a2a2a; color:#fff; border:1px solid #555; border-radius:4px; padding:0.45rem 0.7rem; font-size:0.9rem; cursor:pointer; min-height:2.4rem; }
.task-action:hover { background:#3a3a3a; }
.task-action.danger { color:#f88; border-color:#a44; }
@media (max-width:600px){ .task-list { max-height:none; } }
```

- [ ] **Step 3: main.js — `TERMINAL_STATES` をファイルピッカー refs 付近に追加**

File picker の `let filePickerSelected = null;`（`main.js:128`）の直後に追加（`buildTaskSheet` が
描画時=`refreshSnapshot` 初回(`main.js:151`)で同期参照するため、その前に初期化が必要）:

```js
  // Terminal (finished) task states; gates Resume vs Cancel in the action sheet.
  const TERMINAL_STATES = new Set(["Succeeded", "Failed", "Cancelled"]);
```

- [ ] **Step 4: main.js — 描画呼び出しを差し替え**

`refreshSnapshot` 内の文字列描画（`main.js:148`）:

```js
    taskList.textContent   = renderTasks(snap.tasks);
```

を次に置き換え:

```js
    renderTaskList(snap.tasks);
```

- [ ] **Step 5: main.js — `renderTaskList` / `buildTaskSheet` を IIFE 内に追加**

reattach ハンドラの後・IIFE 終了 `})();`（`main.js:658`）の**直前**に追加。
（関数宣言はIIFE先頭へ巻き上げられるので `refreshSnapshot` 初回呼び出し時にも呼べる。
内部の `setActiveTab`/`term`/`fit`/`attachedTask`/`showError`/`scrollTermToBottom`/`fileTaskSelect`/
`refreshFilePicker`/`appendCmdOutput`/`refreshSnapshot` は**クリックハンドラ内でのみ**参照するため、
それらが後方で定義されていても click 時には初期化済みでTDZにならない。同期本体は `t` のフィールドと
`TERMINAL_STATES` のみ参照する。）

```js
  // renderTaskList builds clickable task rows into #task-list. Each row toggles
  // an inline action sheet; every action derives the id from the row, so the
  // user never copies a 32-hex id by hand. Modeled on the file-picker list
  // (main.js:208-243). Defined as a function declaration so refreshSnapshot()
  // (called before this point textually) can invoke it via hoisting.
  function renderTaskList(tasks) {
    taskList.innerHTML = "";
    if (!tasks || tasks.length === 0) {
      const empty = document.createElement("div");
      empty.className = "task-empty";
      empty.textContent = "(none)";
      taskList.appendChild(empty);
      return;
    }
    for (const t of tasks) {
      const wrap = document.createElement("div");
      const row = document.createElement("div");
      row.className = "task-row";
      const promptShort = (t.prompt || "").slice(0, 60);
      row.textContent = `${t.id.slice(0, 12)}…  ${t.status}  ${t.kind}  ${t.repoPath}  ${JSON.stringify(promptShort)}`;
      const sheet = document.createElement("div");
      sheet.className = "task-sheet";
      sheet.hidden = true;
      buildTaskSheet(sheet, t);
      row.addEventListener("click", () => {
        for (const s of taskList.querySelectorAll(".task-sheet")) {
          if (s !== sheet) s.hidden = true;   // single open sheet at a time
        }
        sheet.hidden = !sheet.hidden;
      });
      wrap.appendChild(row);
      wrap.appendChild(sheet);
      taskList.appendChild(wrap);
    }
  }

  // buildTaskSheet fills one task's action sheet, gating items by status/kind.
  // Each item stops propagation (so it doesn't re-toggle the row), runs its
  // harness call, and switches tabs where relevant.
  function buildTaskSheet(sheet, t) {
    const isTerminal = TERMINAL_STATES.has(t.status);
    const addItem = (label, cls, fn) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "task-action" + (cls ? " " + cls : "");
      item.textContent = label;
      item.addEventListener("click", (e) => { e.stopPropagation(); fn(); });
      sheet.appendChild(item);
    };

    // Reattach — live interactive session only.
    if (t.kind === "Interactive" && (t.status === "Running" || t.status === "Detached")) {
      addItem("↪ Reattach", "", async () => {
        setActiveTab("terminal");
        term.reset();
        try {
          await window.harness.attachSession(t.id);
          attachedTask.textContent = `attached: ${t.id} (reattached)`;
          term.focus();
          scrollTermToBottom();
        } catch (err) { attachedTask.textContent = ""; showError(err); }
        try { fit.fit(); } catch (_) {}
        window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      });
    }

    // Resume — finished task's worktree, opened as a fresh interactive session.
    if (isTerminal) {
      addItem("▶ Resume", "", async () => {
        setActiveTab("terminal");
        term.reset();
        try {
          const id = await window.harness.startInteractive({ repo: "", host: "", claudeArgs: [], resumeTaskId: t.id, detachable: true });
          attachedTask.textContent = `attached: ${id} (resumed)`;
          term.focus();
        } catch (err) { attachedTask.textContent = ""; alert(`resume: ${err.message}`); }
        try { fit.fit(); } catch (_) {}
        window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
      });
    }

    // Files — always available.
    addItem("📁 ファイル", "", () => {
      fileTaskSelect.value = t.id;
      filePickerCurDir = "";
      filePickerSelected = null;
      setActiveTab("files");
      refreshFilePicker();
    });

    // Cancel — non-terminal only.
    if (!isTerminal) {
      addItem("✕ Cancel", "danger", async () => {
        if (!window.confirm(`Cancel task ${t.id.slice(0, 12)}…?`)) return;
        try {
          await window.harness.cancel(t.id);
          appendCmdOutput(`cancelled ${t.id.slice(0, 12)}…`);
          refreshSnapshot();
        } catch (err) { appendCmdOutput(`cancel error: ${err.message}`); }
      });
    }
  }
```

- [ ] **Step 6: main.js — グローバル `renderTasks` を削除し、`list` コマンドを更新**

グローバル関数 `renderTasks`（`main.js:765-771`）を**削除**する（`renderTaskList` に置換済みで未使用。
`pad` は `renderRunners` が使うので残す）。

`runCmd` の `case "list":`（`main.js:420-426`）の出力を、div 化した一覧から組み立てる形に更新:

```js
        case "list":
          await refreshSnapshot();
          out = Array.from(taskList.querySelectorAll(".task-row"))
                  .map(r => r.textContent).join("\n") || "(none)";
          break;
```

- [ ] **Step 7: 手動検証（ブラウザ）**

- 狭幅・`タスク`タブ: 各タスク行がタップ可能。タップでアクションシート展開（別の行を開くと前のは閉じる）。
  - Interactive かつ Running/Detached の行: **Reattach** あり → タップで端末タブへ + 最下部スクロール。
  - 終了状態(Succeeded/Failed/Cancelled)の行: **Resume** あり → タップで端末タブへ + worktree 再開。
  - 全行: **ファイル** あり → タップでファイルタブへ + 当該タスク選択済みでブラウズ可能。
  - 非終了の行: **Cancel** あり → confirm → キャンセル。終了行には Cancel が出ない。
  - **どの操作でも32桁IDのコピペが発生しない**ことを確認。
- `list` コマンド: cmd-output に各行のテキストが改行区切りで出る。
- 広幅: 一覧がクリック式になっている以外は全セクション従来表示。リグレッション無し。

- [ ] **Step 8: コミット**

```bash
git add webui/index.html webui/static/style.css webui/static/main.js
git commit -m "feat(webui): tappable task rows with action sheet (reattach/resume/files/cancel), no id copy-paste"
```

---

## Task 5: 統合検証（全フロー通し + デスクトップ非回帰）

**Files:** なし（検証のみ。問題が出たら該当タスクのファイルを修正してコミット）

- [ ] **Step 1: 狭幅(≤600px)で全フロー通し**

1. 既定で端末タブ、0スクロールで端末に到達。
2. タスクタブ → 行タップ Reattach → 端末タブ + 最下部スクロール。
3. タスクタブ → Compose で `端末を開く` → 端末タブでセッション開始。タッチキーが端末下・キーボード直上。
4. タスクタブ → 終了行 Resume → 端末タブで worktree 再開。
5. タスク行 → ファイル → ファイルタブで当該タスクのファイルをブラウズ → Push/Pull/Delete。
6. タスク行 → Cancel → confirm → キャンセル。
7. タブ往復で端末グリッドが崩れない（端末タブ再表示時の `fit.fit()` が効いている）。

- [ ] **Step 2: 広幅(>600px)で非回帰確認**

タブバー非表示、全6セクション縦積み、起動系は Compose 内に集約された状態で全機能が現状どおり。

- [ ] **Step 3: 問題があれば修正してコミット。無ければ完了**

```bash
# 例: dvh の clip を調整した場合
git add webui/static/style.css
git commit -m "fix(webui): tune terminal-tab height to avoid touch-key clip"
```

---

## 自己レビュー結果（spec 突き合わせ）

- **Spec §3.1 狭幅のみタブ化 / PC不変** → Task 1 Step 3（`@media` 内に非表示ルールを閉じ込め）。
- **Spec §3.2 3タブ構成** → Task 1 Step 1-2（タブバー + `data-tabgroup`）。
- **Spec §3.3 起動系をタスクタブへ移設** → Task 3 Step 1。
- **Spec §3.4 touch-keys を端末下へ** → Task 2。
- **Spec §3.5 タスク行タップ + アクション出し分け** → Task 4 Step 5（status/kind ゲート）。
- **Spec §3.5.1 Reattach 後 scrollToBottom（三段）** → Task 3 Step 2,4 + Task 4 Step 5（Reattach 経路）。
- **Spec §3.6 自動タブ遷移 / Submit は遷移しない** → Task 3 Step 3-4 + Task 4 Step 5（Submit は未変更で据え置き）。
- **Spec §4 Go/WASM 変更なし** → 全タスク `webui/` のみ。
- **型/名称整合**: `setActiveTab`/`scrollTermToBottom`/`renderTaskList`/`buildTaskSheet`/`TERMINAL_STATES` は全タスクで同名。`window.harness.{snapshot,attachSession,startInteractive,cancel,resizeInteractive}` は既存 API（`cmd/harness-webui-wasm/main.go` で確認済み）。
- **プレースホルダ無し**: 各コード片は実コード。`calc(100dvh - 4rem)` の調整値のみ検証で詰める旨を明記（TODO ではなく可変パラメータ）。
