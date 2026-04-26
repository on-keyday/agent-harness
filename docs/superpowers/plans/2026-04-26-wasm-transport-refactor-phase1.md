# Phase 1: transport refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** WS transport API を `WebSocketConfig` 単一構造体 + Client/Server/Mutual 3 関数 (各 + Ex) に統一し、`http.Server` lifecycle と mux 構築を caller (`harness-server`) 所有に移す。Phase 2 (wasm 本体) の前提整備。

**Architecture:** `transport/websocket.go` を server-mode の内部 `http.Server` 所有から切り離し、caller が渡す `*http.ServeMux` に accept handler を登録する形にする。WS path は `cli.WebSocketPath` (var、デフォルト `/ws`) で harness 統合層が所有し、各 cmd/main の `--ws-path` flag で override 可能にする。`startTransportLoops` (旧 `handleRawEndpoint`) はロジック流用 + `dialPath` 引数追加。

**Tech Stack:** Go 1.25.7, 既存の `objproto` / `peer` / `transport` / `cli` / `runner` / `server` パッケージ群。新規依存なし。`Spec`: `docs/superpowers/specs/2026-04-26-wasm-transport-design.md`.

---

## Reference for implementers

### 上流前提（godoc に明示する）

`objproto.endpoint.SendHandshake` (`objproto/objproto.go:639-641`) は `EndpointModeServer` を弾くので、transport の sender loop に Handshake パケットが到達するのは Client か Mutual の endpoint 限定。`startTransportLoops` の Handshake → 新規 dial 分岐はこの上流不変条件に依存する。Server caller 経由では `dialPath` が空文字でも到達経路がないので安全。

### `objproto.RawEndpoint` の Endpoint 互換

`objproto/session.go:83-88` で `RawEndpoint` が `Endpoint` を embed している。`rawSess` をそのまま `objproto.Endpoint` として返せる（ラッパ不要）。

### import 関係（事前確認済）

- `cli` ← `runner` / `server` の片方向 import は循環なし
- 各 cmd/main は `cli`、`runner`、`server`、`transport`、`objproto` を import 可能

### 既存コード位置

- `transport/websocket.go:165-174` 旧 `WebSocketEndpoint` (logger, addr, tlsConf, sessMode)
- `transport/websocket.go:179-241` 旧 `WebSocketEndpointEx` 内部の `http.Server` spawn
- `transport/websocket.go:86-163` 旧 `handleRawEndpoint`
- `transport/websocket.go:108-161` 旧 sender loop (Handshake 分岐 = client dial)
- `transport/dualstack.go:34-65` `UDPWebsocketDualStackEndpoint` (caller ゼロの dead code)
- `cli/client.go:38` `transport.WebSocketEndpoint(...)` 旧呼び出し
- `runner/connect.go:34` `transport.WebSocketEndpoint(...)` 旧呼び出し
- `server/server.go:238` `transport.WebSocketEndpoint(...)` 旧呼び出し
- `cmd/harness-server/main.go` `port` flag → 既に Phase 0 で `--listen` flag に置換済 (commit `17d8c62`)
- `cmd/harness-cli/main.go`, `cmd/agent-runner/main.go`, `cmd/harness-tui/main.go`, `cmd/harness-server/main.go` には `--ws-path` flag 未追加

### dogfood scope の前提

利用者は 1 人。互換性の心配なし。breaking change の deprecation シムは不要、旧 `WebSocketEndpoint(Ex)` は完全削除。

---

## File structure

### Create

```
cli/path.go                           cli.WebSocketPath var (1 行)
```

### Modify

```
transport/websocket.go                WebSocketConfig + 6 関数 + startTransportLoops に書き換え
transport/dualstack.go                新 API (WebSocketServerEndpointEx 等) を呼ぶ形に追従
server/server.go                      WebSocketEndpoint 呼びを WebSocketServerEndpointEx に書き換え、mux + http.Server を所有
cli/client.go                         WebSocketClientEndpoint(WebSocketConfig{Path: cli.WebSocketPath}) 呼びに
runner/connect.go                     同上 (cli.WebSocketPath を参照)
cmd/harness-cli/main.go               --ws-path flag 追加
cmd/agent-runner/main.go              --ws-path flag 追加
cmd/harness-tui/main.go               --ws-path flag 追加
cmd/harness-server/main.go            --ws-path flag 追加 (server.New 経由で参照)
```

