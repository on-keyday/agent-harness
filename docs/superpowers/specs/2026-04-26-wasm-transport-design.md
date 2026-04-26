# WASM 対応 (transport refactor + ブラウザ向けビルド) 設計

**Date:** 2026-04-26
**Status:** approved (controller-side); awaiting user review
**Scope:** transport 層リファクタ + 既存 tui 全機能の wasm 化（ブラウザでの動作）

## Goal

`harness-tui` と同等の機能（submit / list / cancel / watch / prune / interactive PTY）をブラウザ上で動かす。

- 既存の `harness-tui`（bubbletea / native ターミナル UI）はそのまま残す。ブラウザ版は別エントリ (`cmd/harness-webui-wasm`) と静的アセット (`webui/`) で並列に提供する。
- core プロトコル層（`objproto` / `peer` / `trsf` / `runner/protocol` の bgn-generated）は native と wasm でソース・バイナリ表現を共有する。`Q3` の検証で wasm ビルド可否は確認済み。
- transport 層は build tag (`//go:build js`) で native / wasm を切り替える。`peer.Dial` の Endpoint 引数化リファクタ（先行完了）が前提条件。
- 認証は省略（toy-scope を継承）。`harness-server` のデフォルト listen は `127.0.0.1:8539`（loopback）。LAN 公開は `--listen=:8539` で caller 判断、認証は上位層で別途。

---

## Phase 分割

WASM 対応は **2 フェーズ**で進める。

### Phase 1: transport refactor（前提整備）

WS transport API を構造体化＋ Client/Server/Mutual 3 関数分離＋ caller-owned mux/http.Server に切り替える。
**wasm に手を付けず native のままで完結させる**。Phase 1 終了時点で既存機能（cli / runner / server / tui）は全部動いている状態を維持。

### Phase 2: wasm 本体

Phase 1 で整理された transport API の上に、wasm 専用 transport 実装、xterm.js bridge を含む interactive 経路、ブラウザ用エントリ、静的アセット、`harness-server` の embed.FS 配信ハンドラを追加。

> 本 spec はこの 2 フェーズを 1 ドキュメントに含めるが、**plan は 2 つに分ける**。Phase 1 の plan を完了してから Phase 2 の plan を書き起こす。Phase 1 の途中で Phase 2 に進まない。

---

## Architecture（Phase 2 完了後）

```
+-------------------------------------------------------------+
| Browser (http://127.0.0.1:8539/)                            |
|                                                             |
|  +----------------------+   +----------------------------+  |
|  | index.html           |   | main.wasm (Go runtime)     |  |
|  | + main.js            | js|                            |  |
|  | + xterm.js           |<=>|  cli.{Submit, List,        |  |
|  | + wasm_exec.js       | / |       Cancel, Watch,       |  |
|  |                      | js|       Prune}               |  |
|  | DOM:                 |   |  cli.Interactive (wasm)    |  |
|  |  - runner panel      |   |  peer.Dial                 |  |
|  |  - task table        |   |  transport.WebSocket       |  |
|  |  - cmdline           |   |    ClientEndpoint (wasm)   |  |
|  |  - xterm container   |   |  objproto / trsf           |  |
|  +----------------------+   +----------------------------+  |
|                                          |                  |
+------------------------------------------|------------------+
                                           | WebSocket /ws
                                           v
+------------------------------------------|------------------+
| harness-server (native, --listen=...)    v                  |
|                                                             |
|  http.ServeMux                                              |
|   +- /         -> embed.FS: index.html + main.js + ...      |
|   +- /static/* -> embed.FS: main.wasm + wasm_exec.js +      |
|   |                          xterm.js + xterm.css           |
|   +- /ws       -> transport.WebSocketServerEndpoint で      |
|                   登録された accept handler                 |
|                                                             |
|  http.Server{Addr: cfg.Addr, Handler: mux} を caller 所有   |
|                                                             |
|  task store, runner registry, WAL (既存)                    |
|                                                             |
|  agent-runner --- peer.Dial (native) --- claude (PTY)       |
+-------------------------------------------------------------+
```

---

## Phase 1: transport refactor

### 設計判断（決定済み）

