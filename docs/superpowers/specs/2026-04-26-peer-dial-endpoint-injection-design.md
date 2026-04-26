# peer.Dial Endpoint 引数化リファクタ設計

## 目的・背景

`peer.Dial` を呼ぶたびに内部で `transport.WebSocketEndpoint` を作って `objproto.Endpoint` を構築している現状の実装は、`objproto` 側の宣言と矛盾している。`objproto/session.go:29-40` のコメントは `Endpoint` を「process-scoped、lifetime is meant to match the owning process」と明文化しているが、実装上は `peer.Dial` が呼ばれるたびに新しい `Endpoint` がプロセス内に並立する。

このリファクタの主目的は、**`peer.Dial` を `Endpoint` 引数受け取り型に変える**ことで宣言と実装のズレを直すこと。副次的に以下を得る:

1. `peer` パッケージから `transport` パッケージへの import が消え、依存方向が整理される
2. wasm 対応時にトランスポート差し替えが透過的になる（呼び出し側が `transport.WebSocketEndpoint` か `wasmws.Endpoint` かを選んで渡すだけ）
3. テスタビリティ向上（in-memory mock `Endpoint` を渡せるようになる、ただしテスト追加自体は本リファクタ scope 外）
4. `MustParseConnectionID` の本番コード使用を解消（テスト専用の宣言を実装側で守る）
5. CLI フラグが ConnectionID 文字列入力になり、`UniqueNumber` 1111/2222 規約問題が `-*` (random) デフォルトで部分解決
6. `cli.Prune` の責務分割と CLI subcommand 整理（`prune` がサーバ task forget とローカルワークツリー削除を 1 コマンドで両方やっている現状を、`prune` と `prune-local` の独立 subcommand に分離。あわせて tui の `--offline` 関連 dead code を削除）

## スコープ

含むもの:

- `peer.Dial` のシグネチャ変更
- `cli` パッケージのエントリ関数群（`Dial`, `Submit`, `List`, `Cancel`, `Prune`, `PruneTasks`, `Logs`, `Watch`, `Interactive`, `get_log` 系）の引数型変更
- `cli.Prune` の責務縮小（サーバ task forget のみ、ローカルワークツリー削除を `cli.PruneLocal` に切り出し）
- `harness-cli` の subcommand を `prune`（サーバ forget）と `prune-local`（worktree 削除）に分離
- tui の `--offline` 関連 dead code 削除（`tui/cmdline.go` の `PruneAction.Offline` と flag 定義、`tui/app.go` の help text と offline 警告ロジック）
- `runner.Config.ServerAddr` → `runner.Config.ServerCID` への型変更
- `cmd/harness-cli/main.go`, `cmd/agent-runner/main.go`, `cmd/harness-tui/main.go` の CLI フラグ `--server` → `--server-cid` 変更
- 既存テストのシグネチャ追従更新（`cli/prune_test.go`, `integration/e2e_test.go`）
- `transport.WebSocketEndpoint` 呼び出しの `addr` に空文字 `""` を渡す規約の確立（godoc に注釈追加）

含まないもの:

- `transport.WebSocketEndpoint` の関数分割（`WebSocketClientEndpoint` / `WebSocketServerEndpoint` / `WebSocketMutualEndpoint`）→ wasm 対応フェーズで併せて検討
- TLS / wss 対応の CLI フラグ追加 → 別タスク
- `server/server.go`, `transport/dualstack.go` の変更（dead arg `addr` の整理は本リファクタの主目的ではない）
- 新規 mock Endpoint テストの追加（テスタビリティ向上は副次効果として残るが、テスト追加は別タスク）

## 設計

### 全体アーキテクチャ

リファクタ前:

```
cli main / runner main
  └── peer.Dial(ctx, DialConfig{Addr, UniqueNumber, Logger, PingInterval})
        └── transport.WebSocketEndpoint(logger, addr, nil, EndpointModeClient)
        └── MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:%s-%d", port, uniq))
        └── objproto.DoECDHHandshake(...)
        └── trsf.NewStreams(...)
```

リファクタ後:

