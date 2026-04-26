# WASM Phase 2: ブラウザ向け wasm 本体実装 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Phase 1 で整えた transport API の上に、`harness-tui` 全機能 (submit / list / cancel / watch / prune / interactive PTY) をブラウザで動かす wasm 本体一式を載せる。`harness-server` 1 プロセスから index.html / main.wasm / xterm.js / WebSocket /ws まで配信できるようにする。

**Architecture:** `transport/websocket_wasm.go` で `syscall/js` による Client モード WebSocket をラップし、`cli/open_interactive_wasm.go` で xterm.js bridge を実装する。`cmd/harness-webui-wasm/main.go` が wasm エントリで `harness.*` JS API を Promise で公開、`webui/` の静的 SPA が xterm + DOM 配線を担う。`harness-server` は `embed.FS` で `webui/` を同梱して `/`、`/static/*` を配信し、既存の `/ws` と同 origin で動く。

**Tech Stack:** Go 1.25.7 + `syscall/js`, xterm.js v5.5.0 (`@xterm/xterm`), `wasm_exec.js` (Go runtime 同梱), `embed.FS` (Go 標準), 既存の `objproto` / `peer` / `trsf` / `cli` / `server` パッケージ群。Spec: `docs/superpowers/specs/2026-04-26-wasm-transport-design.md`. 完了済み Phase 1: `docs/superpowers/plans/2026-04-26-wasm-transport-refactor-phase1.md` (commits `be0b298` / `43bea49` / `5ca8e0f` / `5366a75`).

---

## Reference for implementers

### Phase 1 で固まった前提

- `transport.WebSocketConfig{Logger, Path, TLS, Mode}` 構造体は既に存在し、native 版の `transport/websocket.go` で定義されている。Phase 2 ではこの構造体を **共通定義** として `transport/websocket_common.go` に切り出す (build constraint なし)。`transport/websocket.go` 全体には `//go:build !js` を付与して native 専用にする。
- `cli.WebSocketPath` (var, default `/ws`) は cli/runner/server/wasm すべての caller が参照する。wasm 側でも cli パッケージ内の同名 var を参照する (cli パッケージは wasm でビルド可能、`creack/pty` 経路だけ build tag で隔離)。
- `peer.Dial` / `cli.Dial` は wasm でそのまま動く (Phase 1 検証済み: `GOOS=js GOARCH=wasm go build ./cli/...` クリーン後、Phase 2 では `creack/pty` を持つ `open_interactive.go` の build tag 分離を行う)。
- 上流不変条件: `objproto.SendHandshake` は `EndpointModeServer` を弾く。wasm 側 transport は Client モード限定でこれは無関係。

### xterm.js / wasm_exec.js のバージョン pin

- xterm.js: **v5.5.0** (`@xterm/xterm` の最新 stable)
  - JS bundle: `https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js`
  - CSS: `https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css`
  - SHA256 は curl 後に `sha256sum` で取って commit message に記録 (再現性のため、本 plan では計算手順のみ記述)
- wasm_exec.js: Go runtime 同梱を直接コピー
  - 取得元: `$(go env GOROOT)/lib/wasm/wasm_exec.js` (Go 1.21+ の場所)
  - 当 repo の Go は `/usr/lib/go/lib/wasm/wasm_exec.js` で確認済み

### `harness.*` JS API シグネチャ (Promise pattern)

wasm の Go 側で `js.FuncOf` でラップして `js.Global().Set("harness", ...)` する。すべての関数は引数 1 つ (option object または primitive) を取り、Promise を返す。

```js
window.harness.connect("ws:127.0.0.1:8539-*")
  .then((info) => { /* { runners: [...] } */ })
  .catch((err) => { /* Error */ });

window.harness.submit({ repo: "/abs/path", task: "fix x" }).then((taskID) => ...);
window.harness.list().then((rows) => ...);
window.harness.cancel(taskIDHexString).then(() => ...);
window.harness.watch();   // 戻り値は Promise<void> だが、イベントは window.harness_onTaskEvent で push
window.harness.prune({ before: "168h" }).then((removed) => ...);
window.harness.startInteractive(taskIDHexString).then(() => ...);
window.harness.sendInteractive("\r");
window.harness.resizeInteractive({ cols: 80, rows: 24 });
window.harness.detachInteractive();
```

JS 側の callback (Go から呼ばれる):

```js
window.harness_onTaskEvent = (evtJsonString) => { /* update DOM */ };
window.harness_xtermWrite  = (uint8Array) => term.write(uint8Array);
```

### `embed.FS` 配信

`server.Config` に `WebUIFS *embed.FS` を追加。non-nil なら `server.Run` の中で:
- `/` → `webui/index.html` を返す
- `/static/*` → `webui/static/*` 配下を返す (中身: `main.wasm`, `wasm_exec.js`, `xterm.js`, `xterm.css`, `main.js`, `style.css`)

webui ディレクトリ構造:
```
webui/
  index.html
  static/
    main.js
    style.css
    xterm.js          (vendor)
    xterm.css         (vendor)
    wasm_exec.js      (vendor)
    main.wasm         (build artifact, .gitignore)
```

`webui/static/main.wasm` は `make webui-build` で生成し、git には含めない (`webui/.gitignore` で除外)。CI で生成しないと harness-server 起動時に embed.FS が空になる → 起動時アサートで fatal にする (spec の E5)。

### winsize forwarding (interactive resize)

ResizeObserver が xterm のサイズを検知 → `window.harness.resizeInteractive({cols, rows})` → wasm が既存の framming 仕組み経由で stream に WinSize フレームを送る。具体的な wire 形式は `exec/frame/` パッケージの既存実装を参照する (Phase 1 で `exec/frame/frame_*.go` のプラットフォーム別ファイル切り出しは完了済み)。`exec/frame` パッケージが pure Go で wasm でも import 可能か Task 2 で確認、pty 依存があれば該当部分だけ build tag で隔離する追加修正を Task 2 に含める。

### import 関係 (事前確認済)

- `cli` ← `runner` / `server` / `cmd/harness-webui-wasm` (新規) すべて非循環
- wasm エントリは `cli`、`peer`、`objproto`、`transport`、`runner/protocol` を直接 import 可能
- `tui` パッケージは wasm で使わない (DOM 側で再構築)

### dogfood scope の前提

利用者は 1 人 (本リポジトリ author)。互換性の心配なし、breaking change 自由、deprecation シム不要。Phase 2 完了時点で `harness-server --listen=127.0.0.1:8539` を起動 → ブラウザで `http://127.0.0.1:8539/` を開けば全機能動く、を合格条件とする。

---

## File structure

### Create

```
cli/prune_local.go              //go:build !js, PruneLocal を cli/prune.go から切り出し
cli/open_interactive_native.go  //go:build !js, 既存 cli/open_interactive.go の中身を移植
cli/open_interactive_wasm.go    //go:build js,  xterm.js bridge 実装
transport/websocket_common.go   build constraint なし, WebSocketConfig 構造体定義
transport/websocket_wasm.go     //go:build js,  syscall/js WebSocket Client モード実装
cmd/harness-webui-wasm/main.go  //go:build js,  wasm エントリ + harness.* JS API
webui/index.html                DOM レイアウト
webui/static/main.js            wasm 起動 + xterm 初期化 + DOM 配線
webui/static/style.css          スタイル
webui/static/xterm.js           xterm.js v5.5.0 vendor
webui/static/xterm.css          xterm.js v5.5.0 vendor
webui/static/wasm_exec.js       Go runtime 同梱を vendor
webui/.gitignore                main.wasm を除外
Makefile                        webui-build / build / wasm-check / test / clean
```