- **R3 (server-side の `http.Server` lifecycle を caller に移す)**: `transport` は accept handler の生成と (caller から渡された mux への) 登録、および常駐 goroutine の起動だけを担い、`http.Server` lifecycle と mux 構築は caller (`harness-server`) が所有する。
- **構造体化**: 引数の意味を field 名で明示するため、`WebSocketConfig` 単一構造体に統合。
- **Client / Server / Mutual を関数で分離**: mode 別の関数で `mux` 引数の有無を signature 上に表現。
- **mux/path を引数で受ける**（Server / Mutual）: caller は `*http.ServeMux` を渡し、callee が `mux.Handle(cfg.Path, handler)` を実行する。caller の登録忘れを構造的に防ぐ。
- **path は caller が決める**: `transport` パッケージは path 規約を持たない。`cli.WebSocketPath` (var、デフォルト `/ws`) が harness 統合層の規約所有点。各 cmd/main で `--ws-path` flag によって override 可能。
- **Mutual API 表面**: `WebSocketMutualEndpoint(Ex)` を用意するが、現状の caller はゼロ。後の対称性と dualstack 整合のため出しておく。
- **dualstack**: 新 API に追従させて caller 不在のまま保存（コード形態のドキュメント）。
- **`handleRawEndpoint` のロジック保存**: 関数名と引数を変えるが中身は流用。`startTransportLoops` にリネーム、`dialPath` 引数を追加。
- **不変条件 (godoc)**: `objproto.SendHandshake` が server-mode を弾く（`objproto/objproto.go:639-641`）ため、sender loop に Handshake が到達するのは Client / Mutual 限定。`startTransportLoops` の dial 経路はこの上流前提に依存する。

### transport API（最終形）

```go
package transport

type WebSocketConfig struct {
    Logger *slog.Logger
    // Path is the WS URL path used by both:
    //   - Client / Mutual dial: passed as Location.Path
    //   - Server / Mutual accept: used in mux.Handle(cfg.Path, handler)
    // The caller is responsible for keeping client and server values aligned;
    // see cli.WebSocketPath as the canonical harness-side convention.
    Path string
    // TLS is consulted only for Origin scheme decisions (ws:// vs wss://).
    // The listen-side TLS is owned by the caller's *http.Server.
    TLS *tls.Config
}

func WebSocketClientEndpoint(cfg WebSocketConfig) (objproto.Endpoint, error)
func WebSocketServerEndpoint(mux *http.ServeMux, cfg WebSocketConfig) (objproto.Endpoint, error)
func WebSocketMutualEndpoint(mux *http.ServeMux, cfg WebSocketConfig) (objproto.Endpoint, error)

// Ex 系: rawSess を caller が共有したいとき (dualstack 等)
func WebSocketClientEndpointEx(rawSess objproto.RawEndpoint, cfg WebSocketConfig) error
func WebSocketServerEndpointEx(rawSess objproto.RawEndpoint, mux *http.ServeMux, cfg WebSocketConfig) error
func WebSocketMutualEndpointEx(rawSess objproto.RawEndpoint, mux *http.ServeMux, cfg WebSocketConfig) error
```

### dualstack の追従

```go
type UDPWebsocketDualStackConfig struct {
    Logger  *slog.Logger
    UDPPort uint16
    Mux     *http.ServeMux       // server/mutual モード時に必須、Client では nil 可
    WS      WebSocketConfig      // Path / TLS / Logger を共有
    Mode    objproto.EndpointMode
}

type UDPWebsocketDualStack struct {
    Endpoint objproto.Endpoint
}

func UDPWebsocketDualStackEndpoint(cfg UDPWebsocketDualStackConfig) (UDPWebsocketDualStack, error)
```

caller 不在のまま新 API に追従するだけ（コード形態をドキュメントとして保存）。

### caller 修正

| ファイル | 変更内容 |
|---|---|
| `transport/websocket.go` | 上記 API 実装。`startTransportLoops` (旧 `handleRawEndpoint`) は中身を流用、`dialPath` 引数追加。`http.Server` 内部所有を撤去 |
| `transport/dualstack.go` | 上記新 API への追従 |
| `server/server.go:238` | 旧 `transport.WebSocketEndpoint(...)` 呼びを廃止し、`mux` を Config 経由で受けて `WebSocketServerEndpoint(mux, cfg)` を呼ぶ。`http.Server.ListenAndServe` を caller 側 (`server.Run` か `cmd/harness-server/main.go`) で起動 |
| `cli/client.go:38` | `WebSocketClientEndpoint(WebSocketConfig{..., Path: cli.WebSocketPath})` に書き換え |
| `runner/connect.go:34` | 同上 |
| `cmd/harness-server/main.go` | mux 構築、WS handler 登録、`http.Server` lifecycle 所有。Phase 1 では `/` には何も載せず、Phase 2 で embed.FS 追加 |
| `cmd/harness-cli/main.go`、`cmd/agent-runner/main.go`、`cmd/harness-tui/main.go`、`cmd/harness-server/main.go` | `--ws-path` flag を追加（デフォルト `/ws`）、`cli.WebSocketPath = *wsPath` で override |
| `cli/path.go` (新規・1 ファイル) | `package cli; var WebSocketPath = "/ws"` のみ |