```
cli main / runner main
  └── peerCID = objproto.ParseConnectionID(flagValue,
                  ParseOption_AllowRandomID | ParseOption_ResolveAddr)
  └── ep = transport.WebSocketEndpoint(logger, "", nil, EndpointModeClient)
  └── peer.Dial(ctx, ep, peerCID, DialConfig{Logger, PingInterval})
        └── objproto.DoECDHHandshake(ctx, ep, peerCID, ...)
        └── trsf.NewStreams(...)
```

責務の境界:

- **cli/runner main**: ConnectionID 文字列の入力受け取り、`ParseConnectionID`、Endpoint 構築、`peer.Dial` 呼び出し
- **peer**: `Endpoint` と `peerCID` を受け取り、ECDH + trsf + AutoSend/Ping を立てる（`transport` パッケージへの import が消える）
- **transport**: 現状のまま（dead arg `addr` は残るが、呼び出し側で `""` を渡す規約）

### peer 側の詳細

新シグネチャ:

```go
// peer/conn.go
func Dial(ctx context.Context, ep objproto.Endpoint, peerCID objproto.ConnectionID, cfg DialConfig) (*Conn, error)

type DialConfig struct {
    Logger       *slog.Logger    // nil なら slog.Default()
    PingInterval time.Duration   // <=0 なら 30s
}
```

`DialConfig` から消えるフィールド: `Addr`, `UniqueNumber`。両方とも呼び出し側の Endpoint 構築 + ConnectionID parse に責務が移る。

`peer/conn.go` の `Dial` 実装変更点:

```go
func Dial(ctx context.Context, ep objproto.Endpoint, peerCID objproto.ConnectionID, cfg DialConfig) (*Conn, error) {
    if cfg.Logger == nil { cfg.Logger = slog.Default() }
    if cfg.PingInterval <= 0 { cfg.PingInterval = 30 * time.Second }

    // 削除: transport.WebSocketEndpoint 呼び出し
    // 削除: cidStr := fmt.Sprintf("ws:127.0.0.1:%s-%d", port, UniqueNumber)
    // 削除: portFrom helper

    conn, err := objproto.DoECDHHandshake(ctx, ep, peerCID, ecdh.P521(), AES128GCM)
    if err != nil { return nil, fmt.Errorf("ecdh: %w", err) }

    p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)

    c := &Conn{
        conn: conn, trans: p, pub: pubsub.NewClient(),
        log: cfg.Logger, done: make(chan struct{}),
        pubTopics: map[string]*pubTopic{},
    }
    go trsf.AutoSend(ctx, p, conn, nil)
    go trsf.AutoPing(ctx, conn, cfg.PingInterval)
    return c, nil
}
```

import の整理:

`peer/conn.go` から `"github.com/on-keyday/agent-harness/transport"` import が削除される。peer は `objproto` / `pubsub` / `trsf` / `trsf/wire` のみに依存。

`Close` のコメント更新:

`peer/conn.go:152-158` の Close コメントは Endpoint をそもそも内部で作らなくなるので「Endpoint をここでは破棄しない（呼び出し側が所有）」という形に文言を整える。

`portFrom` helper の削除:

`peer/conn.go:202-211` の `portFrom` は ConnectionID 構築の hack でしか使っていないので、関数ごと削除。

### cli パッケージ API 変更

影響を受けるエントリ関数（10 箇所前後）の引数 `addr string` を `peerCID objproto.ConnectionID` に変更:

- `cli.Dial(ctx, peerCID)`
- `cli.Submit(ctx, peerCID, repo, prompt)`
- `cli.List(ctx, peerCID, out)`
- `cli.Cancel(ctx, peerCID, taskIDHex)`
- `cli.Prune(ctx, peerCID, before, out)` (`repo` 引数は削除、責務縮小)
- `cli.PruneLocal(ctx, repo, before, out)` (新規、`peerCID` 不要、ローカルワークツリーのみ)
- `cli.PruneTasks(ctx, peerCID, cutoff)` (現状のまま、`cli.Prune` の内部 helper)
- `cli.Logs(ctx, peerCID, taskID, out)`
- `cli.Watch(ctx, peerCID, out)`
- `cli.Interactive(ctx, peerCID, repo)`
- `cli.GetTaskLog(ctx, peerCID, taskIDHex)` (`cli/get_log.go`)