### 触らない

`objproto/*.go`, `peer/*.go`, `trsf/*.go`, `runner/protocol/*.go` (bgn-generated), `tui/*.go`, `cli/*.go` のうち `cli/path.go` 以外, `cmd/harness-server/main.go` の Phase 0 で入れた `--listen` flag 構造, `transport/udp.go`, `transport/websocket_*.go` 系 (本リポジトリには現状なし)。`exec/frame/frame.go` の unreachable 警告は pre-existing で本リファクタと無関係。

---

## Tasks

### Task 1: `cli/path.go` の新設

**Files:**
- Create: `cli/path.go`

- [ ] **Step 1: ファイル新規作成**

```go
package cli

// WebSocketPath is the URL path used for WebSocket endpoints across the
// harness components (cli / runner / tui / server). The transport package
// itself does not own a path convention; this var is the canonical
// harness-side default. Override at startup via the --ws-path cmd flag
// (set cli.WebSocketPath = *wsPath in main, before calling cli.Dial /
// runner.Run / server.Run).
//
// Default: "/ws"
var WebSocketPath = "/ws"
```

- [ ] **Step 2: ビルド確認**

Run:
```sh
go build ./cli/...
```

Expected: エラーなし。

- [ ] **Step 3: commit**

```sh
git add cli/path.go
git commit -m "$(cat <<'EOF'
cli: harness 統合層の WS path 規約用 var を新設

cli.WebSocketPath を 1 ファイル 1 var で導入。default "/ws"。各 cmd の
--ws-path flag (後続 task で追加) からこの var を override する形で、
transport パッケージは path 規約に意見を持たず、harness 統合層の
caller がここを所有する設計。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: transport API 構造体化 + R3 + dualstack 追従 + 全 caller 修正 (atomic refactor)

このタスクは複数ファイルにまたがる atomic な変更で、途中の中間状態ではビルドが通らない。すべての step を完了してから 1 つの commit にまとめる。

**Files:**
- Modify: `transport/websocket.go`
- Modify: `transport/dualstack.go`
- Modify: `cli/client.go`
- Modify: `runner/connect.go`
- Modify: `server/server.go`
- Modify: `cmd/harness-server/main.go`

- [ ] **Step 1: `transport/websocket.go` に `WebSocketConfig` 構造体を導入**

`transport/websocket.go` のファイル先頭近く（既存の type 宣言群と同じ位置）に追加:

```go
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
type WebSocketConfig struct {
	Logger *slog.Logger
	Path   string
	TLS    *tls.Config
}
```

- [ ] **Step 2: 旧 `handleRawEndpoint` を `startTransportLoops` にリネーム + `dialPath` 引数追加**

`transport/websocket.go:86` の `handleRawEndpoint` 関数を以下に置き換え（中身のロジックは流用、関数名と引数のみ変更、Handshake 分岐内の `Location` に `Path: dialPath` を追加）:

```go
// startTransportLoops runs two goroutines on top of rawSess:
//   - recv: pumps frames from each accepted/dialed *WebSocketConn into rawSess.Receive
//   - send: drains rawSess.GetSenderChannel and writes to the mapped conn,
//     dialing a fresh outbound connection (using dialPath) when a Handshake
//     packet targets an unknown peer.
//
// The dial branch in the send loop relies on an upstream invariant:
// objproto.endpoint.SendHandshake (objproto/objproto.go:639-641) returns an
// error for EndpointModeServer. So Handshake packets only reach this loop
// from Client or Mutual endpoints; for pure Server callers dialPath being
// empty is safe because no Handshake is ever observed.
func startTransportLoops(rawSess objproto.RawEndpoint, transportName string,
	connChan chan *WebSocketConn, connMap *connectionMap,
	senderChannel <-chan *objproto.PacketData,
	tlsConf *tls.Config, dialPath string, logger *slog.Logger) {

	go func() {
		for conn := range connChan {
			go func(c *WebSocketConn) {
				for {
					recv, err := c.Receive()
					if err != nil {
						connMap.Delete(c.remoteAddr)
						if errors.Is(err, io.EOF) {
							logger.Info("websocket connection closed by remote", slog.String("address", c.remoteAddr.String()))
						} else {
							logger.Error("failed to receive websocket message", slog.String("address", c.remoteAddr.String()), slog.String("error", err.Error()))
						}
						return
					}
					rawSess.Receive(transportName, c.remoteAddr, recv)
				}
			}(conn)
		}
	}()

	go func() {
		for pkt := range senderChannel {
			conn, ok := connMap.Get(pkt.To.Addr)
			if !ok {
				if pkt.Kind == packet.PacketKind_Handshake {
					go func() {
						wsScheme := "ws"
						httpScheme := "http"
						if tlsConf != nil {
							wsScheme = "wss"
							httpScheme = "https"
						}
						conf := &websocket.Config{
							Location: &url.URL{
								Scheme: wsScheme,
								Host:   pkt.To.Addr.String(),
								Path:   dialPath,
							},
							Origin: &url.URL{
								Scheme: httpScheme,
								Host:   pkt.To.Addr.String(),
							},
							TlsConfig: tlsConf,
							Version:   websocket.ProtocolVersionHybi13,
						}
						ws, err := websocket.DialConfig(conf)
						if err != nil {
							logger.Error("failed to dial websocket", slog.String("address", pkt.To.Addr.String()), slog.String("error", err.Error()))
							rawSess.CannotSend(pkt)
							return
						}
						conn := newWebSocketConn(ws, pkt.To.Addr, func() {})
						connMap.Set(pkt.To.Addr, conn)
						err = conn.Send(pkt.Data)
						if err != nil {
							logger.Error("failed to send websocket handshake message", slog.String("to", pkt.To.String()), slog.String("error", err.Error()))
							rawSess.CannotSend(pkt)
							connMap.Delete(pkt.To.Addr)
							return
						}
						connChan <- conn
					}()
					continue
				}
				logger.Error("no websocket connection for address", slog.String("address", pkt.To.String()))
				rawSess.CannotSend(pkt)
				continue
			}
			err := conn.Send(pkt.Data)
			if err != nil {
				logger.Error("failed to send websocket message", slog.String("to", pkt.To.String()), slog.String("error", err.Error()))
				rawSess.CannotSend(pkt)
				connMap.Delete(pkt.To.Addr)
			}
		}
	}()
}
```

- [ ] **Step 3: accept handler 生成ヘルパ `newAcceptHandler` を追加**

`transport/websocket.go` の Step 2 の関数の上か下に追加（既存 `WebSocketEndpointEx` の `if rawSess.EndpointMode() != EndpointModeClient { ... }` ブロック内の `&websocket.Server{...}` 構築ロジックを切り出した形）:

```go
// newAcceptHandler builds the http.Handler that upgrades incoming WS
// connections, registers them in connMap, and feeds them into connChan
// for the recv loop to pick up.
func newAcceptHandler(connChan chan<- *WebSocketConn, connMap *connectionMap, tlsConf *tls.Config, logger *slog.Logger) http.Handler {
	return &websocket.Server{
		Config: websocket.Config{
			TlsConfig: tlsConf,
		},
		Handshake: func(c *websocket.Config, r *http.Request) error {
			var err error
			c.Origin, err = websocket.Origin(c, r)
			if err == nil && c.Origin == nil {
				return fmt.Errorf("null origin")
			}
			return err
		},
		Handler: func(ws *websocket.Conn) {
			ctx, cancel := context.WithCancel(ws.Request().Context())
			remoteAddr, err := netip.ParseAddrPort(ws.Request().RemoteAddr)
			if err != nil {
				logger.Error("invalid remote address", slog.String("address", ws.Request().RemoteAddr))
				ws.Close()
				cancel()
				return
			}
			conn := newWebSocketConn(ws, remoteAddr, cancel)
			connMap.Set(remoteAddr, conn)
			connChan <- conn
			<-ctx.Done()
		},
	}
}
```

import に `"context"` が無ければ追加（既存ですでに import 済みか要確認）。

- [ ] **Step 4: `transportName` ヘルパを追加**

```go
// transportName returns "wss" if a TLS config is supplied, "ws" otherwise.
// Used to tag PacketData with the right transport identifier.
func transportName(tlsConf *tls.Config) string {
	if tlsConf != nil {
		return "wss"
	}
	return "ws"
}
```

- [ ] **Step 5: `WebSocketClientEndpoint` + `WebSocketClientEndpointEx` を実装**

```go
// WebSocketClientEndpoint constructs a Client-mode WebSocket-backed Endpoint.
// It dials peers (resolved from each PacketData's ConnectionID) on cfg.Path.
// The returned Endpoint is also a RawEndpoint (Endpoint is embedded).
func WebSocketClientEndpoint(cfg WebSocketConfig) (objproto.Endpoint, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, objproto.EndpointModeClient)
	if err := WebSocketClientEndpointEx(rawSess, cfg); err != nil {
		return nil, err
	}
	return rawSess, nil
}