---

## Phase 2: wasm 本体

### 新規 / 修正ファイル

#### transport 層

| ファイル | build tag | 責務 |
|---|---|---|
| `transport/websocket_wasm.go` (新規) | `//go:build js` | `syscall/js` 経由でブラウザ WebSocket API をラップ。`WebSocketClientEndpoint(Ex)` のみ実装 (server / mutual は wasm 環境では成立しない) |
| `transport/websocket.go` の native 専用部分 | `//go:build !js` | server / mutual / `golang.org/x/net/websocket` 依存箇所を native 限定に。`WebSocketConfig` 等の純構造体定義は build constraint なしで両側から参照される |

#### cli 層

| ファイル | build tag | 責務 |
|---|---|---|
| `cli/client.go` `cli/submit.go` `cli/list.go` `cli/cancel.go` `cli/logs.go` `cli/watch.go` `cli/get_log.go` `cli/prune.go` | 制約なし | 通信系。native / wasm 両対応（既に通信のみで FS 不要） |
| `cli/prune_local.go` (新規) | `//go:build !js` | `PruneLocal` 関数を独立ファイルに分離。`os/exec` + `os.ReadDir` を使うので native 限定 |
| `cli/open_interactive.go` (削除) | — | 中身を 2 ファイルに分割 |
| `cli/open_interactive_native.go` (新規) | `//go:build !js` | 既存 `creack/pty` + `exec.ExecuteCommand` 経路 |
| `cli/open_interactive_wasm.go` (新規) | `//go:build js` | xterm.js bridge：bidirectional stream の bytes を `js.Global().Call("harness_xtermWrite", ...)` に流し、xterm onData 入力を `js.FuncOf` 経由で stream に書き戻す |

#### wasm エントリ + webui

| ファイル | build tag | 責務 |
|---|---|---|
| `cmd/harness-webui-wasm/main.go` (新規) | `//go:build js` | wasm エントリポイント。`js.FuncOf` で JS 公開関数を登録 (`harness.connect()`, `harness.submit()`, `harness.list()`, `harness.cancel()`, `harness.watch()`, `harness.prune()`, `harness.startInteractive()`, `harness.detachInteractive()`, `harness.resizeInteractive()` など)。内部で `cli.Dial` → `cli.*` を呼ぶ |
| `webui/index.html` (新規) | static | DOM レイアウト：runner pane、task pane、cmdline、xterm container |
| `webui/main.js` (新規) | static | wasm 起動 (`new Go(); WebAssembly.instantiateStreaming(...)`)、xterm 初期化、`harness.*` 関数の DOM 配線、`harness_xtermWrite` callback 登録 |
| `webui/style.css` (新規) | static | スタイル |
| `webui/vendor/xterm.js`、`webui/vendor/xterm.css`、`webui/vendor/wasm_exec.js` (新規) | static | ベンダリング（CDN ではなく repo 同梱で再現性確保） |
| `cmd/harness-server/main.go` | 制約なし | `embed.FS` で `webui/` を同梱、mux に `/`、`/static/*` の static handler 登録。`/ws` は Phase 1 で済 |

#### ビルド支援

| ファイル | 責務 |
|---|---|
| `Makefile` (or `webui/build.sh`) | `GOOS=js GOARCH=wasm go build -o webui/main.wasm ./cmd/harness-webui-wasm` を回す。`wasm_exec.js` のコピーも (`go env GOROOT` から取得) |

### コンポーネント間の境界