`cli/get_log.go` 内の `waitForReceiveStream` は内部 helper のため変更不要。

各関数内で `cli.Dial(ctx, peerCID)` を呼ぶ。`cli.Dial` 内で `transport.WebSocketEndpoint(slog.Default(), "", nil, objproto.EndpointModeClient)` を呼んで Endpoint を作り、`peer.Dial(ctx, ep, peerCID, peer.DialConfig{Logger: slog.Default()})` する。

`cli.Prune` の責務分割:

現状の `cli.Prune` は **Step 1: サーバ task forget RPC + Step 2: ローカルワークツリー削除** を 1 関数で両方やっている (`cli/prune.go:20-62`)。これを 2 関数に分離して責務を単一化する:

- `cli.Prune(ctx, peerCID, before, out) error` — サーバに `PruneTasks` RPC を投げて task を forget するだけ（旧 Step 1）。`repo` 引数は不要になる
- `cli.PruneLocal(ctx, repo, before, out) error` — `<repo>/.harness-worktrees/` を walk して `git worktree remove --force` するだけ（旧 Step 2）。サーバ通信なし、ConnectionID 不要

これにより `cli.PruneTasks` は `cli.Prune` の内部呼び出し helper として残す（または `cli.Prune` 内部にインライン化）。`cmd/harness-cli/main.go` でも 2 つの subcommand `prune` / `prune-local` から呼び分ける。

副次的影響: 既存 `harness-cli prune` は worktree 削除をしなくなる。両方やりたい場合は `prune-local` と `prune` を別々に実行する形（外部ユーザは存在しないので影響範囲は作者本人のみ）。

import の追加:

`cli/*.go` に `"github.com/on-keyday/agent-harness/objproto"` と `"github.com/on-keyday/agent-harness/transport"` の import が増える。

### cmd/main.go と runner.Config の変更

`cmd/harness-cli/main.go`:

```go
serverCID := flag.String("server-cid", "ws:localhost:8539-*",
    "server ConnectionID (e.g. ws:host:port-id, * for random)")
flag.Usage = usage
flag.Parse()
// ...
sub := flag.Arg(0)
args := flag.Args()[1:]
ctx := context.Background()

// 各 subcommand 内で必要に応じて呼ぶ parse helper
parseCID := func() objproto.ConnectionID {
    peerCID, err := objproto.ParseConnectionID(*serverCID,
        objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr)
    if err != nil { die(fmt.Errorf("server-cid: %w", err)) }
    return peerCID
}

switch sub {
case "submit": cli.Submit(ctx, parseCID(), abs, *task)
case "ls":     cli.List(ctx, parseCID(), os.Stdout)
case "cancel": cli.Cancel(ctx, parseCID(), args[0])
case "prune":
    fs := flag.NewFlagSet("prune", flag.ExitOnError)
    before := fs.Duration("before", 7*24*time.Hour, "forget terminal tasks older than this")
    fs.Parse(args)
    cli.Prune(ctx, parseCID(), *before, os.Stdout)
case "prune-local":
    fs := flag.NewFlagSet("prune-local", flag.ExitOnError)
    repo := fs.String("repo", ".", "repo to prune")
    before := fs.Duration("before", 7*24*time.Hour, "remove worktrees older than this")
    fs.Parse(args)
    abs, err := filepath.Abs(*repo); if err != nil { die(err) }
    cli.PruneLocal(ctx, abs, *before, os.Stdout)  // peerCID 不要
case "logs":        cli.Logs(ctx, parseCID(), args[0], os.Stdout)
case "watch":       cli.Watch(ctx, parseCID(), os.Stdout)
case "interactive": cli.Interactive(ctx, parseCID(), abs)
}
```

`prune-local` は ConnectionID を受け取らないので `parseCID()` を呼ばない。これにより `--server-cid` の値が不正でも `prune-local` は動く（ローカル動作のみのため）。