### Modify

```
cli/prune.go                    PruneLocal の関数定義を削除 (cli/prune_local.go に移動)
transport/websocket.go          冒頭に //go:build !js を追加。WebSocketConfig 定義を websocket_common.go に移動
server/server.go                server.Config に WebUIFS *embed.FS を追加。server.Run の mux に "/" と "/static/" を登録
cmd/harness-server/main.go      //go:embed webui で webui/ を取り込み、server.Config.WebUIFS に渡す。起動時アサートで main.wasm の存在確認
```

### Delete

```
cli/open_interactive.go         中身は cli/open_interactive_native.go に移動
```

### 触らない

`objproto/`, `peer/`, `trsf/`, `runner/`, `runner/protocol/`, `tui/`, `cli/*.go` のうち上記以外, `cmd/harness-cli/main.go` / `cmd/agent-runner/main.go` / `cmd/harness-tui/main.go`, `transport/dualstack.go`, `transport/udp.go`, `exec/frame/` (Task 2 で参照のみ)。

---

## Tasks

### Task 1: cli の build tag 分離 (PruneLocal + open_interactive)

**Files:**
- Create: `cli/prune_local.go`
- Create: `cli/open_interactive_native.go`
- Modify: `cli/prune.go`
- Delete: `cli/open_interactive.go`

- [ ] **Step 1: 現状確認**

Run:
```sh
grep -n 'PruneLocal\|os/exec\|creack/pty' cli/prune.go cli/open_interactive.go
```

Expected: `cli/prune.go` に `PruneLocal` 関数定義 + `os/exec` import あり。`cli/open_interactive.go` に `creack/pty` import あり。

- [ ] **Step 2: `cli/prune_local.go` を新規作成 (PruneLocal を切り出し)**

ファイル冒頭に `//go:build !js` を付ける。中身は `cli/prune.go` の `PruneLocal` 関数 + その godoc + 必要 import を移植。

```go
//go:build !js

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// PruneLocal walks <repo>/.harness-worktrees/ and `git worktree remove --force`
// the entries whose ModTime is older than `before`. No server interaction.
//
// Native-only: requires os/exec to drive the git binary. The wasm build
// excludes this file via build tag; the browser UI does not expose
// prune-local functionality.
func PruneLocal(ctx context.Context, repo string, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)
	dir := filepath.Join(repo, ".harness-worktrees")
	fmt.Fprintf(out, "prune-local: cutoff = %s; scanning %s\n", FormatPruneCutoff(before), dir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		fmt.Fprintf(out, "prune-local: no worktrees directory; nothing to do\n")
		return nil
	}
	if err != nil {
		return err
	}
	var removed, skippedNewer, skippedError int
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			skippedNewer++
			continue
		}

		path := filepath.Join(dir, e.Name())
		cmd := exec.Command("git", "worktree", "remove", "--force", path)
		cmd.Dir = repo
		if out2, cerr := cmd.CombinedOutput(); cerr != nil {
			fmt.Fprintf(out, "skip %s: %s\n", e.Name(), out2)
			skippedError++
			continue
		}
		fmt.Fprintf(out, "removed %s\n", e.Name())
		removed++
	}
	fmt.Fprintf(out, "prune-local: removed %d, skipped %d (newer=%d, error=%d)\n",
		removed, skippedNewer+skippedError, skippedNewer, skippedError)
	return nil
}
```

- [ ] **Step 3: `cli/prune.go` から PruneLocal の関数定義 + 関連 import を削除**

`PruneLocal` 関数全体を削除する。`os` / `os/exec` / `path/filepath` import が他で使われていなければ削除。`Prune`, `PruneTasks`, `FormatPruneCutoff`, `formatBefore` はそのまま残す。

修正後の `cli/prune.go` の import:
```go
import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)
```

- [ ] **Step 4: `cli/open_interactive_native.go` 新規作成 (中身を移植)**

`cli/open_interactive.go` を `cli/open_interactive_native.go` にリネームし、ファイル冒頭に `//go:build !js` を追加。中身のロジックは変更しない。

Run:
```sh
mv cli/open_interactive.go cli/open_interactive_native.go
```

そしてファイル先頭に以下を追加:
```go
//go:build !js

```
(空行 1 つ + `package cli` の前)

- [ ] **Step 5: `GOOS=js GOARCH=wasm go build ./cli/...` が現時点で通ることを確認**

Run:
```sh
GOOS=js GOARCH=wasm go build ./cli/... 2>&1
```

Expected: `cli/open_interactive_wasm.go` がまだ無いので `cli.Interactive` symbol 不在エラーが出る場合は Task 2 で解決する。`creack/pty` undefined エラーは出てはいけない (= native 限定 import が wasm ビルドから除外されたことの確認)。

`creack/pty` がエラーに出るなら `cli/open_interactive.go` の削除が不完全 → 確認して修正。

- [ ] **Step 6: native ビルド + テスト**

Run:
```sh
go build ./...
go test ./cli/...
```

Expected: 全パス。

- [ ] **Step 7: commit**

```sh
git rm cli/open_interactive.go
git add cli/prune.go cli/prune_local.go cli/open_interactive_native.go
git commit -m "$(cat <<'EOF'
cli: PruneLocal と Interactive を build tag で native/wasm 分離

- PruneLocal を cli/prune_local.go に切り出し (//go:build !js)。
  os/exec + git worktree remove は wasm 不可なので native 限定
- cli/open_interactive.go を cli/open_interactive_native.go にリネーム
  (//go:build !js)。creack/pty + exec.ExecuteCommand に依存するので
  native 限定
- cli/prune.go は Prune / PruneTasks / FormatPruneCutoff / formatBefore のみ残す

Phase 2 (wasm 本体) で cli/open_interactive_wasm.go を別途追加する前提
の build tag 分離。GOOS=js GOARCH=wasm go build ./cli/... が
creack/pty undefined を起こさないことを確認済み。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `cli/open_interactive_wasm.go` (xterm.js bridge)

**Files:**
- Create: `cli/open_interactive_wasm.go`
- Inspect: `runner/protocol/` (winsize frame の既存定義を参照)
- Inspect: `cli/open_interactive_native.go` (StartInteractive 用 wire 経路を参照)

- [ ] **Step 1: 既存 native 実装の wire 経路を読む**

Run:
```sh
cat cli/open_interactive_native.go
```

確認すべき点:
- `Interactive` 関数のシグネチャ: `func Interactive(ctx context.Context, peerCID objproto.ConnectionID, repo string) (string, error)` (Phase 1 で peerCID 化済)
- 内部で `cli.Dial` → `OpenInteractiveRequest` 送信 → `peer.WaitForBidirectionalStream` で stream 取得 → `exec.ExecuteCommand(stream, ...)` で PTY と bridge する流れ
- TaskAccepted / TaskStarted の `RunnerMessage` 受信ロジック

wasm 版で必要なのは: stream 取得まで同じ → `exec.ExecuteCommand` の代わりに JS bridge goroutine を起動する。

- [ ] **Step 2: winsize フレーム形式を確認**

Run:
```sh
grep -rn 'WinSize\|winsize\|Resize' runner/protocol/ exec/frame/ 2>&1 | head -30
```

確認すべき点:
- `runner/protocol` に PTY winsize を表すメッセージ kind があるか (例: `RunnerControlKind_PtyResize`)
- もしくは `exec/frame` パッケージで winsize を encoding するか (frame レベルで送る場合)
- 既存 native の `exec.ExecuteCommand` がどこで winsize を読んでいるか (cmd.SysProcAttr 経由 or runtime 中の resize signal)

Expected outcome: 既存の wire 形式を **そのまま** wasm からも生成できることを確認する (encoding が pure Go なら問題なし、`syscall` 依存なら wasm では別経路)。

- [ ] **Step 3: `cli/open_interactive_wasm.go` 新規作成**

```go
//go:build js