- **`transport` パッケージは path 規約を持たない**。caller が `WebSocketConfig.Path` を埋める。
- **`cli.WebSocketPath` が harness 統合層の規約所有点**。各 cmd/main からの override は flag 経由。wasm では override しない（デフォルト `/ws` 固定）。
- **`peer` / `objproto` / `trsf` / `runner/protocol` (bgn-generated) はビルド制約なし**。
- **`runner` / `server` は native 専用**（claude PTY と FS / WAL に必要）。
- **`harness-server` がブラウザ向け配信ハンドラ + WS endpoint の合流点**。`cmd/harness-server/main.go` だけが `embed.FS` を持つ。
- **`webui/` は wasm を `/static/main.wasm` で読む静的フロントエンド**。Go コードと境界は `js.FuncOf` の関数表面のみ。

### 不変条件

- `cli` パッケージから `creack/pty` への直 import は **`!js` ファイル限定** に隔離する。`cli` のどの関数も `import` 連鎖で wasm ビルドを壊さない状態を維持。
- wasm ビルド検証は `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm` を継続する。

---

## Data flow

### Flow 1: ブラウザ起動 → connection 確立

| # | 主体 | 内容 |
|---|---|---|
| 1 | User | `http://127.0.0.1:8539/` を開く |
| 2 | harness-server | embed.FS から `webui/index.html` を返す |
| 3 | Browser | `index.html` が `/static/main.js`、`/static/wasm_exec.js`、`/static/main.wasm` を fetch |
| 4 | main.js | `new Go(); WebAssembly.instantiateStreaming(...)` で wasm 起動 |
| 5 | wasm | `cmd/harness-webui-wasm/main.go` の main() で `js.Global().Set("harness", ...)` し、`select{}` で常駐 |
| 6 | main.js | xterm を初期化、`window.harness.connect(serverCidStr)` を呼ぶ |
| 7 | wasm | `objproto.ParseConnectionID` → `cli.Dial(ctx, peerCID)` |
| 8 | wasm | `transport.WebSocketClientEndpoint(WebSocketConfig{Path: cli.WebSocketPath})` |
| 9 | wasm | `syscall/js` 経由で `new WebSocket("ws://127.0.0.1:8539/ws")` |
| 10 | harness-server | `/ws` の accept handler が connection を受ける |
| 11 | 両端 | objproto handshake (ECDH P521 + AES128GCM) → peer.Conn 確立 |
| 12 | wasm | `cli.Client.Start` で受信ループ開始、main.js に Promise resolve |
| 13 | UI | "connected" 状態に遷移 |

### Flow 2: submit / list / cancel / watch / prune（通信のみ）

| # | 内容 |
|---|---|
| 1 | DOM cmdline で `submit --repo /home/u/proj --task '...'` 入力 |
| 2 | main.js → `window.harness.submit({repo, task})` 呼び出し |
| 3 | wasm → `cli.Submit(ctx, peerCID, repo, task)` |
| 4 | wasm → server に `TaskControlKind_Submit` リクエスト round-trip |
| 5 | wasm が taskID を取り JS Promise resolve、main.js が DOM 更新 |

`list`、`cancel`、`prune` も同じ round-trip。`watch` は cli.Client の subscribe (server push) を wasm 内 goroutine で受け、`js.Global().Call("harness_onTaskEvent", evt)` で main.js に通知。

### Flow 3: interactive PTY

#### 3a. interactive 開始

| 段階 | 主体 | 操作 |
|---|---|---|
| 1 | User | "interactive" ボタンをクリック |
| 2 | main.js | `window.harness.startInteractive(taskID)` |
| 3 | wasm | `cli.Interactive(ctx, peerCID, repo)` |
| 4 | wasm → server | `OpenInteractiveRequest` 送信 |
| 5 | server → runner | `OpenExecRunnerRequest` 転送 |
| 6 | runner | `exec.ExecuteCommand` で claude を PTY 起動 |
| 7 | runner → server → wasm | `TaskAccepted` → `TaskStarted` (worktree dir 含む) |
| 8 | wasm | bidirectional stream を 2 goroutine で wire（recv / send） |

#### 3b. キー入力経路（ブラウザ → claude）

```
xterm.onData("echo hi\r")
  -> main.js callback
  -> wasm send goroutine: stream.Write([]byte)
  -> trsf frame
  -> harness-server (forward)
  -> runner
  -> PTY stdin
  -> claude
```

#### 3c. 出力経路（claude → ブラウザ）

```
claude
  -> PTY stdout
  -> runner
  -> trsf frame
  -> harness-server (forward)
  -> wasm recv goroutine: stream.Read([]byte)
  -> js.Global().Call("harness_xtermWrite", uint8Array)
  -> main.js: xterm.write(bytes)
  -> DOM 更新
```

#### 3d. 終了