usage テキストも更新:

```go
fmt.Fprintln(os.Stderr, "  prune [--before DUR]                forget terminal tasks on the server")
fmt.Fprintln(os.Stderr, "  prune-local [--repo PATH] [--before DUR]  remove old worktrees in <repo>/.harness-worktrees/")
```

`cmd/agent-runner/main.go`:

```go
var (
    serverCID  = flag.String("server-cid", "ws:localhost:8539-*", "server ConnectionID")
    repo       = flag.String("repo", ".", "absolute path to the repo this runner serves")
    claudeBin  = flag.String("claude-bin", "claude", "path to the claude binary")
    claudeArgs = flag.String("claude-args", "", "extra args passed to claude before -p")
)

func main() {
    flag.Parse()
    peerCID, err := objproto.ParseConnectionID(*serverCID,
        objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr)
    if err != nil { /* die */ }
    // ...
    runner.Run(ctx, runner.Config{
        ServerCID:       peerCID,
        RepoPath:        abs,
        ClaudeBin:       *claudeBin,
        ExtraClaudeArgs: strings.Fields(*claudeArgs),
        Logger:          slog.Default(),
    })
}
```

`runner.Config`:

```go
type Config struct {
    ServerCID       objproto.ConnectionID  // was: ServerAddr string
    RepoPath        string
    ClaudeBin       string
    ExtraClaudeArgs []string
    Logger          *slog.Logger
}
```

`runner/connect.go:33-38` の `peer.Dial` 呼び出しは `cli.Dial` と同じく Endpoint 構築 + `peer.Dial(ctx, ep, cfg.ServerCID, ...)` の形に変更。

`cmd/harness-tui/main.go`:

```go
var (
    serverCID = flag.String("server-cid", "ws:localhost:8539-*", "harness-server ConnectionID")
    repoFlag  = flag.String("repo", ".", "default repo path for submit popup")
)

func main() {
    flag.Parse()
    peerCID, err := objproto.ParseConnectionID(*serverCID,
        objproto.ParseOption_AllowRandomID | objproto.ParseOption_ResolveAddr)
    if err != nil { /* die */ }
    // ...
    app := tui.New(tui.Config{
        Server:      *serverCID,  // 文字列のまま、表示用 (tui 側は不変)
        DefaultRepo: repoAbs,
    })
    // ...
    go func() {
        c, err := cli.Dial(ctx, peerCID)
        // ... 以下不変
    }()
}
```

`tui.Config.Server` は文字列のまま（header 表示用 `tui/app.go:442`）。`tui.New(...)` のシグネチャは変更不要。

### tui パッケージの dead code 削除

tui には現状 `--offline` フラグが定義されているが、tui からは server に対して offline 動作を実行できず、`tui/app.go:613-614` で「`--offline` is a CLI-only flag」と警告するためだけに存在している実質 dead code。subcommand 分離（`prune` / `prune-local`）に合わせて削除する:

- `tui/cmdline.go:28` `PruneAction.Offline bool` フィールド削除
- `tui/cmdline.go:144,148` flag 定義 `offline := fs.Bool("offline", ...)` と `PruneAction{... Offline: *offline}` 削除
- `tui/app.go:570` help text から `[--offline]` を削除（subcommand 体系を `prune` のみで案内、`prune-local` は CLI 専用とするか tui にも実装するかは別判断 — 現スコープでは tui には追加しない）
- `tui/app.go:613-614` `if v.Offline { ... }` 警告ロジック削除

tui で `prune-local` 相当を提供するかは別判断。現スコープでは tui の prune は server forget のみ（CLI の `harness-cli prune` と同じ挙動）に統一し、ローカルワークツリー削除が必要なら `harness-cli prune-local` を別途使うことにする。

### parse オプションの選択

`objproto.ParseConnectionID` には既に `ParseOption_AllowRandomID` と `ParseOption_ResolveAddr` のフラグが定義されている。本リファクタでは両方を立てる:

