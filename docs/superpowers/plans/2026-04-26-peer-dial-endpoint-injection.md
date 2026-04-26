# peer.Dial Endpoint Injection リファクタ Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `peer.Dial` を `Endpoint` 引数受け取り型に変え、`peer` パッケージから `transport` への依存を切る。同時に CLI フラグを ConnectionID 文字列入力に変更し、`cli.Prune` の責務を単一化（`prune` と `prune-local` の subcommand 分離）、`tui` の dead な `--offline` 関連を削除する。

**Architecture:** `objproto.Endpoint` の構築を呼び出し側（cli/runner main）の責務に押し出し、`peer.Dial(ctx, ep, peerCID, cfg)` という形に統一。`MustParseConnectionID` の本番使用を `objproto.ParseConnectionID` 経由に置換。`cli.Prune` を「サーバ task forget のみ」、`cli.PruneLocal` を「ローカルワークツリー削除のみ」に責務分割。

**Tech Stack:** Go 1.25.7, 既存の objproto / peer / cli / runner / cmd / tui / transport パッケージ群。新規依存・新規パッケージなし。

**Spec:** `docs/superpowers/specs/2026-04-26-peer-dial-endpoint-injection-design.md`. 大局的なコンテキストはまずこちらを読むこと。

---

## Reference for implementers

### ConnectionID parse の規約

`objproto.ParseConnectionID(s string, opts ParseOption)` は `"transport:host:port-id"` 形式の文字列を `objproto.ConnectionID` に変換する。本リファクタの parse 呼び出しは:

```go
peerCID, err := objproto.ParseConnectionID(*serverCID,
    objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr)
```

- `ParseOption_AllowRandomID`: `id` 部が `*` のときランダム uint16 を生成（デフォルト値 `"-*"` のための必須フラグ）
- `ParseOption_ResolveAddr`: host が IP リテラルでない場合 `net.LookupIP` で解決（`localhost:8539` を受け付けるため）

`MustParseConnectionID` は test 用（`objproto/objproto.go:149` のコメント明示）。本番コードでは使わない。

### Endpoint の所有

`objproto.Endpoint` は process-scoped（`objproto/session.go:29-40`）で、Close API がない設計。本リファクタでは cli の各 subcommand は短命接続（cli.Dial → 操作 → c.Close()）、runner は long-lived 1 接続で運用。Endpoint をプロセス間で共有するパターンは取らず、各呼び出し側の Dial 時点で 1 個作る現状のシンプル形を維持する。

### dead arg `addr` の規約

`transport.WebSocketEndpoint(logger, addr, tlsConf, mode)` の `addr` 引数は `EndpointModeClient` 経路では `httpServer.Addr` にしか使われない（つまり client では使われない）。Client 用には常に `""` を渡す。`tlsConf` は client モードでも `websocket.DialConfig.TlsConfig` で dial 時 TLS 設定として使われるため dead ではない（本リファクタでは `nil` のまま）。

### `objproto.NewEndpoint` のモード分岐

`objproto.NewEndpoint(logger, mode)` の mode は `Client` / `Server` / `Mutual` の 3 つ。cli/runner はすべて `EndpointModeClient`。

### bgn-generated メッセージ操作

`runner/protocol/message.go` の union 型（`TaskControlRequest` 等）は getter/setter で variant にアクセスする。本リファクタでは新たな bgn メッセージ操作を加えないため詳細は省略（spec 参照）。

### 1111/2222 規約問題

旧コードでは `peer.DialConfig.UniqueNumber` で `runner=1111, cli=2222` を ID として ConnectionID に埋めていた。本リファクタで CLI フラグデフォルトを `-*` (random) にすると、server 側で `cid.ID == 1111` / `cid.ID == 2222` 判定があると壊れる。**Task 1 で grep して確認**し、依存があればその時点で対処方針を決める（spec の「リスク・注意点」参照）。

---

## File structure

### Modify

```
peer/conn.go                          Dial シグネチャ変更、DialConfig 縮小、transport import 除去、portFrom 削除、Close コメント更新
cli/client.go                         cli.Dial 内で Endpoint 構築 + 新 peer.Dial 呼び出し、引数型変更
cli/submit.go                         引数 addr string → peerCID objproto.ConnectionID
cli/list.go                           同上
cli/cancel.go                         同上
cli/logs.go                           同上
cli/watch.go                          同上
cli/open_interactive.go               同上
cli/get_log.go                        GetTaskLog の引数型変更（waitForReceiveStream は不変）
cli/prune.go                          Task 2: 引数型変更のみ。Task 3: 責務分割（Prune を server forget のみ、PruneLocal を新設）
runner/connect.go                     Config.ServerCID + Endpoint 構築 + 新 peer.Dial
cmd/harness-cli/main.go               --server-cid フラグ、parseCID helper、Task 3 で prune と prune-local の subcommand 分離
cmd/agent-runner/main.go              --server-cid フラグ、runner.Config.ServerCID 渡し
cmd/harness-tui/main.go               --server-cid フラグ、cli.Dial 呼び出し更新
tui/cmdline.go                        PruneAction.Offline フィールド削除と offline flag 定義削除
tui/app.go                            help text の [--offline] 削除、offline 警告ロジック削除
cli/prune_test.go                     Task 2: zero value ConnectionID で対応。Task 3: PruneLocal 呼び出しに変更
integration/e2e_test.go               フラグ書式変更と prune-local 対応
transport/websocket.go                godoc コメントに dead arg 注釈を 1 行追加
```

