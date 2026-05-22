# Server-mode runner: reverse-dial + agent proxy

Date: 2026-05-22
Status: Design

## Goals

ACL によって runner → server 方向の TCP connect が遮断されるネットワーク環境で、
harness を稼働可能にする。許容する方向は server → runner のみ。

具体的に解く問題:
1. **Runner registration leg**: 現状 runner が `peer.Dial(server)` で登録 → ACL で SYN ブロック。
2. **Agent leg**: claude-code hook 経由で呼ばれる `harness-cli agent {send,wait,...}` は
   毎回 `peer.Dial(server)` で新 outbound conn を張る → 同じく ACL でブロック。
   さらに `harness-cli` のあらゆるサブコマンド (`submit`, `file *`, `logs`, ...) も
   runner host 上で実行されれば同様にブロックされる。

両方を「runner 側ホストから新規 outbound を一切張らない」状態で動かす。

## Non-goals

- Client → server レグの方向反転 (現状 client が dial する形のままで ACL 環境にも適合)。
- Multi-server (1 runner が同時に複数 server と reverse-dial で繋がる) — 当面 1 runner ↔ 1 server。
- Auto-rediscover / persistent retry on the server side — `harness-cli server dial-runner`
  は一発撃ち、conn が切れたら admin が再 trigger する。
- Server 自身が ACL 制約下にあるケース (server も他から呼ばれる必要があるシナリオ)。

## トポロジー

### 現状
```
client ──dial──> server <──dial── runner
                   ^
                   └──dial─── agent process (in runner host)
```
`runner host → server` 方向の 2 本の新規 outbound が ACL でブロックされる。

### 提案
```
client ──dial──> server ──dial──> runner (listening)
                    │
                    └── 既存 peer.Conn 上を agent traffic も走る (objproto negotiated proxy)
                            ↑
                            └── agent ──dial──> runner (localhost WS) ──[SetProxy]──> server
```

ポイント:
- L4 outbound はすべて server 側から張る (server → runner)。
- 一度 conn が確立すれば peer.Conn は両方向データを流せるので、PSK・Hello・タスク制御
  などのアプリ層方向は **すべて現状維持** (runner→server に Hello、server→runner に
  task assignment、など)。
- Agent leg は objproto の既存 (未使用) proxy API で、runner を透過 packet relay
  として使う。Agent ↔ server で end-to-end ECDH + PSK + AuthTicket。Runner は復号せず。

## Phase 分割

Spec は 1 本にまとめるが (schema 一括決定 / no-split-schemas memory)、実装プランは
独立に 2 本に分ける:

- **Phase A**: Runner registration leg だけ reverse-dial 化。動作確認は ACL 不要環境でも可。
- **Phase B**: Agent leg を runner-proxy 化。Phase A の上に乗る。

Phase A 単体でも動くが、ACL 環境では agent コマンドが死ぬ。Phase B まで揃って初めて
ACL 環境で実用化。

---

## Phase A: Runner reverse-dial registration

### 変更点サマリ

| Component | 変更 |
|---|---|
| harness-server | endpoint mode を `Server` → **`Mutual` (無条件)** |
| harness-server | 新 control message `DialRunnerRequest` を client→server WS で受領 → `peer.Dial(target)` |
| agent-runner | 新フラグ `--listen` (WS) / `--udp-listen` (UDP)。`--server-cid` と排他。両方与えると dualstack。Mutual mode で endpoint 起動。命名は `cmd/harness-server/main.go:24-25` に合わせる |
| agent-runner | 起動時に listen addr を log 出力 |
| harness-cli | 新サブコマンド `harness-cli server dial-runner <runner-cid>` (既存 `--server-cid` と同じ CID 書式、uniqueid は `-*` でランダム指定可) |
| PSK 方向 | 不変 — runner が PSK 送出、server が validate。Reverse-dial で listener 側になっても runner 自身が `SendAndWaitPSK` を駆動する |
| Hello 方向 | 不変 — runner→Hello→server、server→HelloResponse→runner |

### Wire schema 追加 (`runner/protocol/message.bgn`)