| 段階 | 主体 | 操作 |
|---|---|---|
| 1 | User | "close" ボタン or タブ閉じ |
| 2 | main.js | `window.harness.detachInteractive()` |
| 3 | wasm | `stream.CloseBoth()` |
| 4 | server → runner | stream EOF |
| 5 | runner | `exec` の SIGHUP → SIGTERM → SIGKILL ladder で claude を reap |
| 6 | runner → server → wasm | `TaskFinished` |
| 7 | runner | worktree 削除（既存挙動） |

### 境界と不変条件

- **WS frame と trsf packet は 1:1**（trsf 側で MTU chunk した後の 1 packet = 1 WS frame）。wasm 側 transport は pass-through。
- **xterm の `onData` は raw escape sequence 文字列**：wasm 側は解釈せず stream.Write へそのまま流す。ターミナルセマンティクスは claude PTY が処理。
- **xterm のサイズ変更 (ResizeObserver)**：cols/rows を `window.harness.resizeInteractive(cols, rows)` で wasm に渡し、wasm が runner に PTY winsize イベントを送る。経路は既存の framing の仕組みをそのまま使う。`runner/protocol` 側の追加実装は不要。
- **wasm 側で接続切れ (reload / server 落ち)**：wasm runtime ごと消えるので wasm 側 cleanup 不要。harness-server 側はアイドル GC で接続を回収。

---

## Error handling

### E1: WebSocket 接続失敗（initial dial）

`WebSocket.onerror` → Go 側 error 伝播 → `cli.Dial` が error 返す → JS Promise reject → main.js が DOM 上に "failed to connect: <reason>" を表示。**自動リトライしない**（ユーザがリロードで再試行）。

### E2: handshake 失敗（プロトコルバージョン不一致等）

`objproto.DoECDHHandshake` が error → E1 と同じ経路でユーザに見える。wasm がブラウザにキャッシュされて古いままの問題は将来検討（spec out of scope）。

### E3: 接続中断（mid-session disconnect）

`WebSocket.onclose` → 上位に "disconnected" 通知 → DOM が disconnected 状態に遷移、すべての操作を無効化。再接続はユーザがリロード（wasm runtime ごと再起動）。

### E4: interactive PTY 中の片方向エラー

| 発生条件 | 対応 |
|---|---|
| recv goroutine: stream EOF（runner 側で claude exit） | wasm が `TaskFinished` を受信 → main.js に "session ended (exit code N)" 通知。xterm はログ確認用に残す |
| send goroutine: stream.Write エラー（接続断） | E3 と合流 |
| xterm onData callback が wasm 終了後に発火 | js.FuncOf の handler 内で release 済みフラグをチェックし no-op |

### E5: server 側 WS handler 起動エラー

| 発生条件 | 対応 |
|---|---|
| `mux.Handle("/ws", ...)` の path 二重登録 | `http.ServeMux.Handle` が panic → harness-server 起動時即死 |
| `--listen` ポート bind 失敗 | `http.Server.ListenAndServe` が error → `slog.Error` + `os.Exit(1)` |
| embed.FS が空（webui/ ビルド忘れ） | 起動時に `fs.ReadFile("webui/main.wasm")` で確認、err なら fatal |

### E6: wasm ビルド時の互換性破壊

将来 cli パッケージに `os/exec` 等 native 専用 import が紛れ込む → CI / pre-commit の `GOOS=js GOARCH=wasm go build` で検知。Phase 2 plan で `Makefile` に `wasm-check` ターゲットを追加。

### E7: wasm runtime panic

デフォルトでブラウザコンソールに stack trace、wasm runtime 停止。復旧経路はリロードのみ。dogfood では console 任せで十分。

### 全体方針

- **silent failure を許さない** (E5 の起動時アサート、二重登録 panic)。
- **自動リトライしない**（観測可能性優先、リロード or 再起動でユーザが認知）。
- **wasm 専用フォールバック経路は作らない**（WebSocket dial 失敗時 long polling 等は不要）。

---

## Testing strategy

### Phase 1 (transport refactor)

| 種類 | 内容 |
|---|---|
| 既存ユニットテスト全通過 | `go test ./...` で全パッケージ ok |
| 既存 integration テスト | `go test -tags integration ./integration/...` |
| 新規ユニットテスト | **追加しない**（関数分割 + 引数構造体化が中心、振る舞い保存） |
| 手元 smoke | `harness-server` + `agent-runner` + `harness-cli` で submit / interactive 動作確認 |