// WebSocketClientEndpointEx is the lower-level variant that lets the caller
// share a RawEndpoint across multiple transports (e.g. dualstack).
func WebSocketClientEndpointEx(rawSess objproto.RawEndpoint, cfg WebSocketConfig) error {
	connChan := make(chan *WebSocketConn, 10)
	connMap := &connectionMap{
		connMap: make(map[netip.AddrPort]*WebSocketConn),
	}
	startTransportLoops(rawSess, transportName(cfg.TLS), connChan, connMap,
		rawSess.GetSenderChannel(), cfg.TLS, cfg.Path, cfg.Logger)
	return nil
}
```

- [ ] **Step 6: `WebSocketServerEndpoint` + `WebSocketServerEndpointEx` を実装**

```go
// WebSocketServerEndpoint constructs a Server-mode WebSocket-backed
// Endpoint. The accept handler is registered onto mux at cfg.Path; the
// caller owns the *http.Server that serves mux.
func WebSocketServerEndpoint(mux *http.ServeMux, cfg WebSocketConfig) (objproto.Endpoint, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, objproto.EndpointModeServer)
	if err := WebSocketServerEndpointEx(rawSess, mux, cfg); err != nil {
		return nil, err
	}
	return rawSess, nil
}

// WebSocketServerEndpointEx is the lower-level variant for callers that
// already own a RawEndpoint (e.g. dualstack).
func WebSocketServerEndpointEx(rawSess objproto.RawEndpoint, mux *http.ServeMux, cfg WebSocketConfig) error {
	connChan := make(chan *WebSocketConn, 10)
	connMap := &connectionMap{
		connMap: make(map[netip.AddrPort]*WebSocketConn),
	}
	mux.Handle(cfg.Path, newAcceptHandler(connChan, connMap, cfg.TLS, cfg.Logger))
	startTransportLoops(rawSess, transportName(cfg.TLS), connChan, connMap,
		rawSess.GetSenderChannel(), cfg.TLS, "" /* server: no outbound dial */, cfg.Logger)
	return nil
}
```

- [ ] **Step 7: `WebSocketMutualEndpoint` + `WebSocketMutualEndpointEx` を実装**

```go
// WebSocketMutualEndpoint constructs a Mutual-mode Endpoint that both
// accepts (at cfg.Path on mux) and dials (to peers' ConnectionIDs on
// cfg.Path). Currently no caller in this repo uses Mutual on WS; the API
// is provided for symmetry and dualstack alignment.
func WebSocketMutualEndpoint(mux *http.ServeMux, cfg WebSocketConfig) (objproto.Endpoint, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, objproto.EndpointModeMutual)
	if err := WebSocketMutualEndpointEx(rawSess, mux, cfg); err != nil {
		return nil, err
	}
	return rawSess, nil
}