### 触らない

`transport/dualstack.go`, `transport/udp*.go`, `transport/websocket_*.go` 系プラットフォーム別ファイル, `server/server.go`, `objproto/*.go`, `tui/` のうち `cmdline.go` と `app.go` 以外。`transport/websocket.go` も実装ロジックは触らずコメントのみ追加。

---

## Tasks

### Task 1: 1111/2222 / UniqueNumber 依存箇所の調査

**Files:**
- 調査対象: コードベース全体（特に `server/`, `runner/`, `peer/`, `cli/`）

- [ ] **Step 1: grep で固定 ID 依存を洗う**

Run:
```sh
grep -rn "1111\|2222" --include='*.go' | grep -v "_test.go" | grep -v "RUNNER_PORT_DEFAULT" | grep -v "DEFAULT_PORT"
grep -rn "UniqueNumber" --include='*.go'
```

Expected: 既知の参照箇所は `peer/conn.go` 内の hack 構築コード (`fmt.Sprintf("ws:127.0.0.1:%s-%d", port, UniqueNumber)`)、`cli/client.go` と `runner/connect.go` の `peer.DialConfig{UniqueNumber: ...}` 渡し。これら以外で **server 側に `cid.ID == 1111/2222` 判定**が出てきたら要注意。

- [ ] **Step 2: 結果の判定**

ケース A（server 側に固定 ID 判定が無い）: そのまま Task 2 へ進む。
ケース B（server 側で `cid.ID == 1111` 等の判定が発見された）: 計画に **追加サブタスク**（役割識別を Hello メッセージ等で別経路にする）を挿入してから Task 2 へ進む。本ドキュメントを編集して該当タスクを追加し、commit する。

- [ ] **Step 3: 調査結果を簡単にメモ**

調査結果を以下のいずれかとして記録（plan ファイルにインライン追記、または手元メモ）:
- 「ケース A: 固定 ID 依存なし、計画通り進める」
- 「ケース B: 固定 ID 依存ありで対処サブタスクを追加した」

ここでは commit 不要（plan のみの追記なら別途 commit）。

---

### Task 2: peer.Dial を Endpoint 引数受け取り型に切り替え（atomic refactor）

このタスクは複数ファイルにまたがる atomic な変更で、途中の中間状態ではビルドが通らない。すべての step を完了してから 1 つの commit にまとめる。

**Files:**
- Modify: `peer/conn.go`
- Modify: `cli/client.go`, `cli/submit.go`, `cli/list.go`, `cli/cancel.go`, `cli/logs.go`, `cli/watch.go`, `cli/open_interactive.go`, `cli/get_log.go`, `cli/prune.go`, `cli/prune_test.go`
- Modify: `runner/connect.go`
- Modify: `cmd/harness-cli/main.go`, `cmd/agent-runner/main.go`, `cmd/harness-tui/main.go`

- [ ] **Step 1: `peer/conn.go` の DialConfig 縮小と Dial シグネチャ変更**

`DialConfig` から `Addr` と `UniqueNumber` フィールドを削除:

```go
type DialConfig struct {
    // Logger; defaults to slog.Default() when nil.
    Logger *slog.Logger
    // PingInterval; defaults to 30s when zero.
    PingInterval time.Duration
}
```

`Dial` 関数のシグネチャ・本体を変更:

```go
// Dial wires up an objproto Connection (via ECDH on the supplied Endpoint),
// a trsf transport, AutoSend, and AutoPing on top of the given peerCID. The
// caller owns ep — its lifetime is independent of the returned *Conn.
// AutoReceive is NOT started yet; the caller must call SetOnControl
// (optional) and then Start before any inbound message can be processed.
func Dial(ctx context.Context, ep objproto.Endpoint, peerCID objproto.ConnectionID, cfg DialConfig) (*Conn, error) {
    if cfg.Logger == nil {
        cfg.Logger = slog.Default()
    }
    if cfg.PingInterval <= 0 {
        cfg.PingInterval = 30 * time.Second
    }

    conn, err := objproto.DoECDHHandshake(ctx, ep,
        peerCID,
        ecdh.P521(), objproto.AES128GCM)
    if err != nil {
        return nil, fmt.Errorf("ecdh: %w", err)
    }
    p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)

    c := &Conn{
        conn:      conn,
        trans:     p,
        pub:       pubsub.NewClient(),
        log:       cfg.Logger,
        done:      make(chan struct{}),
        pubTopics: map[string]*pubTopic{},
    }
    go trsf.AutoSend(ctx, p, conn, nil)
    go trsf.AutoPing(ctx, conn, cfg.PingInterval)
    return c, nil
}
```