- `AllowRandomID`: デフォルト値の `-*` をランダム ID として展開する。`UniqueNumber` の 1111/2222 固定規約問題に対する部分解決
- `ResolveAddr`: `localhost:8539` のような hostname を DNS resolve で受け付け、利便性を上げる

明示で `-1234` のように固定 ID も指定可能（既存挙動を完全再現したいケース）。

### 副次影響: 1111/2222 依存ロジックの確認

`-*` ランダム ID をデフォルトにすることで、server 側のコードが `cid.ID == 1111` / `cid.ID == 2222` で runner/cli を判別している場合に壊れる。実装フェーズの最初のタスクとして:

```sh
grep -rn "1111\|2222\|UniqueNumber" --include='*.go'
```

で依存箇所を洗う。依存があれば、役割識別を別経路（Hello メッセージ等）に移すサブタスクを追加するか、デフォルト値を固定 ID `-1111` / `-2222` に戻す判断点とする。

### dead arg の扱い

`transport.WebSocketEndpoint(logger, addr, tlsConf, mode)` の `addr` 引数は `EndpointModeClient` 経路では `httpServer.Addr` にしか使われず実質 dead。本リファクタでは関数分割せず、呼び出し側で `""` を渡す規約とする。`transport/websocket.go` の godoc コメントに「`addr` is ignored when mode is Client」を 1 行追加して注意喚起する。

`tlsConf` は client モードでも `websocket.DialConfig.TlsConfig` で dial 時の TLS 設定として使われるため dead ではない。

## テスト方針

### 既存テストの更新

| ファイル | 変更内容 |
|---|---|
| `cli/prune_test.go:45,55` | `Prune(ctx, "", repo, ...)` → `PruneLocal(ctx, repo, ...)` |
| `cli/logs_test.go` | 中身が `t.Skip` のみ、変更不要 |
| `cli/watch_test.go` | 同上、変更不要 |
| `integration/e2e_test.go` | 起動コマンドの `--server` を `--server-cid` に変更（書式は実装時に確認） |

### 新規テストの追加

本リファクタ scope 外。テスタビリティ向上は副次効果として残るが、in-memory mock `Endpoint` を渡せるようになった API を活用するテスト追加は別タスク。

### 回帰確認のチェックリスト

- `go test ./...` が pass
- 手元起動: `harness-server` 起動 → `harness-cli ls` / `harness-cli submit --task "x"` が動く
- TUI 起動: `harness-tui` が server に接続、header の `harness-tui · %s · ...` に CID 文字列がそのまま表示される
- runner 起動: `agent-runner --server-cid ws:localhost:8539-*` で server に Hello が届く

## 影響範囲

### 変更するファイル

| ファイル | 主な変更 |
|---|---|
| `peer/conn.go` | `Dial` シグネチャ変更、`DialConfig` 縮小、`transport` import 除去、`portFrom` 削除、Close コメント更新 |
| `cli/client.go` | `Dial` 型変更、Endpoint 構築コード追加 |
| `cli/submit.go`, `list.go`, `cancel.go`, `logs.go`, `watch.go`, `open_interactive.go` | 引数 `addr string` → `peerCID objproto.ConnectionID` |
| `cli/get_log.go` | `GetTaskLog` の引数 `addr string` → `peerCID objproto.ConnectionID` (`waitForReceiveStream` は不変) |
| `cli/prune.go` | `Prune` の責務縮小（worktree 削除を切り出し、サーバ task forget のみに）、`PruneLocal` 関数を新設、引数型変更 |
| `runner/connect.go` | `Config.ServerAddr` → `Config.ServerCID`、Endpoint 構築コード追加、`peer.Dial` 呼び出し変更 |
| `cmd/harness-cli/main.go` | `--server` → `--server-cid`、`parseCID` helper による subcommand-local parse、`prune` / `prune-local` subcommand 分離、usage テキスト更新 |
| `cmd/agent-runner/main.go` | `--server` → `--server-cid`、`runner.Config.ServerCID` 渡す |
| `cmd/harness-tui/main.go` | `--server` → `--server-cid`、`tui.Config.Server` には CID 文字列をそのまま渡す |
| `tui/cmdline.go` | `PruneAction.Offline` フィールドと `--offline` flag 削除 |
| `tui/app.go` | help text の `[--offline]` 削除、offline 警告ロジック削除 |
| `cli/prune_test.go` | `Prune(ctx, "", repo, ...)` → `PruneLocal(ctx, repo, ...)` |
| `integration/e2e_test.go` | フラグ書式変更 |
| `transport/websocket.go` | godoc コメントに dead arg 注釈を 1 行追加（関数本体・シグネチャは不変） |