func WebSocketMutualEndpointEx(rawSess objproto.RawEndpoint, mux *http.ServeMux, cfg WebSocketConfig) error {
	connChan := make(chan *WebSocketConn, 10)
	connMap := &connectionMap{
		connMap: make(map[netip.AddrPort]*WebSocketConn),
	}
	mux.Handle(cfg.Path, newAcceptHandler(connChan, connMap, cfg.TLS, cfg.Logger))
	startTransportLoops(rawSess, transportName(cfg.TLS), connChan, connMap,
		rawSess.GetSenderChannel(), cfg.TLS, cfg.Path, cfg.Logger)
	return nil
}
```

- [ ] **Step 8: 旧 `WebSocketEndpoint` / `WebSocketEndpointEx` を削除**

`transport/websocket.go` の旧 `func WebSocketEndpoint(...)` (元 line 165-174 あたり) と `func WebSocketEndpointEx(...)` (元 line 179-241 あたり) を完全に削除する。中身の logic は Step 2-7 で新 API に分配済み。

未使用 import が出たら整理（特に `context` が新 API でも使われていれば残す。`golang.org/x/net/websocket` も新 API で使われるので残す）。

- [ ] **Step 9: `cli/client.go:38` の Dial を新 API 呼びに**

`cli/client.go` の `Dial` 関数内、`transport.WebSocketEndpoint(slog.Default(), "", nil, objproto.EndpointModeClient)` を以下に置き換え:

```go
ep, err := transport.WebSocketClientEndpoint(transport.WebSocketConfig{
    Logger: slog.Default(),
    Path:   WebSocketPath,
})
if err != nil {
    return nil, fmt.Errorf("ws endpoint: %w", err)
}
```

`WebSocketPath` は同 `cli` package の package-level var (`cli/path.go` で定義済み)。same-package 参照なのでパッケージプレフィックスなし。

- [ ] **Step 10: `runner/connect.go:34` の Run を新 API 呼びに**

`runner/connect.go` の import に `"github.com/on-keyday/agent-harness/cli"` を追加。

`Run` 関数内、`transport.WebSocketEndpoint(cfg.Logger, "", nil, objproto.EndpointModeClient)` を以下に置き換え:

```go
ep, err := transport.WebSocketClientEndpoint(transport.WebSocketConfig{
    Logger: cfg.Logger,
    Path:   cli.WebSocketPath,
})
if err != nil {
    return fmt.Errorf("ws endpoint: %w", err)
}
```

- [ ] **Step 11: `server/server.go:238` の Run を mux + http.Server 所有に**

まず `server/server.go` を読み、`Run` 関数の中で `transport.WebSocketEndpoint(s.cfg.Logger, s.cfg.Addr, nil, objproto.EndpointModeServer)` を呼んでいる箇所を確認。`Addr` を listen に使っていた前提で、新形は以下:

import に以下を追加:
```go
"net/http"
"github.com/on-keyday/agent-harness/cli"
```

`Run` 関数の中の `transport.WebSocketEndpoint(...)` 呼び出しを以下に置き換え:

```go
mux := http.NewServeMux()
sess, err := transport.WebSocketServerEndpoint(mux, transport.WebSocketConfig{
    Logger: s.cfg.Logger,
    Path:   cli.WebSocketPath,
})
if err != nil {
    return fmt.Errorf("ws endpoint: %w", err)
}