`peer/conn.go` の package doc コメントの `pc, err := peer.Dial(ctx, peer.DialConfig{...})` の例を新シグネチャに合わせて更新:

```go
//	pc, err := peer.Dial(ctx, ep, peerCID, peer.DialConfig{...})  // ECDH+trsf+AutoSend+AutoPing only
```

- [ ] **Step 2: `peer/conn.go` の portFrom helper 削除と Close コメント更新**

ファイル末尾の `portFrom` 関数（`peer/conn.go:202-211`）を削除:

```go
// 削除対象:
// portFrom extracts the port portion from a "host:port" string. ...
// func portFrom(addr string) string { ... }
```

`Close` のコメント（`peer/conn.go:152-158`）を以下に更新:

```go
// Close sends a wire-level Close to the peer (best-effort; lets the server
// deregister the runner / drop the subscriber immediately instead of
// waiting for the idle GC) and then releases the underlying objproto
// connection. The owning objproto.Endpoint is owned by the caller and is
// NOT torn down here — peer.Dial accepts an externally constructed Endpoint
// and does not assume ownership.
func (c *Conn) Close() {
    _ = trsf.SendClose(c.conn)
    _ = c.conn.Close()
}
```

`peer/conn.go` の import から `"github.com/on-keyday/agent-harness/transport"` を削除。`crypto/ecdh` も `objproto.DoECDHHandshake` の引数として残るので維持。`fmt` は他で使っているか確認し、未使用なら削除。

- [ ] **Step 3: `cli/client.go` の Dial を新シグネチャに**

```go
import (
    // ...既存の import...
    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/transport"
)

// Dial establishes the underlying peer.Conn and starts the receive loop
// with this Client's TaskControl-aware handler. The peerCID identifies
// which server peer to ECDH with (e.g. parsed from --server-cid).
func Dial(ctx context.Context, peerCID objproto.ConnectionID) (*Client, error) {
    ep, err := transport.WebSocketEndpoint(slog.Default(), "", nil, objproto.EndpointModeClient)
    if err != nil {
        return nil, fmt.Errorf("ws endpoint: %w", err)
    }
    pc, err := peer.Dial(ctx, ep, peerCID, peer.DialConfig{
        Logger: slog.Default(),
    })
    if err != nil {
        return nil, err
    }
    c := &Client{
        conn:    pc,
        pending: map[uint32]chan *protocol.TaskControlResponse{},
    }
    pc.SetOnControl(c.dispatchControl)
    pc.Start(ctx)
    return c, nil
}
```

- [ ] **Step 4: cli パッケージの他のエントリ関数で引数型変更**

`cli/submit.go`, `cli/list.go`, `cli/cancel.go`, `cli/logs.go`, `cli/watch.go`, `cli/open_interactive.go`, `cli/get_log.go`, `cli/prune.go` の各ファイルで、エントリ関数の `addr string` 引数を `peerCID objproto.ConnectionID` に置換し、内部の `Dial(ctx, addr)` 呼び出しを `Dial(ctx, peerCID)` に書き換える。

各ファイルに `"github.com/on-keyday/agent-harness/objproto"` の import を追加。

例 (`cli/submit.go`):

```go
import (
    "context"
    "fmt"
    // ... その他 ...
    "github.com/on-keyday/agent-harness/objproto"
)

func Submit(ctx context.Context, peerCID objproto.ConnectionID, repo, prompt string) (string, error) {
    c, err := Dial(ctx, peerCID)
    if err != nil { return "", err }
    defer c.Close()
    // ... 残りはそのまま ...
}
```

同じパターンを以下の関数に適用:

| ファイル | 関数 | After |
|---|---|---|
| `cli/list.go` | `List` | `func List(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error` |
| `cli/cancel.go` | `Cancel` | `func Cancel(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string) error` |
| `cli/logs.go` | `Logs` | `func Logs(ctx context.Context, peerCID objproto.ConnectionID, taskID string, out io.Writer) error` |
| `cli/watch.go` | `Watch` | `func Watch(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error` |
| `cli/open_interactive.go` | `Interactive` | `func Interactive(ctx context.Context, peerCID objproto.ConnectionID, repo string) (string, error)` |
| `cli/get_log.go` | `GetTaskLog` | `func GetTaskLog(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string) ([]byte, bool, error)` |
| `cli/prune.go` | `PruneTasks` | `func PruneTasks(ctx context.Context, peerCID objproto.ConnectionID, cutoff time.Time) (uint32, error)` |

- [ ] **Step 5: `cli/prune.go` の Prune は signature だけ型変更（責務分割は Task 3）**

`Prune` の `addr string` を `peerCID objproto.ConnectionID` に変える。中の `if addr != ""` を一時的に `if peerCID != (objproto.ConnectionID{})` に置き換え（Task 3 で責務分割される際にこの zero value 判定は消える）。`PruneTasks(ctx, addr, cutoff)` 呼び出しも `PruneTasks(ctx, peerCID, cutoff)` に。