### Phase 2 (wasm 本体)

| 種類 | 内容 |
|---|---|
| **wasm ビルドチェック (自動)** | `GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm` がエラーなしで通る |
| **既存 native テスト全通過** | `go test ./...` を継続 |
| **wasm 用ユニットテスト** | **追加しない**（headless browser + wasm test runner は overkill） |
| **手動 smoke (ブラウザ)** | 後述 |

### Phase 2 手動 smoke 手順

1. `make webui-build` で `webui/main.wasm` 生成
2. `make build` で `harness-server` / `agent-runner` を native ビルド
3. `harness-server --listen=127.0.0.1:8539` 起動
4. `agent-runner --server-cid='ws:127.0.0.1:8539-*' --repo /tmp/test-repo --claude-bin claude` 起動
5. ブラウザで `http://127.0.0.1:8539/` を開く
6. 接続確認: 画面上に runner が一覧表示される
7. submit: cmdline で `submit --task='echo hello'` → task が enqueue される
8. list: タスクが Queued → Running → Succeeded と遷移する
9. interactive: `interactive --repo=/tmp/test-repo` を発行 → xterm が開いて claude プロンプトが見える
10. キー入力 / 出力が遅延なく流れる
11. ブラウザのタブを閉じる → harness-server ログで接続切れが通常通り処理される
12. リロード → connection 再確立できる

### CI / pre-commit 追加

| ターゲット | 内容 |
|---|---|
| `make wasm-check` (新規) | `GOOS=js GOARCH=wasm go build` を回す |
| `go vet ./...` | 既存通り。`exec/frame/frame.go:248,267` の unreachable 警告は本リファクタと無関係なので無視 |
| `gofmt -l` | 既存通り |

### 明示的に作らないもの

- headless browser 自動 e2e
- wasm 性能ベンチ
- TLS (`wss://`) 経路の自動テスト

---

## Out of scope

- 認証 / 認可（toy-scope 継承、上位層責務）
- LAN / 公開インターネット運用（loopback dogfood 前提）
- 複数 runner 横断のリソース管理（既存挙動踏襲）
- bubbletea TUI のブラウザ移植（DOM 側はゼロから書く、tui パッケージは流用しない）
- wasm 側の build version チェック (E2)
- wasm 側 `--ws-path` override（ビルド時固定 `/ws`）
- harness-server の dual-stack listen (`[::]` 経由 IPv4/IPv6 同時 listen)：別途検討、本 spec では loopback IPv4 固定運用に閉じる

---

## Risks / Notes

- **R1**: wasm バイナリサイズ（standard Go で 10-20MB）。dogfood ロード時間として許容するが、tinygo 化は当面検討しない（互換性検証コストが高い）。
- **R2**: `transport/websocket_wasm.go` の syscall/js 実装は新規ゼロから書く。ECDH handshake 中のフレーム順序が想定通りに流れるかを smoke で要確認。
- **R3**: dualstack の caller-zero 状態は将来「使い始めたとき bit-rot している」可能性。godoc に "intentionally maintained as a template; if rotted, prefer fixing over deleting" を明示する。dogfood は単独利用なので fixing 責任は利用者本人（リポジトリ author）で、godoc にこの方針を残しておけば十分。
- **R4**: `cli.WebSocketPath` の package-level var は global state。dogfood で並行プロセス問題は起きない前提だが、テストで隔離が要るシナリオが将来出たら `cli.ServerConfig` 構造体への昇格を検討。
- **R5**: wasm 側で xterm の input → wasm → 送信、出力 → wasm → xterm の往復が main thread 1 本で走る。長大な出力で UI freeze が起きたら Web Worker 化（`Q3b` の B 案）を検討。本 spec 範囲では native と同等に試して様子を見る。

---

## Migration

1. Phase 1 を独立した plan として切り出し、完了まで Phase 2 に進まない（既存機能の動作維持を優先）。
2. Phase 1 完了後、`harness-server` / `cli` / `runner` / `tui` の主要動線を手元 smoke で確認。
3. Phase 2 の plan を別ファイルとして起こす。Phase 2 内部はさらに「transport_wasm」「cli/interactive_wasm」「webui/static + harness-webui-wasm」「harness-server embed.FS 配信」等のタスクに分割可能。
4. ユーザーは個人 dogfood で利用者は自分一人。対外的な breaking change 通知は不要。