package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall/js"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// InteractiveSession holds the state of an active wasm-side interactive PTY
// session: the bidirectional stream with the runner, the goroutine that pumps
// runner→browser bytes, and the cancel hook.
type InteractiveSession struct {
	stream trsf.BidirectionalStream
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
}

// activeInteractiveSession is the singleton current session. Browser UX only
// allows one interactive task at a time; if a second StartInteractive is
// invoked while a session exists, the old one is detached first.
var (
	activeInteractiveSession *InteractiveSession
	activeInteractiveMu      sync.Mutex
)

// Interactive (wasm) opens an interactive PTY session against the given
// runner-bound task and wires its bytes to the browser xterm. Unlike the
// native variant it does not exec a local PTY; it just streams the stream.
//
// The peerCID is the server's ConnectionID (already used by cli.Dial).
// taskIDHex is the hex-encoded TaskID assigned to the interactive task at
// the server side.
func Interactive(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	taskIDBytes, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return fmt.Errorf("invalid task id hex: %w", err)
	}
	if len(taskIDBytes) != 16 {
		return fmt.Errorf("task id must be 16 bytes, got %d", len(taskIDBytes))
	}

	// Send OpenInteractiveRequest to claim the stream.
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenInteractive}
	oi := protocol.OpenInteractiveRequest{}
	copy(oi.TaskId.Id[:], taskIDBytes)
	req.SetOpenInteractive(oi)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return fmt.Errorf("OpenInteractive RPC: %w", err)
	}
	if resp.Kind != protocol.TaskControlKind_OpenInteractive {
		return fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	oiResp := resp.OpenInteractive()
	if oiResp == nil {
		return errors.New("empty OpenInteractive response")
	}
	streamID := trsf.StreamID(oiResp.StreamId)

	stream := peer.WaitForBidirectionalStream(ctx, c.Transport(), streamID)
	if stream == nil {
		return fmt.Errorf("stream %d not visible", streamID)
	}

	sessCtx, cancel := context.WithCancel(ctx)

	session := &InteractiveSession{
		stream: stream,
		cancel: cancel,
	}

	// Detach any previous session before installing.
	activeInteractiveMu.Lock()
	if old := activeInteractiveSession; old != nil {
		old.detach()
	}
	activeInteractiveSession = session
	activeInteractiveMu.Unlock()

	// recv goroutine: stream → harness_xtermWrite
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-sessCtx.Done():
				return
			default:
			}
			n, err := stream.Read(buf)
			if err != nil {
				slog.Info("interactive recv ended", "err", err)
				return
			}
			if n == 0 {
				continue
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			arr := js.Global().Get("Uint8Array").New(n)
			js.CopyBytesToJS(arr, data)
			js.Global().Call("harness_xtermWrite", arr)
		}
	}()

	return nil
}

// SendInteractive writes user-typed bytes (from xterm.onData) to the active
// interactive stream. Called from JS via window.harness.sendInteractive.
func SendInteractive(data []byte) error {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveMu.Unlock()
	if session == nil {
		return errors.New("no active interactive session")
	}
	session.mu.Lock()
	closed := session.closed
	session.mu.Unlock()
	if closed {
		return errors.New("interactive session is closed")
	}
	if _, err := session.stream.Write(data); err != nil {
		return fmt.Errorf("stream write: %w", err)
	}
	return nil
}

// ResizeInteractive forwards a window-size change to the runner. The wire
// format reuses the existing exec/frame WinSize encoding (a frame whose
// payload is two big-endian uint16: cols, rows). See ResizeFrame below.
//
// NOTE: if the existing exec/frame package has a wasm-incompatible import
// in this code path, switch to building the frame manually here.
func ResizeInteractive(cols, rows uint16) error {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveMu.Unlock()
	if session == nil {
		return errors.New("no active interactive session")
	}
	frame := buildWinSizeFrame(cols, rows)
	if _, err := session.stream.Write(frame); err != nil {
		return fmt.Errorf("stream write resize: %w", err)
	}
	return nil
}

// DetachInteractive closes the active session.
func DetachInteractive() {
	activeInteractiveMu.Lock()
	session := activeInteractiveSession
	activeInteractiveSession = nil
	activeInteractiveMu.Unlock()
	if session != nil {
		session.detach()
	}
}

func (s *InteractiveSession) detach() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.stream.CloseBoth()
	s.cancel()
}