```go
func Prune(ctx context.Context, peerCID objproto.ConnectionID, repo string, before time.Duration, out io.Writer) error {
    cutoff := time.Now().Add(-before)
    var zero objproto.ConnectionID
    if peerCID != zero {
        if removed, err := PruneTasks(ctx, peerCID, cutoff); err != nil {
            fmt.Fprintf(out, "warning: server prune skipped: %v\n", err)
        } else if removed > 0 {
            fmt.Fprintf(out, "server forgot %d task(s)\n", removed)
        }
    }
    // ... worktree walk は既存のまま ...
}
```

- [ ] **Step 6: `cli/prune_test.go` を一時的に zero value ConnectionID で対応**

既存テストの `Prune(ctx, "", repo, ...)` を `Prune(ctx, objproto.ConnectionID{}, repo, ...)` に置換（Task 3 で `PruneLocal` 呼び出しに最終調整される）:

```go
import (
    // ... 既存 ...
    "github.com/on-keyday/agent-harness/objproto"
)

// 該当 2 箇所 (line 45, line 55 相当)
if err := Prune(context.Background(), objproto.ConnectionID{}, repo, 7*24*time.Hour, &out); err != nil {
    // ...
}
err := Prune(context.Background(), objproto.ConnectionID{}, t.TempDir(), 7*24*time.Hour, &out)
```

- [ ] **Step 7: `runner/connect.go` の Config と Run を新形に**

```go
import (
    // 既存 +
    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/transport"
)

// Config holds the configuration for the runner connection.
type Config struct {
    ServerCID       objproto.ConnectionID  // was: ServerAddr string
    RepoPath        string
    ClaudeBin       string
    ExtraClaudeArgs []string
    Logger          *slog.Logger
}

func Run(ctx context.Context, cfg Config) error {
    if cfg.Logger == nil {
        cfg.Logger = slog.Default()
    }
    ep, err := transport.WebSocketEndpoint(cfg.Logger, "", nil, objproto.EndpointModeClient)
    if err != nil {
        return fmt.Errorf("ws endpoint: %w", err)
    }
    pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
        Logger:       cfg.Logger,
        PingInterval: 30 * time.Second,
    })
    if err != nil {
        return err
    }
    defer pc.Close()
    // ... 以下 sender, session, SetOnControl など既存ロジックそのまま ...
}
```

- [ ] **Step 8: `cmd/agent-runner/main.go` のフラグ更新**

```go
package main

import (
    "context"
    "flag"
    "fmt"
    "log/slog"
    "os"
    "os/signal"
    "path/filepath"
    "strings"

    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner"
)

var (
    serverCID  = flag.String("server-cid", "ws:localhost:8539-*", "server ConnectionID (e.g. ws:host:port-id, * for random)")
    repo       = flag.String("repo", ".", "absolute path to the repo this runner serves")
    claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
    claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p (whitespace-separated)")
)

func main() {
    flag.Parse()
    abs, err := filepath.Abs(*repo)
    if err != nil {
        slog.Error("repo abs", "err", err)
        os.Exit(1)
    }
    peerCID, err := objproto.ParseConnectionID(*serverCID,
        objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
    if err != nil {
        slog.Error("server-cid", "err", err)
        os.Exit(1)
    }
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
    defer cancel()
    if err := runner.Run(ctx, runner.Config{
        ServerCID:       peerCID,
        RepoPath:        abs,
        ClaudeBin:       *claudeBin,
        ExtraClaudeArgs: strings.Fields(*claudeArgs),
        Logger:          slog.Default(),
    }); err != nil {
        slog.Error("runner exit", "err", err)
        os.Exit(1)
    }
    _ = fmt.Sprint // keep imports if not used elsewhere
}
```

- [ ] **Step 9: `cmd/harness-tui/main.go` のフラグ更新**

```go
import (
    // 既存 +
    "github.com/on-keyday/agent-harness/objproto"
)

var (
    serverCID = flag.String("server-cid", "ws:localhost:8539-*", "harness-server ConnectionID (e.g. ws:host:port-id, * for random)")
    repoFlag  = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
    flag.Parse()
    repoAbs, err := filepath.Abs(*repoFlag)
    if err != nil {
        fmt.Fprintln(os.Stderr, "repo:", err)
        os.Exit(1)
    }
    peerCID, err := objproto.ParseConnectionID(*serverCID,
        objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
    if err != nil {
        fmt.Fprintln(os.Stderr, "server-cid:", err)
        os.Exit(1)
    }

    // ...slog handler 設定はそのまま...

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
    defer cancel()

    app := tui.New(tui.Config{
        Server:      *serverCID,  // 文字列のまま、表示用
        DefaultRepo: repoAbs,
    })
    program := tea.NewProgram(app, tea.WithAltScreen())
    app.BindProgram(program)
    app.BindContext(ctx)
    slogHandler.BindProgram(program)

    go func() {
        c, err := cli.Dial(ctx, peerCID)
        if err != nil {
            program.Send(tui.ConnectionMsg{Connected: false, Err: err})
            return
        }
        // ...以下既存と同じ...
    }()
    // ...
}
```

- [ ] **Step 10: `cmd/harness-cli/main.go` のフラグ更新（subcommand 分離は Task 3）**