httpServer := &http.Server{Addr: s.cfg.Addr, Handler: mux}
serverDone := make(chan error, 1)
go func() {
    if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
        serverDone <- err
        return
    }
    serverDone <- nil
}()
```

そして `Run` 関数の終了直前 (ctx.Done() 観測後) に shutdown を入れる:

```go
<-ctx.Done()
shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
defer shutdownCancel()
_ = httpServer.Shutdown(shutdownCtx)
<-serverDone
return ctx.Err()
```

> **注**: `server.Run` の現状の構造 (どこで `<-ctx.Done()` を待っているか、return 値の扱いがどうなっているか) は実装者が `server/server.go` を読んでから判断する。上の挿入はあくまで「mux + http.Server 所有を caller 側に持つ」基本パターンで、既存コードの error / shutdown 経路に合わせて配置を調整する。`errors` import が既存になければ追加。

- [ ] **Step 12: `cmd/harness-server/main.go` は変更不要（Step 11 で server.Run が完結するため）**

確認のみ。既存の `s := server.New(server.Config{...})` + `s.Run(ctx)` 呼びは新 API でも変わらない。Phase 2 で embed.FS を追加するときに本ファイルを再度触る予定。

> Phase 1 では `server.Config.Mux` のような新フィールドは追加しない。mux は `server.Run` の内部で生成・所有する。Phase 2 で外部から embed.FS handler を渡したくなったら、その時点で `server.Config` に `WebUIFS *embed.FS` 等を追加する想定（本 plan の対象外）。

- [ ] **Step 13: `transport/dualstack.go` を新 API に追従**

`transport/dualstack.go:34-65` の `UDPWebsocketDualStackEndpoint` を以下に置き換え:

```go
// UDPWebsocketDualStackConfig configures a UDP+WebSocket dual stack
// Endpoint that shares a single objproto RawEndpoint across both
// transports. This is template code: there are no callers in this repo,
// but the wiring pattern (one rawSess fed by two transports, sender
// channel split by pkt.To.Transport) is preserved as a reference for
// future UDP-on-harness work. If this code has bit-rotted by the time you
// need it, prefer fixing it over deleting it.
type UDPWebsocketDualStackConfig struct {
	Logger  *slog.Logger
	UDPPort uint16
	Mux     *http.ServeMux       // required for Server / Mutual modes; ignored for Client
	WS      WebSocketConfig      // Path / TLS / Logger shared with the WS leg
	Mode    objproto.EndpointMode
}