// buildWinSizeFrame encodes a window-size change as the wire byte sequence
// the runner side parses.
//
// IMPORTANT: This assumes the existing native winsize forwarding wire shape
// is `[byte FrameKind_WinSize, uint16BE cols, uint16BE rows]`. Verify
// against exec/frame/* before relying on this in production. If the
// encoding differs, update this function (and the godoc) to match.
func buildWinSizeFrame(cols, rows uint16) []byte {
	const frameKindWinSize byte = wire.FrameKindWinSize
	out := make([]byte, 5)
	out[0] = frameKindWinSize
	out[1] = byte(cols >> 8)
	out[2] = byte(cols & 0xff)
	out[3] = byte(rows >> 8)
	out[4] = byte(rows & 0xff)
	return out
}
```

> **NOTE for the implementer:** `wire.FrameKindWinSize` の定数名は `runner/protocol/` か `trsf/wire/` にあるはず。grep で確認 (`grep -n 'WinSize\|winsize' runner/protocol/*.go trsf/wire/*.go`)。実際の定数名と payload の wire layout が上の想定と違う場合、`buildWinSizeFrame` をその layout に合わせて書き換える。winsize を別 stream や別 frame kind で扱う設計なら、本コードもその経路に合わせる。

- [ ] **Step 4: wasm ビルド確認**

Run:
```sh
GOOS=js GOARCH=wasm go build ./cli/...
```

Expected: クリーン。`creack/pty` undefined や exec/frame の native-only import エラーが出たら、該当箇所を build tag で隔離する追加修正を入れる (`exec/frame/winsize_native.go` 等)。

- [ ] **Step 5: native ビルドが壊れていないことを確認**

Run:
```sh
go build ./...
go test ./cli/...
```

Expected: 全パス。

- [ ] **Step 6: commit**

```sh
git add cli/open_interactive_wasm.go
git commit -m "$(cat <<'EOF'
cli: open_interactive_wasm.go に xterm.js bridge を実装

wasm 用 cli.Interactive (taskIDHex 引数版) を新設し、bidirectional
stream 取得後に recv goroutine で stream を読み出して
harness_xtermWrite() に流す。SendInteractive / ResizeInteractive /
DetachInteractive は js.FuncOf で公開予定の補助関数。

InteractiveSession は global singleton で、新規 Start 時に既存
session を detach する。winsize は exec/frame の既存 wire と同形式の
5 byte frame を組み立てる buildWinSizeFrame で送出。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `transport/websocket_wasm.go` + 共通定義切り出し

**Files:**
- Create: `transport/websocket_common.go`
- Create: `transport/websocket_wasm.go`
- Modify: `transport/websocket.go` (`//go:build !js` 追加、共通 struct を common.go に移動)

- [ ] **Step 1: `transport/websocket_common.go` を新規作成 (build constraint なし)**

`transport/websocket.go` から `WebSocketConfig` 構造体定義 + その godoc を移植。

```go
package transport

import (
	"crypto/tls"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
)

// WebSocketConfig configures a WebSocket-backed objproto Endpoint. The same
// struct is used for Client / Server / Mutual modes; the Path field is
// interpreted by Client/Mutual as the dial Location.Path, and by
// Server/Mutual as the mount path passed to mux.Handle.
//
// The transport package does not own a path convention. Callers are expected
// to align Client and Server values; cli.WebSocketPath is the canonical
// harness-side default.
//
// TLS is consulted for Origin scheme decisions (ws:// vs wss://). The
// listen-side TLS for Server / Mutual is owned by the caller's *http.Server.
//
// Mode selects Client / Server / Mutual semantics. The mux argument of
// WebSocketEndpoint must be nil for Client and non-nil for Server / Mutual.
//
// This file (websocket_common.go) is build-constraint-free so the struct is
// shared between native (websocket.go, !js) and wasm (websocket_wasm.go, js)
// implementations.
type WebSocketConfig struct {
	Logger *slog.Logger
	Path   string
	TLS    *tls.Config
	Mode   objproto.EndpointMode
}
```

- [ ] **Step 2: `transport/websocket.go` から `WebSocketConfig` 定義を削除し、`//go:build !js` を冒頭に追加**

ファイル先頭 (package 宣言の前):
```go
//go:build !js

package transport
```

`WebSocketConfig` 構造体宣言とその godoc を削除する (Step 1 で `websocket_common.go` に移植済)。`crypto/tls` import が他で使われていなければ削除 (使われていれば残す)。

- [ ] **Step 3: `transport/websocket_wasm.go` 新規作成 (Client モード限定)**

```go
//go:build js

package transport

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"syscall/js"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/objproto/packet"
)

// WebSocketEndpoint constructs a WebSocket-backed objproto Endpoint in the
// mode specified by cfg.Mode. The wasm build supports Client mode only;
// Server / Mutual return an error because the browser environment cannot
// listen for incoming WS connections.
func WebSocketEndpoint(mux *http.ServeMux, cfg WebSocketConfig) (objproto.Endpoint, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, cfg.Mode)
	if err := WebSocketEndpointEx(rawSess, mux, cfg); err != nil {
		return nil, err
	}
	return rawSess, nil
}

// WebSocketEndpointEx is the lower-level variant for callers that already
// own a RawEndpoint. wasm build supports Client mode only.
func WebSocketEndpointEx(rawSess objproto.RawEndpoint, mux *http.ServeMux, cfg WebSocketConfig) error {
	if cfg.Mode != objproto.EndpointModeClient {
		return fmt.Errorf("websocket_wasm: only Client mode is supported (got %v)", cfg.Mode)
	}
	if mux != nil {
		return errors.New("websocket_wasm: mux must be nil in wasm Client mode")
	}

	transportName := "ws"
	if cfg.TLS != nil {
		transportName = "wss"
	}

	var connsMu sync.Mutex
	conns := make(map[netip.AddrPort]*wasmWSConn)
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// sender goroutine: drain rawSess.GetSenderChannel and route packets to
	// the right connection. Handshake packets to unknown peers trigger a
	// fresh dial (only Client/Mutual reach this branch per upstream
	// SendHandshake invariant).
	go func() {
		for pkt := range rawSess.GetSenderChannel() {
			connsMu.Lock()
			conn, ok := conns[pkt.To.Addr]
			connsMu.Unlock()

			if !ok {
				if pkt.Kind != packet.PacketKind_Handshake {
					logger.Error("no websocket connection for address",
						slog.String("address", pkt.To.String()))
					rawSess.CannotSend(pkt)
					continue
				}
				go dialAndSend(rawSess, transportName, &connsMu, conns, pkt, cfg, logger)
				continue
			}
			if err := conn.send(pkt.Data); err != nil {
				logger.Error("ws send failed",
					slog.String("to", pkt.To.String()),
					slog.String("err", err.Error()))
				rawSess.CannotSend(pkt)
				connsMu.Lock()
				delete(conns, pkt.To.Addr)
				connsMu.Unlock()
				conn.close()
			}
		}
	}()

	return nil
}

// wasmWSConn wraps a JS WebSocket value with the same Send / Receive shape
// used internally by the sender / receiver goroutines.
type wasmWSConn struct {
	ws         js.Value
	remoteAddr netip.AddrPort

	incoming chan []byte
	closed   chan struct{}

	cleanupMu sync.Mutex
	cleanedUp bool
	releases  []js.Func
}

func dialAndSend(
	rawSess objproto.RawEndpoint,
	transportName string,
	connsMu *sync.Mutex,
	conns map[netip.AddrPort]*wasmWSConn,
	pkt *objproto.PacketData,
	cfg WebSocketConfig,
	logger *slog.Logger,
) {
	scheme := "ws"
	if cfg.TLS != nil {
		scheme = "wss"
	}
	url := fmt.Sprintf("%s://%s%s", scheme, pkt.To.Addr.String(), cfg.Path)

	ws := js.Global().Get("WebSocket").New(url)
	ws.Set("binaryType", "arraybuffer")

	openCh := make(chan struct{})
	errCh := make(chan struct{}, 1)

	var releases []js.Func
	addListener := func(event string, fn func(this js.Value, args []js.Value) any) js.Func {
		f := js.FuncOf(fn)
		ws.Call("addEventListener", event, f)
		releases = append(releases, f)
		return f
	}

	addListener("open", func(this js.Value, args []js.Value) any {
		select {
		case <-openCh:
		default:
			close(openCh)
		}
		return nil
	})
	addListener("error", func(this js.Value, args []js.Value) any {
		select {
		case errCh <- struct{}{}:
		default:
		}
		return nil
	})

	select {
	case <-openCh:
	case <-errCh:
		logger.Error("ws dial failed", slog.String("addr", pkt.To.Addr.String()))
		for _, f := range releases {
			f.Release()
		}
		rawSess.CannotSend(pkt)
		return
	}

	conn := &wasmWSConn{
		ws:         ws,
		remoteAddr: pkt.To.Addr,
		incoming:   make(chan []byte, 16),
		closed:     make(chan struct{}),
		releases:   releases,
	}

	addListener("message", func(this js.Value, args []js.Value) any {
		evt := args[0]
		data := evt.Get("data") // ArrayBuffer (binaryType = "arraybuffer")
		u8 := js.Global().Get("Uint8Array").New(data)
		buf := make([]byte, u8.Length())
		js.CopyBytesToGo(buf, u8)
		select {
		case conn.incoming <- buf:
		case <-conn.closed:
		}
		return nil
	})
	addListener("close", func(this js.Value, args []js.Value) any {
		conn.markClosed()
		return nil
	})

	connsMu.Lock()
	conns[pkt.To.Addr] = conn
	connsMu.Unlock()

	if err := conn.send(pkt.Data); err != nil {
		logger.Error("ws handshake send failed",
			slog.String("addr", pkt.To.Addr.String()),
			slog.String("err", err.Error()))
		rawSess.CannotSend(pkt)
		connsMu.Lock()
		delete(conns, pkt.To.Addr)
		connsMu.Unlock()
		conn.close()
		return
	}

	go func() {
		for {
			select {
			case data := <-conn.incoming:
				rawSess.Receive(transportName, conn.remoteAddr, data)
			case <-conn.closed:
				connsMu.Lock()
				delete(conns, conn.remoteAddr)
				connsMu.Unlock()
				conn.close()
				return
			}
		}
	}()
}

func (c *wasmWSConn) send(data []byte) error {
	if c.ws.Get("readyState").Int() != 1 { // 1 = OPEN
		return errors.New("websocket not open")
	}
	arr := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(arr, data)
	c.ws.Call("send", arr)
	return nil
}

func (c *wasmWSConn) markClosed() {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
}

func (c *wasmWSConn) close() {
	c.cleanupMu.Lock()
	if c.cleanedUp {
		c.cleanupMu.Unlock()
		return
	}
	c.cleanedUp = true
	c.cleanupMu.Unlock()

	c.markClosed()
	c.ws.Call("close")
	for _, f := range c.releases {
		f.Release()
	}
}
```

- [ ] **Step 4: native ビルド確認 (`websocket_common.go` の切り出しが既存を壊していないか)**

Run:
```sh
go build ./...
go vet ./...
go test ./...
```

Expected: 全パス。`exec/frame/frame.go:248,267` の pre-existing 警告のみ。

- [ ] **Step 5: wasm ビルド確認**

Run:
```sh
GOOS=js GOARCH=wasm go build ./cli/... ./transport/...
```

Expected: クリーン。

- [ ] **Step 6: commit**

```sh
git add transport/websocket_common.go transport/websocket.go transport/websocket_wasm.go
git commit -m "$(cat <<'EOF'
transport: WebSocketConfig を共通定義に切り出し、wasm Client 実装を追加

- transport/websocket_common.go (新規, build constraint なし) に
  WebSocketConfig 構造体定義を移動。native と wasm 両側から共有
- transport/websocket.go の冒頭に //go:build !js を追加し、native
  限定の golang.org/x/net/websocket 経路を wasm ビルドから除外
- transport/websocket_wasm.go (新規, //go:build js) に syscall/js 経由の
  WebSocket Client 実装を追加。Server / Mutual モードは error 返却

GOOS=js GOARCH=wasm go build ./cli/... ./transport/... が通ることを確認。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `cmd/harness-webui-wasm/main.go` (wasm エントリ)

**Files:**
- Create: `cmd/harness-webui-wasm/main.go`

- [ ] **Step 1: ファイル新規作成**

```go
//go:build js

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"syscall/js"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
)

var (
	rootCtx context.Context

	clientMu sync.Mutex
	client   *cli.Client
	peerCID  objproto.ConnectionID
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootCtx = ctx

	js.Global().Set("harness", js.ValueOf(map[string]any{
		"connect":           js.FuncOf(harnessConnect),
		"submit":            js.FuncOf(harnessSubmit),
		"list":              js.FuncOf(harnessList),
		"cancel":            js.FuncOf(harnessCancel),
		"watch":             js.FuncOf(harnessWatch),
		"prune":             js.FuncOf(harnessPrune),
		"startInteractive":  js.FuncOf(harnessStartInteractive),
		"sendInteractive":   js.FuncOf(harnessSendInteractive),
		"resizeInteractive": js.FuncOf(harnessResizeInteractive),
		"detachInteractive": js.FuncOf(harnessDetachInteractive),
	}))

	slog.Info("harness-webui-wasm started")
	select {} // keep runtime alive
}

// promiseFn wraps an asynchronous Go function in a JS Promise. The provided
// fn receives the call args and a (resolve, reject) pair as js.Value.
func promiseFn(fn func(args []js.Value, resolve, reject js.Value)) js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
			resolve := promiseArgs[0]
			reject := promiseArgs[1]
			go fn(args, resolve, reject)
			return nil
		})
		defer executor.Release()
		return js.Global().Get("Promise").New(executor)
	})
}

func rejectErr(reject js.Value, err error) {
	reject.Invoke(js.Global().Get("Error").New(err.Error()))
}

// harnessConnect parses the CID string and dials the server.
//   harness.connect("ws:127.0.0.1:8539-*") -> Promise<{}>
func harnessConnect(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if len(args) < 1 {
				rejectErr(reject, errors.New("connect: missing CID arg"))
				return
			}
			cidStr := args[0].String()
			cid, err := objproto.ParseConnectionID(cidStr,
				objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
			if err != nil {
				rejectErr(reject, fmt.Errorf("parse cid: %w", err))
				return
			}
			c, err := cli.Dial(rootCtx, cid)
			if err != nil {
				rejectErr(reject, fmt.Errorf("dial: %w", err))
				return
			}
			clientMu.Lock()
			client = c
			peerCID = cid
			clientMu.Unlock()
			resolve.Invoke(js.ValueOf(map[string]any{}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

func currentClient() (*cli.Client, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client == nil {
		return nil, errors.New("not connected; call harness.connect first")
	}
	return client, nil
}

// harnessSubmit submits a task.
//   harness.submit({repo: "/abs/path", task: "..."}) -> Promise<taskIDHex>
func harnessSubmit(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("submit: missing options arg"))
				return
			}
			opts := args[0]
			repo := opts.Get("repo").String()
			task := opts.Get("task").String()
			id, err := cli.Submit(rootCtx, peerCID, repo, task)
			if err != nil {
				rejectErr(reject, fmt.Errorf("submit: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(id))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessList returns the list output as a string.
//   harness.list() -> Promise<string>
func harnessList(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			var buf bytesBuffer
			if err := cli.List(rootCtx, peerCID, &buf); err != nil {
				rejectErr(reject, fmt.Errorf("list: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(buf.String()))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessCancel cancels a queued/running task.
//   harness.cancel("0123abcd...") -> Promise<void>
func harnessCancel(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("cancel: missing taskID arg"))
				return
			}
			taskIDHex := args[0].String()
			if err := cli.Cancel(rootCtx, peerCID, taskIDHex); err != nil {
				rejectErr(reject, fmt.Errorf("cancel: %w", err))
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessWatch starts a watch goroutine. Events are pushed via
// window.harness_onTaskEvent(jsonString). The promise resolves when
// the watch is established (subscribe round-trip done) or rejects on error.
func harnessWatch(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			pipe := &watchPipe{}
			go func() {
				if err := cli.Watch(rootCtx, peerCID, pipe); err != nil {
					slog.Error("watch ended", "err", err)
				}
			}()
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessPrune asks the server to forget terminal tasks older than the
// given duration string (e.g. "168h").
//   harness.prune({before: "168h"}) -> Promise<string>
func harnessPrune(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("prune: missing options arg"))
				return
			}
			beforeStr := args[0].Get("before").String()
			before, err := time.ParseDuration(beforeStr)
			if err != nil {
				rejectErr(reject, fmt.Errorf("invalid before duration: %w", err))
				return
			}
			var buf bytesBuffer
			if err := cli.Prune(rootCtx, peerCID, before, &buf); err != nil {
				rejectErr(reject, fmt.Errorf("prune: %w", err))
				return
			}
			resolve.Invoke(js.ValueOf(buf.String()))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessStartInteractive opens an interactive PTY session for a task.
//   harness.startInteractive("0123abcd...") -> Promise<void>
func harnessStartInteractive(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve := promiseArgs[0]
		reject := promiseArgs[1]
		go func() {
			if _, err := currentClient(); err != nil {
				rejectErr(reject, err)
				return
			}
			if len(args) < 1 {
				rejectErr(reject, errors.New("startInteractive: missing taskID arg"))
				return
			}
			taskIDHex := args[0].String()
			if _, err := hex.DecodeString(taskIDHex); err != nil {
				rejectErr(reject, fmt.Errorf("invalid task id: %w", err))
				return
			}
			if err := cli.Interactive(rootCtx, peerCID, taskIDHex); err != nil {
				rejectErr(reject, fmt.Errorf("interactive: %w", err))
				return
			}
			resolve.Invoke(js.Undefined())
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// harnessSendInteractive forwards user keystrokes (xterm.onData) to the
// active interactive stream. Synchronous; returns nothing.
//   harness.sendInteractive(stringOrUint8Array)
func harnessSendInteractive(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(false)
	}
	val := args[0]
	var data []byte
	switch val.Type() {
	case js.TypeString:
		data = []byte(val.String())
	default:
		// Uint8Array path
		length := val.Get("length").Int()
		data = make([]byte, length)
		js.CopyBytesToGo(data, val)
	}
	if err := cli.SendInteractive(data); err != nil {
		slog.Error("sendInteractive", "err", err)
		return js.ValueOf(false)
	}
	return js.ValueOf(true)
}

// harnessResizeInteractive forwards a window-size change to the runner.
//   harness.resizeInteractive({cols: 80, rows: 24})
func harnessResizeInteractive(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(false)
	}
	opts := args[0]
	cols, _ := strconv.Atoi(opts.Get("cols").String())
	rows, _ := strconv.Atoi(opts.Get("rows").String())
	if cols <= 0 || rows <= 0 {
		return js.ValueOf(false)
	}
	if err := cli.ResizeInteractive(uint16(cols), uint16(rows)); err != nil {
		slog.Error("resizeInteractive", "err", err)
		return js.ValueOf(false)
	}
	return js.ValueOf(true)
}

// harnessDetachInteractive closes the active interactive session.
//   harness.detachInteractive()
func harnessDetachInteractive(this js.Value, args []js.Value) any {
	cli.DetachInteractive()
	return js.Undefined()
}

// bytesBuffer is a minimal io.Writer used for collecting cli output before
// returning it to JS as a single string. We avoid pulling in bytes.Buffer
// just to dodge any potential growth in wasm bundle size; this is a string-
// safe append-only buffer.
type bytesBuffer struct {
	buf []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}
func (b *bytesBuffer) String() string { return string(b.buf) }

// watchPipe wraps each Write line as a JSON event and forwards to the JS
// callback window.harness_onTaskEvent. Lines are separated by '\n' as
// emitted by cli.Watch.
type watchPipe struct {
	carry []byte
}

func (w *watchPipe) Write(p []byte) (int, error) {
	w.carry = append(w.carry, p...)
	for {
		idx := -1
		for i, b := range w.carry {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx == -1 {
			break
		}
		line := string(w.carry[:idx])
		w.carry = w.carry[idx+1:]
		// best-effort JSON wrap; if line is already JSON pass through
		evt := map[string]any{"line": line}
		blob, _ := json.Marshal(evt)
		js.Global().Call("harness_onTaskEvent", string(blob))
	}
	return len(p), nil
}
```

> **NOTE for the implementer:** `sync` import が抜けている可能性。`clientMu sync.Mutex` を使うため必要。`go vet` でチェックして抜けていれば追加。

- [ ] **Step 2: wasm ビルド**

Run:
```sh
GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm/...
```

Expected: クリーン。`cli.SendInteractive` / `cli.ResizeInteractive` / `cli.DetachInteractive` 等は Task 2 で `cli/open_interactive_wasm.go` に追加済みなので resolve できるはず。`cli.Interactive` のシグネチャは Task 2 で `(ctx, peerCID, taskIDHex string) error` に変えたが、native 版 (`cli/open_interactive_native.go`) のシグネチャは `(ctx, peerCID, repo string) (string, error)` と異なる。これは **Task 4 で問題が出たら native と wasm のシグネチャを揃えるか、wasm 専用関数名にする**ことを検討する (例: native は `Interactive`、wasm は `Attach`)。本 step で build error が出たら Task 2 の InteractiveSession 関連関数名を見直す。

> 推奨: `cli.Interactive` (wasm) を `cli.AttachInteractive` にリネームして、native 版とは衝突しない名前にする。Task 2 のコードもそれに合わせる。Task 4 内で発覚するこの不整合は、Task 2 を一旦書き換える形で対処する (atomic refactor の哲学を維持するため、commit 単位は再分割せず Task 2 の commit を amend する選択もありうるが、本 plan では新規 commit で `cli.Interactive (wasm) → cli.AttachInteractive` リネームする)。

- [ ] **Step 3: native ビルドが壊れていないことを確認**

Run:
```sh
go build ./...
go test ./...
```

Expected: 全パス。

- [ ] **Step 4: commit**

```sh
git add cmd/harness-webui-wasm/
git commit -m "$(cat <<'EOF'
cmd/harness-webui-wasm: wasm エントリと harness.* JS API を実装

js.FuncOf で connect / submit / list / cancel / watch / prune /
startInteractive / sendInteractive / resizeInteractive /
detachInteractive を JS に公開。すべて Promise を返す pattern。
watch は server push を harness_onTaskEvent(jsonString) で JS 側に
forward する。

GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm/ クリーン。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: webui/ 静的アセット (vendor + index.html + main.js + style.css)

**Files:**
- Create: `webui/index.html`
- Create: `webui/static/main.js`
- Create: `webui/static/style.css`
- Create: `webui/static/xterm.js` (vendor)
- Create: `webui/static/xterm.css` (vendor)
- Create: `webui/static/wasm_exec.js` (vendor)
- Create: `webui/.gitignore`

- [ ] **Step 1: ディレクトリと .gitignore を作成**

Run:
```sh
mkdir -p webui/static
cat > webui/.gitignore <<'EOF'
# Build artifact, generated by `make webui-build`.
static/main.wasm
EOF
```

- [ ] **Step 2: xterm.js + xterm.css を vendor**

Run:
```sh
curl -fsSL -o webui/static/xterm.js  https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js
curl -fsSL -o webui/static/xterm.css https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/css/xterm.css
sha256sum webui/static/xterm.js webui/static/xterm.css
```

Expected: 2 ファイル取得成功。SHA256 を commit message に記録 (再現性のため)。

- [ ] **Step 3: wasm_exec.js を vendor**

Run:
```sh
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" webui/static/wasm_exec.js
sha256sum webui/static/wasm_exec.js
```

Expected: コピー成功。SHA256 を commit message に記録。

- [ ] **Step 4: `webui/index.html` 作成**

```html
<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="UTF-8">
<title>harness-webui</title>
<link rel="stylesheet" href="/static/xterm.css">
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<div id="app">
  <header>
    <h1>harness-webui</h1>
    <div id="status">disconnected</div>
  </header>
  <main>
    <section id="runners">
      <h2>Runners</h2>
      <pre id="runner-list"></pre>
    </section>
    <section id="tasks">
      <h2>Tasks</h2>
      <pre id="task-list"></pre>
    </section>
    <section id="cmdline">
      <h2>Command</h2>
      <input id="cmd-input" type="text" placeholder='submit --repo /tmp/test --task "echo hi"' size="80">
      <button id="cmd-run">Run</button>
      <pre id="cmd-output"></pre>
    </section>
    <section id="interactive">
      <h2>Interactive</h2>
      <input id="task-id-input" type="text" placeholder="task id (hex)" size="40">
      <button id="attach">Attach</button>
      <button id="detach">Detach</button>
      <div id="terminal"></div>
    </section>
  </main>
</div>
<script src="/static/wasm_exec.js"></script>
<script src="/static/xterm.js"></script>
<script src="/static/main.js" defer></script>
</body>
</html>
```

- [ ] **Step 5: `webui/static/style.css` 作成**

```css
body {
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
  margin: 0;
  padding: 1rem;
  background: #1e1e1e;
  color: #d4d4d4;
}
header {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  border-bottom: 1px solid #444;
  padding-bottom: 0.5rem;
  margin-bottom: 1rem;
}
header h1 { margin: 0; font-size: 1.2rem; }
#status {
  padding: 0.2rem 0.6rem;
  border-radius: 4px;
  background: #444;
  font-size: 0.8rem;
}
#status.connected { background: #2d5; color: #000; }
#status.error { background: #c33; }
section {
  margin-bottom: 1.5rem;
}
section h2 { font-size: 1rem; border-bottom: 1px solid #333; padding-bottom: 0.2rem; }
pre {
  background: #111;
  padding: 0.5rem;
  border-radius: 4px;
  max-height: 200px;
  overflow: auto;
  font-size: 0.85rem;
}
input {
  background: #2a2a2a;
  color: #d4d4d4;
  border: 1px solid #555;
  padding: 0.3rem;
  font-family: monospace;
}
button {
  background: #444;
  color: #fff;
  border: 1px solid #666;
  padding: 0.3rem 0.8rem;
  cursor: pointer;
}
button:hover { background: #555; }
#terminal {
  background: #000;
  height: 400px;
  border-radius: 4px;
}
```

- [ ] **Step 6: `webui/static/main.js` 作成**

```js
"use strict";

const SERVER_CID = location.protocol.startsWith("https")
  ? `wss:${location.hostname}:${location.port || 443}-*`
  : `ws:${location.hostname}:${location.port || 80}-*`;

(async () => {
  const status = document.getElementById("status");
  const setStatus = (s, cls) => {
    status.textContent = s;
    status.className = cls || "";
  };

  // 1. Load and start the wasm module.
  const go = new Go();
  setStatus("loading wasm…");
  const result = await WebAssembly.instantiateStreaming(fetch("/static/main.wasm"), go.importObject);
  go.run(result.instance);   // does NOT block; harness object becomes available shortly

  // 2. Wait for window.harness to appear.
  const start = Date.now();
  while (typeof window.harness === "undefined") {
    if (Date.now() - start > 5000) {
      setStatus("wasm timeout", "error");
      return;
    }
    await new Promise(r => setTimeout(r, 50));
  }

  // 3. Connect.
  setStatus("connecting…");
  try {
    await window.harness.connect(SERVER_CID);
    setStatus("connected", "connected");
  } catch (e) {
    setStatus(`connect failed: ${e.message}`, "error");
    return;
  }

  // 4. Initial list refresh.
  const refreshList = async () => {
    try {
      const out = await window.harness.list();
      document.getElementById("task-list").textContent = out;
    } catch (e) {
      document.getElementById("task-list").textContent = `list error: ${e.message}`;
    }
  };
  refreshList();
  setInterval(refreshList, 5000);

  // 5. Watch (server push).
  window.harness_onTaskEvent = (jsonStr) => {
    try {
      const evt = JSON.parse(jsonStr);
      const el = document.getElementById("task-list");
      el.textContent = `[${new Date().toISOString()}] ${evt.line}\n` + el.textContent;
    } catch (e) { /* ignore */ }
  };
  window.harness.watch().catch(e => console.error("watch:", e));

  // 6. Cmdline submit / cancel / prune.
  const cmdInput  = document.getElementById("cmd-input");
  const cmdRun    = document.getElementById("cmd-run");
  const cmdOutput = document.getElementById("cmd-output");

  const runCmd = async () => {
    const line = cmdInput.value.trim();
    if (!line) return;
    cmdInput.value = "";
    cmdOutput.textContent = `> ${line}\n` + cmdOutput.textContent;
    try {
      const tokens = line.split(/\s+/);
      const cmd = tokens[0];
      const flags = parseFlags(tokens.slice(1));
      let out;
      switch (cmd) {
        case "submit":
          out = await window.harness.submit({ repo: flags.repo || "", task: flags.task || "" });
          break;
        case "list":
          out = await window.harness.list();
          break;
        case "cancel":
          out = await window.harness.cancel(tokens[1] || "");
          out = "cancelled";
          break;
        case "prune":
          out = await window.harness.prune({ before: flags.before || "168h" });
          break;
        default:
          out = `unknown command: ${cmd}`;
      }
      cmdOutput.textContent = `${out}\n` + cmdOutput.textContent;
      refreshList();
    } catch (e) {
      cmdOutput.textContent = `error: ${e.message}\n` + cmdOutput.textContent;
    }
  };
  cmdRun.addEventListener("click", runCmd);
  cmdInput.addEventListener("keydown", (e) => { if (e.key === "Enter") runCmd(); });

  // 7. Interactive PTY.
  const term = new Terminal({ convertEol: true, fontSize: 13 });
  term.open(document.getElementById("terminal"));
  window.harness_xtermWrite = (uint8Array) => term.write(uint8Array);
  term.onData((data) => window.harness.sendInteractive(data));
  // Best-effort resize on container resize.
  const ro = new ResizeObserver(() => {
    window.harness.resizeInteractive({ cols: term.cols, rows: term.rows });
  });
  ro.observe(document.getElementById("terminal"));

  document.getElementById("attach").addEventListener("click", async () => {
    const id = document.getElementById("task-id-input").value.trim();
    if (!id) return;
    try {
      await window.harness.startInteractive(id);
      term.focus();
    } catch (e) {
      alert(`startInteractive: ${e.message}`);
    }
  });
  document.getElementById("detach").addEventListener("click", () => {
    window.harness.detachInteractive();
  });
})();