`--server` を `--server-cid` にリネームし、`parseCID` helper を追加。各 subcommand 内で `parseCID()` を呼んで `cli.X(ctx, peerCID, ...)` を呼ぶ。`prune --offline` は Task 3 で正式に分離するが、このタスクでは現状の体系を維持しつつ型変更だけ追従する形:

```go
package main

import (
    "context"
    "flag"
    "fmt"
    "os"
    "path/filepath"
    "time"

    "github.com/on-keyday/agent-harness/cli"
    "github.com/on-keyday/agent-harness/objproto"
)

func main() {
    serverCID := flag.String("server-cid", "ws:localhost:8539-*",
        "server ConnectionID (e.g. ws:host:port-id, * for random)")
    flag.Usage = usage
    flag.Parse()

    if flag.NArg() == 0 {
        usage()
        os.Exit(2)
    }
    sub := flag.Arg(0)
    args := flag.Args()[1:]
    ctx := context.Background()

    parseCID := func() objproto.ConnectionID {
        peerCID, err := objproto.ParseConnectionID(*serverCID,
            objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
        if err != nil {
            die(fmt.Errorf("server-cid: %w", err))
        }
        return peerCID
    }

    switch sub {
    case "submit":
        fs := flag.NewFlagSet("submit", flag.ExitOnError)
        repo := fs.String("repo", ".", "path to repo (defaults to cwd)")
        task := fs.String("task", "", "prompt text")
        fs.Parse(args)
        if *task == "" {
            fmt.Fprintln(os.Stderr, "submit: --task is required")
            os.Exit(2)
        }
        abs, err := filepath.Abs(*repo)
        if err != nil { die(err) }
        id, err := cli.Submit(ctx, parseCID(), abs, *task)
        if err != nil { die(err) }
        fmt.Println(id)

    case "ls":
        if err := cli.List(ctx, parseCID(), os.Stdout); err != nil { die(err) }

    case "cancel":
        if len(args) == 0 {
            fmt.Fprintln(os.Stderr, "cancel: missing task id")
            os.Exit(2)
        }
        if err := cli.Cancel(ctx, parseCID(), args[0]); err != nil { die(err) }

    case "prune":
        // Task 3 で subcommand 分離する。Task 2 では現状の --offline を維持して型のみ追従。
        fs := flag.NewFlagSet("prune", flag.ExitOnError)
        repo := fs.String("repo", ".", "repo to prune")
        before := fs.Duration("before", 7*24*time.Hour, "remove worktrees and forget tasks older than this")
        offline := fs.Bool("offline", false, "skip the server task-forget step (worktrees only)")
        fs.Parse(args)
        abs, err := filepath.Abs(*repo)
        if err != nil { die(err) }
        var pcid objproto.ConnectionID
        if !*offline {
            pcid = parseCID()
        }
        if err := cli.Prune(ctx, pcid, abs, *before, os.Stdout); err != nil { die(err) }

    case "logs":
        if len(args) == 0 {
            fmt.Fprintln(os.Stderr, "logs: missing task id")
            os.Exit(2)
        }
        if err := cli.Logs(ctx, parseCID(), args[0], os.Stdout); err != nil { die(err) }

    case "watch":
        if err := cli.Watch(ctx, parseCID(), os.Stdout); err != nil { die(err) }

    case "interactive":
        fs := flag.NewFlagSet("interactive", flag.ExitOnError)
        repo := fs.String("repo", ".", "path to repo (defaults to cwd)")
        fs.Parse(args)
        abs, err := filepath.Abs(*repo)
        if err != nil { die(err) }
        if _, err := cli.Interactive(ctx, parseCID(), abs); err != nil { die(err) }

    default:
        usage()
        os.Exit(2)
    }
}

func usage() {
    fmt.Fprintln(os.Stderr, "usage: harness-cli [--server-cid CID] <subcommand> [args]")
    fmt.Fprintln(os.Stderr, "  submit [--repo PATH] --task TEXT    enqueue a new task (--repo defaults to cwd)")
    fmt.Fprintln(os.Stderr, "  ls                                  list runners and recent tasks")
    fmt.Fprintln(os.Stderr, "  cancel TASK_ID                      cancel a queued/running task")
    fmt.Fprintln(os.Stderr, "  prune [--repo PATH] [--before DUR]  remove old worktrees and forget old tasks (--offline = local only)")
    fmt.Fprintln(os.Stderr, "  logs TASK_ID                        stream task log output")
    fmt.Fprintln(os.Stderr, "  watch                               stream task and runner status events")
    fmt.Fprintln(os.Stderr, "  interactive [--repo PATH]           attach an interactive PTY claude session")
}

func die(err error) {
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
}
```

- [ ] **Step 11: ビルド確認**

Run:
```sh
go build ./...
```

Expected: エラーなし。

未使用 import 等のエラーが出たら個別に修正。`cli/prune_test.go` も `go vet` 等で確認。

- [ ] **Step 12: テスト実行**

Run:
```sh
go test ./...
```