```bgn
# admin → server の制御メッセージ。既存 ClientHello 系の親類に追加する位置付け。
format DialRunnerRequest:
    target :RunnerID         # runner 側 listen endpoint。harness-cli の引数で
                             # 受け取った CID 文字列を ParseConnectionID で構築
                             # (`-*` でランダム uniqueid 指定可、既存 server-cid と同規約)。
                             # ip_addr_len は IPv4 placeholder 規約に従う
                             # ([[project_runnerid_constraint]]).

enum DialRunnerStatus:
    :u8
    Ok
    DialFailed       # transport-level dial failed (timeout, conn refused, etc.)
    PskFailed        # ECDH ok but PSK validation failed
    HelloTimeout     # PSK ok but no Hello arrived within timeout
    InvalidTarget    # target RunnerID malformed (empty transport, etc.)

format DialRunnerResponse:
    status :DialRunnerStatus
```

`DialRunnerRequest` / `DialRunnerResponse` は既存 `ClientRequest` enum の一エントリ
として追加 (詳細 enum 値割り当ては実装で確定)。

### Flow

```
admin host (harness-cli)        harness-server               agent-runner (--listen)
       │                              │                            │
       │  (already connected via      │                            │  (listening,
       │   normal client→server WS)   │                            │   not yet dialed)
       │                              │                            │
       │ DialRunnerRequest{target}    │                            │
       ├─────────────────────────────►│                            │
       │                              │                            │
       │                              │ peer.Dial(target.toCID)    │
       │                              ├───────────────────────────►│ (TCP SYN inbound to runner)
       │                              │                            │ accept, ECDH handshake
       │                              │◄───────────────────────────┤
       │                              │                            │
       │                              │           (PSK)            │
       │                              │◄───────────────────────────┤ SendAndWaitPSK
       │                              │  pskGate.Check / reply     │
       │                              ├───────────────────────────►│
       │                              │                            │
       │                              │          (Hello)           │
       │                              │◄───────────────────────────┤ RunnerHello{hostname, roots, ...}
       │                              │ RunnerHelloResponse        │
       │                              ├───────────────────────────►│
       │                              │                            │
       │                              │ -> registry に追加         │
       │                              │                            │
       │ DialRunnerResponse{Ok}       │                            │
       │◄─────────────────────────────┤                            │
```

### CLI 表面

```
agent-runner [--listen <host:port>] [--udp-listen <host:port>] \
             [--roots ...] [--hostname ...] [--max-tasks N] [other flags 同様]
  # --listen: WebSocket listen (`cmd/harness-server/main.go` と同じ規約)
  # --udp-listen: UDP listen (同様)
  # 両方与えると dualstack (UDPWebsocketDualStackEndpoint を使う)
  # 少なくともどちらか一方は必須。--server-cid と排他。
  # 起動時 log:
  #   runner listen ws=0.0.0.0:8540 udp=0.0.0.0:8541
  # (CID は server が dial してきた時点で uniqueid 含めて確定する。
  #  起動時点では addr だけわかれば十分。)
```

```
harness-cli server dial-runner <runner-cid> [--server-cid <server-cid>]
  # runner-cid: 既存 --server-cid と同じ書式 (例: ws:192.168.3.10:8540-*, udp:192.168.3.10:8541-*)
  # --server-cid は既存の HARNESS_SERVER_CID env と同じ解決ルール (admin が CLI を打つ先)。
  # stdout: "ok" or "error: <DialRunnerStatus>"
```

### 既存コード変更箇所

- `cmd/harness-server/main.go`: `objproto.EndpointModeServer` → `EndpointModeMutual` に。
- `server/server.go`: `DialRunnerRequest` 受領ハンドラ追加。`peer.Dial(target)` を
  既存 endpoint 上で呼ぶ (Mutual なので可能)。Conn 確立後は既存 accept パスと
  同じ handler に流す。
- `cmd/agent-runner/main.go`: `--listen` フラグ。設定時は `runner.Connect` の代わりに
  新しい `runner.Listen` 系関数を呼ぶ (or `Connect` を listen 対応に拡張)。
- `runner/connect.go` (or 新 `runner/listen.go`): `peer.Dial` の代わりに endpoint の
  `GetNewActiveConnectionChannel()` から conn を待つ。確立後の PSK送出 / Hello送出 /
  message dispatch ループは現状のコードをほぼそのまま流用。