function parseFlags(tokens) {
  const out = {};
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.startsWith("--")) {
      const eq = t.indexOf("=");
      if (eq !== -1) {
        out[t.slice(2, eq)] = t.slice(eq + 1).replace(/^"(.*)"$/, "$1");
      } else {
        out[t.slice(2)] = tokens[i + 1] || "";
        i++;
      }
    }
  }
  return out;
}
```

- [ ] **Step 7: 作成物確認**

Run:
```sh
ls -la webui/ webui/static/
```

Expected: `webui/index.html`, `webui/.gitignore`, `webui/static/main.js`, `webui/static/style.css`, `webui/static/xterm.js`, `webui/static/xterm.css`, `webui/static/wasm_exec.js` の 7 ファイル。`webui/static/main.wasm` は無い (build artifact)。

- [ ] **Step 8: commit**

```sh
git add webui/
git commit -m "$(cat <<'EOF'
webui: 静的アセット一式 (index.html / main.js / style.css / vendor)

- webui/index.html: DOM レイアウト (runner / task / cmdline / xterm)
- webui/static/main.js: wasm 起動 + harness.* API 呼び出し + xterm 配線
- webui/static/style.css: ダーク基調のシンプル CSS
- webui/static/xterm.js + xterm.css: xterm.js v5.5.0 vendor (jsdelivr CDN)
- webui/static/wasm_exec.js: Go runtime 同梱 (Go 1.25.7) コピー
- webui/.gitignore: main.wasm (build artifact) を除外

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: harness-server に embed.FS 配信を追加