Expected: pass（既存テスト群は zero value `ConnectionID` 経由で従来挙動を保持しているはず）。`integration/e2e_test.go` がフラグ書式変更で失敗する可能性 → これは Task 5 で扱う。一時的に `t.Skip` でも可だが、まず実状を確認。

- [ ] **Step 13: commit**

```sh
git add peer/conn.go cli/*.go runner/connect.go cmd/harness-cli/main.go cmd/agent-runner/main.go cmd/harness-tui/main.go
git commit -m "$(cat <<'EOF'
peer+cli+runner+cmd: peer.Dial を Endpoint 引数受け取り型に切替

- peer.Dial を Dial(ctx, ep, peerCID, cfg) に変更し、内部での
  transport.WebSocketEndpoint 構築を呼び出し側に移譲
- DialConfig から Addr/UniqueNumber を削除、ConnectionID は呼び出し側
  で ParseConnectionID から構築する
- cli/runner の各 callsite と cmd/* の CLI フラグ (--server-cid) を更新
- peer から transport への import を除去
- cli.Prune は signature 型変更のみで責務分割は次タスク

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: cli.Prune の責務分割と prune-local subcommand 追加

**Files:**
- Modify: `cli/prune.go`
- Modify: `cmd/harness-cli/main.go`
- Modify: `cli/prune_test.go`

- [ ] **Step 1: `cli/prune.go` の Prune を server forget のみに、PruneLocal を新設**

```go
package cli

import (
    "context"
    "fmt"
    "io"
    "os"
    "os/exec"
    "path/filepath"
    "time"

    "github.com/on-keyday/agent-harness/objproto"
    "github.com/on-keyday/agent-harness/runner/protocol"
)

// Prune asks the server to forget terminal tasks older than `before`.
// This used to also walk local worktrees; that step is now in PruneLocal.
func Prune(ctx context.Context, peerCID objproto.ConnectionID, before time.Duration, out io.Writer) error {
    cutoff := time.Now().Add(-before)
    removed, err := PruneTasks(ctx, peerCID, cutoff)
    if err != nil {
        return err
    }
    if removed > 0 {
        fmt.Fprintf(out, "server forgot %d task(s)\n", removed)
    }
    return nil
}

// PruneLocal walks <repo>/.harness-worktrees/ and `git worktree remove --force`
// the entries whose ModTime is older than `before`. No server interaction.
func PruneLocal(ctx context.Context, repo string, before time.Duration, out io.Writer) error {
    cutoff := time.Now().Add(-before)
    dir := filepath.Join(repo, ".harness-worktrees")
    entries, err := os.ReadDir(dir)
    if os.IsNotExist(err) {
        return nil
    }
    if err != nil {
        return err
    }
    for _, e := range entries {
        info, err := e.Info()
        if err != nil { continue }
        if info.ModTime().After(cutoff) { continue }

        path := filepath.Join(dir, e.Name())
        cmd := exec.Command("git", "worktree", "remove", "--force", path)
        cmd.Dir = repo
        if out2, cerr := cmd.CombinedOutput(); cerr != nil {
            fmt.Fprintf(out, "skip %s: %s\n", e.Name(), out2)
            continue
        }
        fmt.Fprintf(out, "removed %s\n", e.Name())
    }
    return nil
}

// PruneTasks asks the server to forget terminal tasks whose EndedAt is before
// cutoff. Internal helper used by Prune; exposed for callers that want the
// raw count (e.g. tui).
func PruneTasks(ctx context.Context, peerCID objproto.ConnectionID, cutoff time.Time) (uint32, error) {
    c, err := Dial(ctx, peerCID)
    if err != nil { return 0, err }
    defer c.Close()

    req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks}
    req.SetPrune(protocol.PruneTasksRequest{BeforeTs: uint64(cutoff.UnixNano())})
    resp, err := c.RoundTripTaskControl(ctx, req)
    if err != nil { return 0, err }
    if resp.Kind != protocol.TaskControlKind_PruneTasks {
        return 0, fmt.Errorf("unexpected response kind: %v", resp.Kind)
    }
    pr := resp.Prune()
    if pr == nil {
        return 0, fmt.Errorf("empty prune response")
    }
    return pr.Removed, nil
}
```

`Prune` の `repo` 引数が消えたことに注意。`Dial` 経由でサーバ通信を行うので `peerCID` のゼロ値判定は不要（不正な CID なら `Dial` がエラーを返す）。

- [ ] **Step 2: `cmd/harness-cli/main.go` の subcommand を `prune` と `prune-local` に分離**

`switch sub` の `prune` case を以下のように書き換え、`prune-local` を新規追加:

```go
case "prune":
    fs := flag.NewFlagSet("prune", flag.ExitOnError)
    before := fs.Duration("before", 7*24*time.Hour, "forget terminal tasks older than this")
    fs.Parse(args)
    if err := cli.Prune(ctx, parseCID(), *before, os.Stdout); err != nil { die(err) }

case "prune-local":
    fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
    repo := fs.String("repo", ".", "repo to prune")
    before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
    fs.Parse(args)
    abs, err := filepath.Abs(*repo)
    if err != nil { die(err) }
    if err := cli.PruneLocal(ctx, abs, *before, os.Stdout); err != nil { die(err) }