- `cmd/harness-cli/main.go`: `server` サブコマンドの下に `dial-runner` を追加。

---

## Phase B: Agent leg = objproto negotiated proxy

### 原理

objproto には既に `Endpoint.SetProxy` / `Connection.RehandshakeForProxy` という
「中間ホストを raw packet relay として使う」API が実装されており (ksdk から
継承、harness 側に caller 無し)、これを使う。

データプレーンの動作は `objproto/objproto.go:1018-1038`:
```go
proxyTo, exists := s.proxySettings[cid]
if exists {
    peer := proxyTo.getPeer(cid)
    s.sendPacket(peer, pkt.Header.Kind, data)  // raw bytes
    return
}
```
受信パケットの src CID が `proxySettings` にヒットしたら、ペアの CID の addr に
**復号せずそのまま** 転送する。

ACL 整合性: `transport/websocket.go:126` の `connMap.Get(pkt.To.Addr)` で送信は
**addr ごとに既存 WS conn を再利用**する。つまり runner↔server の既存 conn
(Phase A の reverse-dial で確立済み) が生きていれば、proxy forward 時に新規 outbound
を張らない。これが本設計の ACL 整合性の根拠。

### Wire schema 追加 (agent ↔ runner 区間のみ)

```bgn
# 追加: agent → runner の最初のアプリメッセージ。
format ProxyRequest:
    task_id :TaskID

enum ProxyEstablishStatus:
    :u8
    Ok
    IdCollision         # agent が選んだ connection_id が runner の server-conn と衝突。retry。
    ServerNotConnected  # runner が server と未接続。Phase A の再 trigger が必要。
    UnknownTask         # task_id が runner の current task と一致しない。

format ProxyEstablishResponse:
    status :ProxyEstablishStatus
```

**Server-side schema 変更: 無し**。
Server は proxied agent conn を「自身に dial してきた普通の agent conn」として
受ける (`server/server.go:457` の `GetNewActiveConnectionChannel` ループに流れる)。
最初の `AgentBridgeHello` で agent identity / task_id / auth_ticket を validate。

### ceremony (ksdk 時代の `TestWebSocketNegotiatedProxy` 準拠)

```
agent (cli/agent)            runner                                  server
   │                            │                                       │
   │                            │ (Phase A で確立済みの conn)            │
   │                            │◄══════════════════════════════════════┤
   │                            │                                       │
   │ peer.Dial(runner.Addr,     │                                       │
   │   CID=(ws, runner, X))     │                                       │
   ├───────────────────────────►│ accept, ECDH (agent↔runner key)        │
   │ localConn 確立              │                                       │
   │                            │                                       │
   │ ProxyRequest{task_id}      │                                       │
   ├───────────────────────────►│                                       │
   │                            │ task_id check                          │
   │                            │ CID collision check (X != runner's server-conn ID) │
   │                            │                                       │
   │                            │ (1) SetProxy(                          │
   │                            │       (ws, agent.Addr, X),  # owned    │
   │                            │       (ws, server.Addr, X), # allocate │
   │                            │     )                                  │
   │                            │                                       │
   │ (2) ProxyEstablishResponse │                                       │
   │     {Ok}                   │                                       │
   │◄───────────────────────────┤                                       │
   │                            │ (3) peerConn.Close()                   │
   │                            │     (activeConn 削除、transport conn は│
   │                            │      connMap に残るので proxy forward  │
   │                            │      続行)                             │
   │                            │                                       │
   │ RehandshakeForProxy(       │                                       │
   │   newKey, newHS)           │                                       │
   ├──packet (CID=X)───────────►│ proxy table hit                        │
   │                            ├──packet (CID=X) re-addr to server────►│ ECDH (agent's pubkey)
   │                            │                                       │ → 新 activeConn
   │                            │                                       │   at (ws, runner.Addr, X)
   │                            │◄──packet (CID=X) HandshakeAck──────────┤
   │◄──packet (CID=X)───────────┤ proxy table hit, re-addr to agent      │
   │ newLocalConn (agent↔server │                                       │
   │  with end-to-end keys)     │                                       │
   │                            │                                       │
   │ PskAuth                    │                                       │
   ├──packet (proxied)─────────►├──packet (proxied)─────────────────────►│ pskGate.Check → Ok
   │◄──packet (proxied)─────────┤◄──packet (proxied)─────────────────────┤
   │                            │                                       │
   │ AgentBridgeHello{          │                                       │
   │   task_id, runner_id,      │                                       │
   │   auth_ticket}             │                                       │
   ├──packet (proxied)─────────►├──packet (proxied)─────────────────────►│ ticket validation
   │                            │                                       │ → agentConn 登録
   │◄──HelloResponse(proxied)───┤◄──HelloResponse(proxied)───────────────┤
   │                            │                                       │
   │ ... 以降 send/wait/etc 既存通り ...                                 │
```