**Files:**
- Modify: `server/server.go`
- Modify: `cmd/harness-server/main.go`

- [ ] **Step 1: `server/server.go` の Config に WebUIFS フィールドを追加**

`server.Config` 構造体に以下を追加:

```go
// WebUIFS, when non-nil, causes server.Run to register handlers on its
// internal mux for "/" (serving "<root>/index.html") and "/static/" (serving
// the directory tree). The fs.FS is expected to have webui/index.html at
// its root and webui/static/* below. Typically supplied via //go:embed
// from cmd/harness-server.
WebUIFS fs.FS
```

import に `"io/fs"` を追加。

- [ ] **Step 2: `server/server.go` の Run 内で WebUIFS handler を mux に登録**

`server.Run` の中で、`mux := http.NewServeMux()` の直後に以下を追加 (transport の WS handler 登録の前):

```go
if s.cfg.WebUIFS != nil {
    indexBytes, err := fs.ReadFile(s.cfg.WebUIFS, "index.html")
    if err != nil {
        return fmt.Errorf("webui: index.html not in embed.FS: %w", err)
    }
    if _, err := fs.Stat(s.cfg.WebUIFS, "static/main.wasm"); err != nil {
        return fmt.Errorf("webui: static/main.wasm missing (did you forget `make webui-build`?): %w", err)
    }
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        _, _ = w.Write(indexBytes)
    })
    staticFS, err := fs.Sub(s.cfg.WebUIFS, "static")
    if err != nil {
        return fmt.Errorf("webui: fs.Sub(static): %w", err)
    }
    mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}
```