```

`usage` 関数の help text を更新:

```go
fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]")
fmt.Fprintln(os.Stderr, "                                      remove old worktrees in <repo>/.harness-worktrees/")
```

- [ ] **Step 3: `cli/prune_test.go` を `PruneLocal` 呼び出しに変更**

```go
package cli

import (
    "bytes"
    "context"
    "os"
    "os/exec"
    "path/filepath"
    "testing"
    "time"
)

func TestPruneRemovesOldWorktrees(t *testing.T) {
    repo := t.TempDir()
    // ... git init / commit / worktree add の既存セットアップそのまま ...

    var out bytes.Buffer
    // 旧: Prune(context.Background(), "", repo, ...)
    // 旧 (Task 2 中間): Prune(context.Background(), objproto.ConnectionID{}, repo, ...)
    // 新:
    if err := PruneLocal(context.Background(), repo, 7*24*time.Hour, &out); err != nil {
        t.Fatalf("PruneLocal: %v", err)
    }
    // ... 検証は既存のまま ...
}

func TestPruneNoWorktreesDir(t *testing.T) {
    var out bytes.Buffer
    err := PruneLocal(context.Background(), t.TempDir(), 7*24*time.Hour, &out)
    if err != nil {
        t.Fatalf("PruneLocal: %v", err)
    }
}
```

`objproto` import が不要になるので削除。

- [ ] **Step 4: ビルドとテスト**

Run:
```sh
go build ./... && go test ./cli/...
```

Expected: pass.

- [ ] **Step 5: commit**

```sh
git add cli/prune.go cli/prune_test.go cmd/harness-cli/main.go
git commit -m "$(cat <<'EOF'
cli+harness-cli: Prune の責務分割と prune-local subcommand 追加

- cli.Prune を server task forget のみに縮小し、ローカルワークツリー
  削除を cli.PruneLocal に分離
- harness-cli に prune-local subcommand を追加 (--offline フラグの
  廃止に対応)
- cli/prune_test.go を PruneLocal 呼び出しに変更

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: tui の `--offline` 関連 dead code 削除

**Files:**
- Modify: `tui/cmdline.go`
- Modify: `tui/app.go`

- [ ] **Step 1: `tui/cmdline.go` から `PruneAction.Offline` と offline flag を削除**

`PruneAction` 構造体の `Offline bool` フィールドを削除（line 28 付近）:

```go
type PruneAction struct {
    Before time.Duration
    // Offline bool  ← 削除
}
```

`prune` を parse する箇所（line 144 付近）から `offline := fs.Bool("offline", ...)` を削除し、`PruneAction{... Offline: *offline}` から `Offline:` を消す:

```go
// 修正前:
offline := fs.Bool("offline", false, "")
fs.Parse(rest)
return PruneAction{Before: *before, Offline: *offline}, nil

// 修正後:
fs.Parse(rest)
return PruneAction{Before: *before}, nil
```

- [ ] **Step 2: `tui/app.go` の help text と offline 警告ロジックを削除**

`tui/app.go:570` 付近の help text から `[--offline]` を削除:

```go
// 修正前:
a.cmdresult.Append("commands: submit / interactive [--repo=PATH] / cancel <id> / prune [--before=DUR] [--offline] / repo <path> / clear / help / quit")

// 修正後:
a.cmdresult.Append("commands: submit / interactive [--repo=PATH] / cancel <id> / prune [--before=DUR] / repo <path> / clear / help / quit")
```

`tui/app.go:613-614` 付近の offline 警告ロジックを削除（PruneAction を扱う case 内）:

```go
// 修正前:
case PruneAction:
    if v.Offline {
        a.cmdresult.Append(WarnStyle.Render("--offline is a CLI-only flag; use harness-cli prune --offline. Server-side prune skipped."))
    }
    // ... 通常の prune 実行 ...

// 修正後:
case PruneAction:
    // ... 通常の prune 実行 ...
```

実際の `case PruneAction` ブロックの中身は spec のままで問題なし（offline 警告だけが消える）。

- [ ] **Step 3: ビルドとテスト**

Run:
```sh
go build ./... && go test ./tui/...
```

Expected: pass. tui のテストが `Offline` フィールドを参照していたら追従修正が必要だが、`tui/cmdline_test.go` がもしあれば該当テストの `Offline:` 行を削除する。

- [ ] **Step 4: commit**

```sh
git add tui/cmdline.go tui/app.go
git commit -m "$(cat <<'EOF'
tui: --offline 関連 dead code を削除

PruneAction.Offline フィールドと flag 定義、help text の [--offline]、
offline 警告ロジックを削除。tui からは server に対して offline 動作を
実行できないため、subcommand 分離 (prune と prune-local) に合わせて
dead code を整理。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: integration テスト更新

**Files:**
- Modify: `integration/e2e_test.go`

- [ ] **Step 1: `integration/e2e_test.go` のフラグ書式を更新**

ファイル内の `--server` を `--server-cid` に置換し、値も ConnectionID 文字列に合わせる。`prune` を local 削除目的で呼んでいる箇所があれば `prune-local` subcommand に置換する。

具体的な変更箇所はファイル内 grep で確認:

```sh
grep -n '"--server"\|"--server=' integration/e2e_test.go
grep -n '"prune"' integration/e2e_test.go
```

代表的な置換例:

```go
// 修正前
cmd := exec.Command("./harness-cli", "--server", "localhost:8539", "ls")