### 触らないファイル

`transport/dualstack.go`, `transport/udp*.go`, `transport/websocket_*.go` 系プラットフォーム別ファイル, `server/server.go`, `objproto/*.go`, `tui/` のうち `cmdline.go` と `app.go` 以外。`transport/websocket.go` も実装ロジックは触らずコメントのみ。

## 移行・後方互換

外部ユーザは存在せず作者本人のみが使用しているため、後方互換の懸念は最小。本リファクタには以下の CLI 表面変更が含まれるが、移行ガイド等は不要:

- `--server` → `--server-cid` のフラグ名変更（go の flag パッケージは unknown flag でエラー終了するので、誤用は静かに動くのではなく明示的に失敗する）
- `--server-cid` のデフォルト値 `"ws:localhost:8539-*"` で、引数なしの起動は host:port が同じ・ID が random という挙動になる
- `harness-cli prune` の挙動変更: 旧来の「サーバ forget + ローカル削除」両方から「サーバ forget のみ」に変更。ローカル削除は新 subcommand `harness-cli prune-local` で別途実行
- tui の `prune` コマンド: `--offline` flag を削除（dead code だったため）

## リスク・注意点

- **server 側 1111/2222 依存**: 実装初期の grep で発見されたら、Hello メッセージ等で役割識別する別経路を作るサブタスクが追加される。発見時の対処方針はその時点で判断
- **wasm 対応との連動**: 本リファクタが完了すると `peer/conn.go` から `transport` import が消えているので、wasm 対応では `cmd/harness-web` を作って `transport/wasmws` を別建てし `peer.Dial(ctx, ep, peerCID, cfg)` を呼ぶだけで済む状態になる
- **dead arg `addr`**: `transport.WebSocketEndpoint(logger, "", nil, EndpointModeClient)` の `""` は規約として残る。godoc に注釈を追加して将来の読み手の認知負荷を下げる
- **DNS resolve の失敗**: `ParseOption_ResolveAddr` を立てると hostname 解決時に DNS lookup が走る。lookup 失敗時はエラーで即座に die（cmd 層で起動失敗）。wasm 対応時は DNS lookup が使えないため、wasm エントリポイントでは別フラグを立てない（IP literal のみ受け付け）

## 実装順序

writing-plans フェーズで詳細化するが、想定する大筋:

1. **依存調査**: `grep -rn "1111\|2222\|UniqueNumber" --include='*.go'` で server 側の固定 ID 依存を洗う
2. **peer 変更**: `peer/conn.go` のシグネチャ変更（Endpoint 構築を内部から除去、`Dial` 引数変更）
3. **cli 変更**: `cli/*.go` の引数型変更、`cli.Prune` の責務縮小と `cli.PruneLocal` 分離、`cli.Dial` の Endpoint 構築追加
4. **runner 変更**: `runner/connect.go` 同様
5. **cmd 変更**: `cmd/harness-cli/main.go` のフラグ・subcommand 体系（`prune` / `prune-local` 分離）、`cmd/agent-runner/main.go` と `cmd/harness-tui/main.go` のフラグ
6. **tui dead code 削除**: `tui/cmdline.go` の `PruneAction.Offline` と flag 定義、`tui/app.go` の help text と offline 警告ロジック
7. **テスト更新**: `cli/prune_test.go`（`PruneLocal` 呼び出しに変更）、`integration/e2e_test.go`（フラグ書式と `prune-local` subcommand 対応）
8. **transport godoc**: dead arg 注釈の追加
9. **回帰確認**: `go test ./...` と手元起動