import に `"io/fs"` (既に Step 1 で追加) と `"net/http"` (既存) を確認。

- [ ] **Step 3: `cmd/harness-server/main.go` で webui/ を embed**

ファイル冒頭の import 群の後に embed 宣言を追加:

```go
import (
    // ... 既存 ...
    "embed"
    "io/fs"

    "github.com/on-keyday/agent-harness/cli"
    "github.com/on-keyday/agent-harness/server"
)

//go:embed all:webui
var webuiFSEmbed embed.FS
```

`server.New` 呼び出しに `WebUIFS` を渡す:

```go
webuiFS, err := fs.Sub(webuiFSEmbed, "webui")
if err != nil {
    slog.Error("webui sub fs", "err", err)
    os.Exit(1)
}
s := server.New(server.Config{
    Addr:          *listen,
    DataDir:       *dataDir,
    TaskRetention: *taskRetain,
    Logger:        slog.Default(),
    WebUIFS:       webuiFS,
})
```

> **NOTE:** `//go:embed all:webui` は webui/ 以下を再帰的に取り込む (`.gitignore` で git は無視するが embed は取り込むので、`webui/static/main.wasm` が `make webui-build` 経由で実体化されている前提)。`make webui-build` を `go build ./cmd/harness-server` の前に必ず走らせる必要がある旨を Makefile (Task 7) で明示する。