// 修正後
cmd := exec.Command("./harness-cli", "--server-cid", "ws:localhost:8539-*", "ls")
```

`prune` の用途が「ローカル worktree 削除も期待していた」場合は `prune-local` に置換。サーバ task forget 目的なら `prune` のままで OK。

- [ ] **Step 2: テスト実行**

Run:
```sh
go test ./integration/...
```

Expected: pass.

サーバ起動環境が必要なテストの場合は手元で `harness-server` を起動してから実行。CI で自動起動している場合はそちらに任せる。

- [ ] **Step 3: commit**

```sh
git add integration/e2e_test.go
git commit -m "$(cat <<'EOF'
integration: --server-cid フラグと prune-local subcommand に追従

e2e テストを ConnectionID 文字列ベースの新フラグ書式と subcommand
分離 (prune / prune-local) に合わせて更新。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: transport godoc 注釈追加

**Files:**
- Modify: `transport/websocket.go`

- [ ] **Step 1: `WebSocketEndpoint` の godoc に dead arg 注釈を追加**

`transport/websocket.go:165` 付近の `WebSocketEndpoint` 関数の上に godoc コメントを追加:

```go
// WebSocketEndpoint constructs a WebSocket-backed objproto.Endpoint in the
// requested mode. NOTE: addr is only used when mode is EndpointModeServer or
// EndpointModeMutual (it is the listen address for the embedded http.Server).
// In EndpointModeClient, addr is ignored — pass "" by convention. tlsConf is
// used for both the listen side (Server/Mutual) and the dial side (Client),
// so it is meaningful in all modes.
func WebSocketEndpoint(logger *slog.Logger, addr string, tlsConf *tls.Config, sessMode objproto.EndpointMode) (objproto.Endpoint, error) {
    rawSess := objproto.NewEndpoint(logger, sessMode)
    return WebSocketEndpointEx(rawSess, logger, addr, tlsConf, rawSess.GetSenderChannel())
}
```

`WebSocketEndpointEx` にも同様の注釈を 1 行追加すると良い:

```go
// WebSocketEndpointEx is the lower-level constructor used by dualstack and
// callers that want to share a RawEndpoint. The addr / tlsConf rules are the
// same as WebSocketEndpoint (addr ignored for Client, tlsConf used by all).
func WebSocketEndpointEx(rawSess objproto.RawEndpoint, logger *slog.Logger, addr string, tlsConf *tls.Config, sendTo <-chan *objproto.PacketData) (objproto.Endpoint, error) {
    // ... 本体は不変 ...
}
```

- [ ] **Step 2: ビルド確認**

Run:
```sh
go build ./transport/...
```

Expected: エラーなし（コメント追加のみのため当然）。

- [ ] **Step 3: commit**

```sh
git add transport/websocket.go
git commit -m "$(cat <<'EOF'
transport: WebSocketEndpoint の dead arg を godoc で明示

EndpointModeClient では addr 引数が使われない (httpServer.Addr に
渡るのみで、client は listen しない) ため "" を渡す規約として
godoc に注釈を追加。tlsConf は client モードでも dial 時の TLS 設定
として使われるため dead ではない旨も明示。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: 最終回帰確認

**Files:**
- 既存ファイル全体（変更なし、確認のみ）

- [ ] **Step 1: 全テストとビルドを通す**

Run:
```sh
go vet ./...
go build ./...
go test ./...
```

Expected: 全 pass、warning なし。

- [ ] **Step 2: 手元起動チェック (manual)**

ターミナル 1: harness-server 起動
```sh
go run ./cmd/harness-server
```

ターミナル 2: agent-runner 起動
```sh
go run ./cmd/agent-runner --server-cid 'ws:localhost:8539-*' --repo /tmp/test-repo --claude-bin claude
```

ターミナル 3: cli 各種確認
```sh
go run ./cmd/harness-cli ls
go run ./cmd/harness-cli watch  # Ctrl-C で停止
go run ./cmd/harness-cli submit --repo /tmp/test-repo --task "echo hello"
go run ./cmd/harness-cli prune-local --repo /tmp/test-repo
go run ./cmd/harness-cli prune
```

ターミナル 4: tui 確認
```sh
go run ./cmd/harness-tui
```

Expected:
- 全コマンドが ConnectionID 文字列で server に接続成功
- TUI の header に CID 文字列がそのまま表示される
- ls / watch / submit / prune / prune-local が正常動作

問題があれば原因を特定して該当 task に戻って修正。

- [ ] **Step 3: 完了確認**

すべての回帰確認 pass を確認したら本リファクタは完了。`docs/superpowers/plans/2026-04-26-peer-dial-endpoint-injection.md` の全 step が completed であることを確認。

完了。