type UDPWebsocketDualStack struct {
	Endpoint objproto.Endpoint
}

func UDPWebsocketDualStackEndpoint(cfg UDPWebsocketDualStackConfig) (UDPWebsocketDualStack, error) {
	rawSess := objproto.NewEndpoint(cfg.Logger, cfg.Mode)
	udpChan := make(chan *objproto.PacketData, 100)
	wsChan := make(chan *objproto.PacketData, 100)

	if _, err := UDPEndpointEx(rawSess, cfg.Logger, cfg.UDPPort, udpChan); err != nil {
		return UDPWebsocketDualStack{}, err
	}

	switch cfg.Mode {
	case objproto.EndpointModeClient:
		// Client only dials; no mux required.
		// We use a Client endpoint over the shared rawSess but drive it via
		// the wsChan for parity with the dispatch loop below.
		// (Note: WebSocketClientEndpointEx will start its own sender loop
		// reading from rawSess.GetSenderChannel directly, NOT from wsChan.
		// In Client-only dual stack the wsChan is unused; we keep the
		// dispatch loop's "case ws/wss" branch as a no-op fallback.)
		if err := WebSocketClientEndpointEx(rawSess, cfg.WS); err != nil {
			return UDPWebsocketDualStack{}, err
		}
	case objproto.EndpointModeServer, objproto.EndpointModeMutual:
		if cfg.Mux == nil {
			return UDPWebsocketDualStack{}, fmt.Errorf("UDPWebsocketDualStackEndpoint: Mux is required for Server/Mutual mode")
		}
		var err error
		if cfg.Mode == objproto.EndpointModeServer {
			err = WebSocketServerEndpointEx(rawSess, cfg.Mux, cfg.WS)
		} else {
			err = WebSocketMutualEndpointEx(rawSess, cfg.Mux, cfg.WS)
		}
		if err != nil {
			return UDPWebsocketDualStack{}, err
		}
	default:
		return UDPWebsocketDualStack{}, fmt.Errorf("unknown EndpointMode: %v", cfg.Mode)
	}

	// Split rawSess.GetSenderChannel by pkt.To.Transport.
	// NOTE: in the current shape both UDPEndpointEx and the WS variants
	// internally read from rawSess.GetSenderChannel. The dispatch loop here
	// is preserved from the original dualstack design as a reference, but
	// when both legs are wired through Ex variants the upstream routing
	// happens at the rawSess level, not through this fan-out. Future
	// rewiring can revisit this.
	go func() {
		for pkt := range rawSess.GetSenderChannel() {
			switch pkt.To.Transport {
			case "udp":
				udpChan <- pkt
			case "ws", "wss":
				wsChan <- pkt
			default:
				cfg.Logger.Error("unsupported transport for udp-websocket session", slog.String("transport", pkt.To.Transport))
			}
		}
	}()

	return UDPWebsocketDualStack{Endpoint: rawSess}, nil
}
```

`transport/dualstack.go` の import に `"net/http"` を追加。`UDPEndpointEx` の呼び方は既存通り。

> **注**: dualstack の sender 分配ループは "原版から保存しているテンプレ"。実は新 API では `WebSocketClientEndpointEx` / `WebSocketServerEndpointEx` の中でそれぞれ `rawSess.GetSenderChannel` を直接読むので、上のループは厳密には冗長 (両者が同じ channel を競って読むことになる)。Phase 1 ではこの正確な振る舞いまで詰めず、godoc に「テンプレ／要 rewiring」を明示することで現状を許容する。テストもないので動作未保証で OK。

- [ ] **Step 14: ビルドとテスト**

Run:
```sh
go build ./...
```
Expected: エラーなし。

Run:
```sh
go vet ./...
```
Expected: `exec/frame/frame.go:248,267` の unreachable 警告のみ (pre-existing で本リファクタと無関係)。それ以外に警告が出たら修正。

Run:
```sh
go test ./...
```
Expected: 全パス (cli / pubsub / runner / runner/protocol / server / topics / tui)。

Run:
```sh
go test -tags integration ./integration/...
```
Expected: 既存通り (smoke 環境が要るので CI/手元で実行できる範囲で確認)。

- [ ] **Step 15: commit**

```sh
git add transport/websocket.go transport/dualstack.go cli/client.go runner/connect.go server/server.go
git commit -m "$(cat <<'EOF'
transport+cli+runner+server: WebSocket API を構造体化し caller-owned mux に切替