- [ ] **Step 4: ビルド確認**

Run:
```sh
# Step 4-pre: webui/static/main.wasm が無いと go build が embed エラーになるか確認
GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/
go build ./cmd/harness-server/
```

Expected: クリーン。Step 4-pre なしで `go build ./cmd/harness-server/` を打つと `embed: pattern all:webui: no matching files found` のような失敗 (wasm が無いため)。これは正しい挙動 (Makefile で順序を強制する)。

- [ ] **Step 5: native テスト**

Run:
```sh
go build ./...
go test ./server/...
```

Expected: 全パス。

- [ ] **Step 6: commit**

```sh
git add server/server.go cmd/harness-server/main.go
git commit -m "$(cat <<'EOF'
server+harness-server: WebUIFS で webui/ を埋め込み配信

- server.Config に WebUIFS fs.FS を追加。non-nil なら server.Run の
  内部 mux に "/" (index.html) と "/static/" (FileServer) を登録。
  起動時に main.wasm の存在をアサート (`make webui-build` 忘れ防止)
- cmd/harness-server/main.go で //go:embed all:webui で同梱。
  fs.Sub("webui") で root を切って WebUIFS に渡す

これで harness-server 1 プロセスから index.html / wasm / xterm.js /
WebSocket /ws まで全部配信できるようになる。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Makefile

**Files:**
- Create: `Makefile`

- [ ] **Step 1: `Makefile` 新規作成**

```makefile
.PHONY: all build webui-build wasm-check test vet clean help

GOROOT := $(shell go env GOROOT)
WASM_EXEC := $(GOROOT)/lib/wasm/wasm_exec.js

all: build

# Build the wasm module and refresh wasm_exec.js from the current Go SDK.
webui-build:
	GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/
	cp $(WASM_EXEC) webui/static/wasm_exec.js

# Build all native binaries. Requires webui-build to have run at least once
# (cmd/harness-server uses //go:embed all:webui which needs static/main.wasm).
build: webui-build
	go build ./...

# Static check that the wasm-relevant packages still compile under
# GOOS=js GOARCH=wasm. Run before commit.
wasm-check:
	GOOS=js GOARCH=wasm go build ./cli/... ./transport/... ./cmd/harness-webui-wasm/

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f webui/static/main.wasm
	go clean ./...

help:
	@echo "Targets:"
	@echo "  webui-build   build wasm module + refresh wasm_exec.js"
	@echo "  build         webui-build then go build ./..."
	@echo "  wasm-check    GOOS=js GOARCH=wasm go build (lint level)"
	@echo "  test          go test ./..."
	@echo "  vet           go vet ./..."
	@echo "  clean         remove build artifacts"
```

- [ ] **Step 2: 動作確認**

Run:
```sh
make clean
make webui-build
ls -la webui/static/main.wasm webui/static/wasm_exec.js
make build
make wasm-check
make test
make vet
```

Expected:
- `make webui-build`: `webui/static/main.wasm` 生成、`wasm_exec.js` リフレッシュ。
- `make build`: `harness-server` / `harness-cli` / `agent-runner` / `harness-tui` バイナリ生成。
- `make wasm-check`: クリーン。
- `make test`: 全パス。
- `make vet`: pre-existing 警告のみ。

- [ ] **Step 3: commit**

```sh
git add Makefile
git commit -m "$(cat <<'EOF'
Makefile: webui-build / build / wasm-check / test / vet / clean

go build, GOOS=js GOARCH=wasm go build, wasm_exec.js のリフレッシュ
を 1 つの Makefile に集約。dogfood 想定の最小構成。`make build` は
内部で `webui-build` を必ず先に走らせるので main.wasm が無い状態で
harness-server の embed.FS を空にする事故を防ぐ。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: 最終回帰確認 + 手動 smoke

**Files:**
- 既存ファイル全体 (確認のみ、変更なし)

- [ ] **Step 1: 全体 build / vet / test**

Run:
```sh
make clean
make build
make wasm-check
make test
make vet
```

Expected: 全パス。`exec/frame/frame.go:248,267` の pre-existing unreachable 警告のみ許容。

- [ ] **Step 2: 手元 smoke (controller でできる範囲)**

ターミナル 1: harness-server 起動
```sh
./cmd/harness-server/harness-server --listen=127.0.0.1:8539
```
(or `go run ./cmd/harness-server --listen=127.0.0.1:8539`)

ブラウザで `http://127.0.0.1:8539/` を開く。Expected:
- `index.html` が表示される
- DevTools Network タブで `/static/main.wasm`、`/static/xterm.js`、`/static/xterm.css`、`/static/wasm_exec.js`、`/static/main.js`、`/static/style.css` が 200 OK
- `#status` が "loading wasm…" → "connecting…" → "connected" と推移

- [ ] **Step 3: ブラウザ手動 smoke (interactive 含む)**

ターミナル 2: agent-runner 起動
```sh
go run ./cmd/agent-runner --server-cid='ws:127.0.0.1:8539-*' --repo /tmp/test-repo --claude-bin claude
```

ブラウザの cmdline で:
```
submit --repo=/tmp/test-repo --task="echo hello"
```

Expected:
- task が enqueue される (`#cmd-output` に taskID 表示)
- `#task-list` の poll または watch push で task が Queued → Running → Succeeded 遷移
- (claude 実行中なら) interactive: task id を `#task-id-input` に入れて Attach → xterm が開いて claude プロンプト表示
- キー入力 / 出力が流れる
- Detach で session 終了

エラーが出たら原因特定して該当タスクに戻って修正。

- [ ] **Step 4: 完了確認**

Phase 2 完了。`docs/superpowers/plans/2026-04-27-wasm-phase2-browser-build.md` の全 step が completed であることを確認し、WASM 対応 (transport refactor + ブラウザビルド) を完了とする。