### Transport 整合制約

`transport/dualstack.go:94` の `fanOutByTransport` は `pkt.To.Transport` で送信 leg
を振り分ける。`receive()` で agent packet を拾ったとき `proxySettings` 経由で peer CID
に転送する際、転送先 leg は **peer (allocate) CID の transport** で決まる。

→ **制約は「SetProxy allocate CID の transport = server↔runner 既存 conn の transport」のみ**。

| server↔runner | agent↔runner | proxy allocate CID の transport |
|---|---|---|
| ws / wss | (runner endpoint がサポートする任意) | `ws` / `wss` (server 側に合わせる) |
| udp | (同上) | `udp` (server 側に合わせる) |

具体的には:
- `SetProxy` 呼び出し時、`objproto.NewConnectionID(server_cid.Transport, server.Addr, X)`
  と server 側の transport を引き継ぐ。これで forward は server↔runner の既存 conn
  (= 既存 connMap entry 再利用、ACL 違反なし) を必ず使う。
- Agent↔runner 側の transport は runner endpoint が何を聴いているかで決まる。
  Dualstack endpoint なら WS / UDP どちらで来ても受け、SetProxy が server 側 transport に
  揃えてくれるので自動的に正しい leg で forward される。WS-only endpoint なら agent も WS。

### SetProxy / ack / Close の順序制約

ksdk テスト `TestWebSocketNegotiatedProxy` は `ack → SetProxy → Close` の順だが、
これは race window が小さいので運良く通っているだけで、本設計では明示的に
`SetProxy → ack → Close` の順序を採用する。根拠:

| 制約 | 理由 |
|---|---|
| Close は SetProxy の **後** | `SetProxy(owned, ...)` は owned が `activeConnections` に存在することを要求。先に Close すると activeConn が消えて SetProxy が失敗する。 |
| Ack は Close の **前** | Close 後は peerConn 経由で send 不可。Agent に establish 通知を届けるには Close 前に送る必要がある。 |
| SetProxy は ack の **前** | Ack が先だと、agent が ack を受け取って即 `RehandshakeForProxy` を起動した場合、その handshake packet が runner に到着した時点で SetProxy 未設定 → 通常の handshake パスで処理され、ゴースト activeConn が agent CID に作られる。SetProxy を先に走らせれば、後続パケットは確実に proxy table 経由で server に転送される。Ack 送信自体は send 方向のため SetProxy の影響を受けない (proxy table は receive 側のみ参照)。 |

したがって順序は **`SetProxy → SendMessage(ack) → Close`** に確定。

### CID collision 回避

Agent が選ぶ connection_id `X` は uint16 (0x0000-0xffff)。Runner はその時点で自身が
server に対して持っている activeConn の connection_id を知っているので、衝突なら
即 `IdCollision` を返す。Agent 側で別 ID で 1 回だけ retry (3 回失敗なら fatal)。

衝突確率は実用上低い (1/65536) が、retry path があるので robust。

### 既存コード変更箇所

**`cli/agent/conn.go`**:
- env `HARNESS_PROXY_VIA_RUNNER=<runner-listen-addr>` が set されてたら proxy mode。
- `peer.Dial(runner.Addr, CID=random_id)` → app msg `ProxyRequest{task_id}` 送信 →
  `ProxyEstablishResponse` 待ち → status=Ok なら `localConn.RehandshakeForProxy(newKey, newHS)`
  → 新 conn を以降の PSK / AgentBridgeHello に渡す。