- transport.WebSocketConfig 単一構造体を導入し、Client/Server/Mutual の
  3 関数 (各 + Ex) に分離
- 旧 transport.WebSocketEndpoint(Ex) を削除。server-mode の内部 http.Server
  所有を撤去し、caller (server.Run) が mux と http.Server を所有する形に
- handleRawEndpoint を startTransportLoops にリネーム + dialPath 引数追加
- transport/dualstack.go を新 API に追従。caller 不在のまま template として
  保存
- cli.Dial / runner.Run / server.Run を新 API 呼びに更新。WS path は
  cli.WebSocketPath を共有参照

godoc に上流前提 (objproto.SendHandshake が server-mode を弾く) を明示し、
Handshake 分岐が Client/Mutual 限定で発火する根拠を残す。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: 4 main に `--ws-path` フラグを追加

**Files:**
- Modify: `cmd/harness-cli/main.go`
- Modify: `cmd/agent-runner/main.go`
- Modify: `cmd/harness-tui/main.go`
- Modify: `cmd/harness-server/main.go`

- [ ] **Step 1: `cmd/harness-cli/main.go` に `--ws-path` flag 追加**

import に既存の `flag` がある。その flag 群の中に追加:

```go
wsPath := flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
```

`flag.Parse()` の直後（既に存在する位置）に override を追加:

```go
cli.WebSocketPath = *wsPath
```

`cli` import が既存なので追加 import 不要。

- [ ] **Step 2: `cmd/agent-runner/main.go` に `--ws-path` flag 追加**

import に `"github.com/on-keyday/agent-harness/cli"` を追加。