- 既存 `peer.Dial(server.Addr)` ルートと if-else 分岐するだけで cli/agent 内の他コードは無変更。
- `harness-cli` の **他のサブコマンドにも同じ env で peer.Dial の前段に同等の処理を挟む**
  (具体的には `cli.Dial` を `proxyDialIfNeeded` 経由に差し替える)。これで全 harness-cli が
  proxy mode に対応。

**`runner/`**:
- 新ファイル `runner/agent_proxy.go`: agent 用 endpoint listener (localhost WS、Mutual mode、
  既存 server endpoint と同一 — 同じ endpoint に server conn と agent conn が両方
  住む。`SetProxy` は同一 endpoint 内で動作するため必須)。
- `runner/connect.go` (or `listen.go`): 自身の server conn ID を保持し、衝突チェックに使う。
- 受信 `ProxyRequest` ハンドラで `SetProxy` 呼び出し → ack 送信 → 元 conn Close。
- Agent process spawn 時の env injection: `HARNESS_PROXY_VIA_RUNNER=ws://127.0.0.1:<port>` を
  追加。既存 `HARNESS_SERVER_CID` も維持 (agent から見た server.Addr は env 経由で
  必要、ただし agent コード上は proxy mode なら HARNESS_SERVER_CID の addr 部分は
  使わない、`HARNESS_PROXY_VIA_RUNNER` の addr を peer.Dial の宛先にする)。

**`server/`**: **変更なし**。

**`cmd/agent-runner/main.go`**: agent proxy listener の起動と port 公開。
`--listen` フラグの port と兼用するか、別 `--agent-proxy-listen` で分けるかは
実装時に決定 (推奨: 同 port で OK、Phase A の reverse-dial endpoint と同一)。

### Trust / 認証モデル

- **Agent↔runner 区間**: ECDH のみ (PSK なし)。localhost 限定で UID 同一なので OS 層で
  separation 担保。Worst case で localhost 別 user が proxy ceremony を真似ても、その先の
  AgentBridgeHello で `AuthTicket` (server 発行、env 経由で本物の agent にだけ注入される)
  を持っていないので server に拒否される。
- **Agent↔server 区間 (proxy 越し)**: ECDH (rehandshake で derive)、PSK、AuthTicket
  すべて end-to-end。Runner は復号できず、改竄もできない (改竄すれば AEAD で検出される)。

### Runner-server conn が落ちた時の振る舞い

Phase A の reverse-dial で確立した runner↔server conn が切れると、`connMap` から
entry が削除される。以降の agent proxy forward は target addr 未解決で失敗する
(`transport/websocket.go:168` "no websocket connection for address" → `CannotSend`)。

このとき:
1. Agent 側は send timeout / error を観測。
2. Runner は server conn lost を検知 (ping timeout)。
3. Admin が `harness-cli server dial-runner` を再 trigger するまで agent は機能しない。

→ runner-server conn の health を agent 側にも伝える機構があると親切だが、
   dogfood scope なので Phase B 範囲外。Phase A の `--server-cid` 自動再 dial も同様に範囲外。

---

## Open questions (実装時に詰める)

1. **Phase A の listen 側 PSK 駆動**: 現状の `runner.Connect` は `peer.Dial` の後すぐ
   `SendAndWaitPSK` を呼ぶ。Listen 側になっても runner 自身が PSK 送出側であることは
   不変だが、トリガーするタイミングが「conn 確立通知 (GetNewActiveConnectionChannel)」に
   変わる。コード構造上は素直に書き換え可能だが、テストカバレッジを Phase A プランで
   確認する。

2. **connection_id 衝突 retry の同時性**: 複数 agent process が同時に同じ ID を
   選ぶ可能性。Runner 側で「choose ID for agent」型 API を追加すれば retry 不要
   だが、wire 追加になる。当面は random 64-bit seed → 16-bit truncate + retry で
   許容。

3. **`--listen` / `--udp-listen` のデフォルト**: `cmd/harness-server/main.go` は WS
   loopback default + UDP empty default。Runner 側も同規約で良いか (loopback だと
   server からの reverse-dial が届かないので、`--listen 0.0.0.0:<port>` を明示的に
   要求するか、loopback default のままで admin が必ず override する運用にするか)。
   Phase A プランで確定。