既存の `var (...)` ブロック内に flag を追加:

```go
wsPath = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
```

`flag.Parse()` 直後に override 追加:

```go
cli.WebSocketPath = *wsPath
```

- [ ] **Step 3: `cmd/harness-tui/main.go` に `--ws-path` flag 追加**

既存の `var (...)` ブロック内に flag を追加:

```go
wsPath = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
```

`flag.Parse()` 直後に override 追加:

```go
cli.WebSocketPath = *wsPath
```

`cli` import は既存。

- [ ] **Step 4: `cmd/harness-server/main.go` に `--ws-path` flag 追加**

import に `"github.com/on-keyday/agent-harness/cli"` を追加。

既存の `var (...)` ブロック内に flag を追加:

```go
wsPath = flag.String("ws-path", "/ws", "WebSocket URL path (overrides cli.WebSocketPath)")
```

`flag.Parse()` 直後に override 追加:

```go
cli.WebSocketPath = *wsPath
```

- [ ] **Step 5: ビルドとテスト**

Run:
```sh
go build ./...
```
Expected: エラーなし。

Run:
```sh
go vet ./...
```
Expected: `exec/frame/frame.go` の pre-existing 警告のみ。

Run:
```sh
go test ./...
```
Expected: 全パス。

- [ ] **Step 6: commit**

```sh
git add cmd/harness-cli/main.go cmd/agent-runner/main.go cmd/harness-tui/main.go cmd/harness-server/main.go
git commit -m "$(cat <<'EOF'
cmd: 4 main に --ws-path フラグを追加して cli.WebSocketPath を override

harness-cli / agent-runner / harness-tui / harness-server の各 main で
--ws-path (default "/ws") を受け、flag.Parse 直後に
cli.WebSocketPath = *wsPath で上書きする。LAN 越し / proxy 越しの path
変更運用に備えた拡張点。dogfood 想定では default のままで動く。

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: 最終回帰確認

**Files:**
- 既存ファイル全体 (確認のみ、変更なし)

- [ ] **Step 1: 全体ビルド + vet + test**

Run:
```sh
go vet ./...
go build ./...
go test ./...
```
Expected: 全パス。`exec/frame/frame.go:248,267` の pre-existing unreachable 警告のみ許容。

- [ ] **Step 2: 手元 smoke**

ターミナル 1: harness-server 起動 (default path で)
```sh
go run ./cmd/harness-server
```
Expected: 起動ログ、`127.0.0.1:8539` で listen 開始。

ターミナル 2: agent-runner 起動
```sh
go run ./cmd/agent-runner --server-cid 'ws:127.0.0.1:8539-*' --repo /tmp/test-repo --claude-bin claude
```
Expected: server に接続成功、Idle 状態のログ。

ターミナル 3: cli 確認
```sh
go run ./cmd/harness-cli ls
go run ./cmd/harness-cli watch  # Ctrl-C で停止
go run ./cmd/harness-cli prune
go run ./cmd/harness-cli prune-local --repo /tmp/test-repo
```
Expected: それぞれ通常の応答が返る (server forget 0 task / worktree なし等)。

ターミナル 4: tui 確認
```sh
go run ./cmd/harness-tui
```
Expected: 起動して runner が表示される。`q` で終了。

オプションで非 default path 確認:
ターミナル 1 (再起動): `go run ./cmd/harness-server --ws-path=/api/ws`
ターミナル 2 (再起動): `go run ./cmd/agent-runner --server-cid='ws:127.0.0.1:8539-*' --ws-path=/api/ws --repo /tmp/test-repo`
ターミナル 3: `go run ./cmd/harness-cli --ws-path=/api/ws ls`

Expected: 接続成功。`/api/ws` を server / cli / runner で揃える運用が動くことを確認。

- [ ] **Step 3: 完了確認**

Phase 1 完了。`docs/superpowers/plans/2026-04-26-wasm-transport-refactor-phase1.md` の全 step が completed であることを確認し、Phase 2 plan の作成へ移る。
