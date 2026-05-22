# Server → runner via relay-runner

Date: 2026-05-23
Status: Design

## Goals

Server から target_runner に直接 (Phase A の reverse-dial を含めて) 到達できないが、
別の **既に server に registered な runner** を経由すれば到達できるネットワーク構成
で、その第三者 runner を **objproto packet relay** として使い、server↔target_runner
の peer.Conn を end-to-end で確立する。

具体例 (ACL 階層化された配置):
```
admin → server@A → proxy_runner@B   (← server から direct OK、proxy_runner は既に registered)
                         │
                         │  (proxy_runner→target は内部網で OK、
                         │   server→target は ACL で遮断)
                         ↓
                   target_runner@C
```

Admin が `harness-cli server dial-runner ws:C:port-* --via ws:B:port-*` を打つと、
target_runner が server に登録される (registry に追加される)。

## Non-goals

- **N-hop relay (3 段以上)**: target_runner が更に別 proxy_runner を経由するケースは
  本 spec のスコープ外。2-hop (server → proxy_runner → target_runner) のみ
- **Relay ACL / quota / per-target whitelist**: proxy_runner は registered であれば
  任意の target への relay を受け付ける。Dogfood scope での割り切り
- **動的な relay 切り替え**: 一度確立した relay は target_runner との conn 寿命と
  一致する。途中で別 proxy_runner に切り替えるなどは spec 外
- **Agent leg のための relay (Phase B)** との混在: Phase B は agent ↔ server の話、
  本 spec は server ↔ runner の話。独立に動く

## トポロジー

### 現状 (Phase A 完了時)
```
server ──[Phase A reverse-dial]──→ target_runner
       (server から直接 dial)
```
ACL で `server → target_runner` 不可だと Phase A だけでは届かない。

### 提案
```
server ──[既存 registered conn]──→ proxy_runner
   │                                    │
   │      [admin が --via 指定で      │
   │       relay 経由を要求]            │
   │                                    │
   │                                    ↓
   │                              proxy_runner が
   │                              target_runner に dial
   │                                    │
   ↓                                    ↓
   │ ←──[objproto.SetProxy で          │
   │     server↔target 間の              │
   │     packet relay]                  │
   │←───────────────────────────────────┘
   │
   ↓
target_runner との end-to-end peer.Conn (proxy_runner は中身復号せず)
```

ポイント:
- proxy_runner は **既に server に登録された (Phase A or 通常) 既存 runner**。新規 PSK 交換不要、信頼は登録時の PSK validation で確立済
- target_runner 側のコード変更ゼロ。Phase A の `--listen` モード runner として動作、server-dialed conn として受ける (DialGreeting 含む既存 flow)
- packet relay は objproto.SetProxy / RehandshakeForProxy で実現 (Phase B と同じ primitive)

## Phase 分割

本 spec 1 本、実装プラン 1 本 (前段の Phase A/B と独立、相互依存無し)。

---

## 設計の中核

### Auth: 「proxy_runner = 既に registered」を信頼の根拠にする

Phase B (agent leg) では agent↔runner 間に PSK 無く、localhost 信頼に依存していた。
本 spec の relay-runner では、proxy_runner は **server に既に PSK 検証通過した
peer.Conn** を持っているので、追加の認証無しでその conn 経由で relay 設定要求を
受け取って良い。新 wire-level PSK / token は不要。

### Relay 設定要求は既存 server↔proxy_runner conn 上を流れる

新しい proxy_runner への接続を server から張る必要は無い。Admin → server の
`DialRunnerRequest{target, via}` を server が受け取ったら、server は既存の registered
runner 一覧 (`s.registry`) から via 指定の runner conn を探し、その conn 上で
`EstablishRelay{target, slot_id}` を proxy_runner に送る。

### proxy_runner は target と ECDH しない — packet relay のみ

ksdk の `TestWebSocketNegotiatedProxy` パターンに従い、proxy_runner は target と
ECDH を**行わない**。SetProxy の `allocate` 側は **synthetic ConnectionID**
(`(target.Transport, target.Addr, slot_id)`) で、proxy_runner の endpoint には
対応する activeConn が存在しない。Packet は proxy_runner.receive() の
proxySettings lookup でそのまま forward されるだけ:

- Packet from server addr (CID slot_id) → proxy.receive cid=(server.Addr, slot_id) → owned 一致 → 転送先 = allocate = (target.Addr, slot_id) → proxy.sendPacket to target.Addr
- Packet from target addr (CID slot_id) → proxy.receive cid=(target.Addr, slot_id) → allocate 一致 → 転送先 = owned = (server.Addr, slot_id) → proxy.sendPacket to server.Addr

proxy_runner は target traffic を decrypt できない (target↔server で end-to-end
ECDH 後に shared key が derived される、proxy_runner はこの key 生成過程から除外
されている)。

### proxy_runner の役割は SetProxy 設定だけ

proxy_runner が `EstablishRelay` を受けたら:
1. `expectedRelays[slot_id] = target_cid` を session state に記録
2. server に `EstablishRelayResponse{Ok}` を返却 (registered conn 経由)
3. server が `SendHandshake(proxy_runner.Addr, slot_id)` で初期 ECDH 開始
4. proxy_runner endpoint が handshake を accept、activeConn at (server.Addr, slot_id) が確保される
5. proxy_runner の accept handler が新 conn の slot_id を expectedRelays で照合
6. 一致したら `SetProxy(owned=activeConn.CID, allocate=NewConnectionID(target.Transport, target.Addr, slot_id))`
7. proxy_runner が activeConn を Close (proxySettings は残るので forward 機能は維持)

### 結果として server↔target で Phase A の通常 flow が走る

- server が `RehandshakeForProxy` を呼ぶ → 新 ECDH 鍵で再 handshake、packet が
  proxy_runner の proxySettings 経由で target に届く
- target は **proxy_runner を中継だと意識せず**、proxy_runner.Addr から来た新規
  handshake として activeConn を作る
- target の listen handler (Phase A の `handleAcceptedConn`) が新 conn を pickup
- **server が** rehandshake 完了後に **DialGreeting{Version: 1}** を送出
  (proxy_runner じゃなく server 自身)。proxy_runner は packet を中継するだけ
- target の handleAcceptedConn が first inbound = DialGreeting と認識 → driveAfterConn
  → PSK 送出 → server validate → RunnerHello → server registry insert

target_runner 側のコードは何も変える必要が無い。proxy_runner 側もシンプル
(ECDH dial 不要、SetProxy 1 回呼ぶだけ)。

---

## Wire schema 追加

### `runner/protocol/message.bgn` への追加

```bgn
# server → proxy_runner: 「target_cid に dial して、私のために slot_id で relay 設定して」
# proxy_runner がこの request を受けたら DoECDHHandshake(target_cid) + SetProxy + DialGreeting 送出
# + relay 完了応答までを担う。
format EstablishRelayRequest:
    target  :RunnerID
    slot_id :u16       # server が選ぶ connection_id。proxy_runner はこれを SetProxy 設定で使う

enum EstablishRelayStatus:
    :u8
    ok                       = "ok"
    target_dial_failed       = "target_dial_failed"        # proxy_runner が target に ECDH できなかった
    slot_collision           = "slot_collision"            # slot_id が proxy_runner の既存 conn と衝突
    set_proxy_failed         = "set_proxy_failed"          # SetProxy の内部失敗
    invalid_target           = "invalid_target"            # target RunnerID malformed

format EstablishRelayResponse:
    status :EstablishRelayStatus

# RunnerRequest enum に新 variant を追加 (server → proxy_runner 方向のメッセージ)。
# Phase A の AssignTask / CancelTask / 等と並ぶ。
# (現状 RunnerRequestType 末尾に追加)
enum RunnerRequestType:
    :u8
    assign_task
    cancel_task
    open_exec
    runner_hello_response
    task_wake
    open_file_transfer
    list_files
    establish_relay         # ← 追加

format RunnerRequest:
    kind :RunnerRequestType
    match kind:
        # ... 既存 variant ...
        RunnerRequestType.establish_relay => establish_relay :EstablishRelayRequest
```

Response は別途 `RunnerMessage` 系か `RunnerRequest` 系の応答チャネルで返す。実装時
に既存パターンに合わせて確定 (現状 RunnerMessage には HelloResponse 系統あり)。

### `DialRunnerRequest` を `via` 拡張

既存 (`docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md`
Phase A):
```bgn
format DialRunnerRequest:
    target :RunnerID
```

拡張:
```bgn
format DialRunnerRequest:
    target :RunnerID
    via    :RunnerID    # transport が空文字列なら未指定 = 直接 dial (Phase A 既存挙動)
                        # 非空なら registered runner の CID として relay 経由
```

`via.transport_len == 0` (ゼロ値) を「via 未指定」のマーカーとして使う (Phase A の
直接 dial と完全 backward compatible)。

**RunnerID を直接使う利点**:
- `harness-cli ls` 出力の `id=ws:host:port-id` がそのまま server-side
  `transport.connMap` の key になっている。Admin はそれをコピペして
  `--via <cid>` に渡すだけで良い
- Server 側は registry で **CID exact match** で entry 検索 → 見つからなければ
  `via_not_found`。Selector 解決の中間 layer 不要
- 一致した entry の `peer.Conn` の CID から直接 addr を取り、`SendHandshake` に使う

**dial-mode legacy proxy_runner との互換性**: server-side ConnectionID には
runner の outbound src ephemeral port が含まれる。`ls` で見えるのも同じ ephemeral
port なので、admin がコピペすれば addr 不一致は起きない (admin が手書きで
listen port を入れた場合のみ問題)。

`DialRunnerStatus` に **via 関連の新 status を追加**:
```bgn
enum DialRunnerStatus:
    :u8
    ok                   = "ok"
    dial_failed          = "dial_failed"
    psk_failed           = "psk_failed"
    hello_timeout        = "hello_timeout"
    invalid_target       = "invalid_target"
    via_not_found        = "via_not_found"          # via 指定された CID が registered でない
    via_relay_failed     = "via_relay_failed"       # proxy_runner が EstablishRelay 失敗を返した
```

---

## Ceremony (詳細シーケンス)

```
admin                    server                proxy_runner            target_runner
  │                         │                       │                       │
  │ DialRunnerRequest{      │                       │                       │
  │   target=C,             │                       │                       │
  │   via=B-cid             │                       │                       │
  │   (ls 由来の CID)}      │                       │                       │
  ├────────────────────────►│                       │                       │
  │                         │                       │                       │
  │                         │ via CID で registry   │                       │
  │                         │ を exact-match 検索   │                       │
  │                         │ → RunnerEntry (B)     │                       │
  │                         │  → 見つからなければ   │                       │
  │                         │    via_not_found       │                       │
  │                         │                       │                       │
  │                         │ slot_id := random u16  │                       │
  │                         │ (proxy_runner の既存   │                       │
  │                         │  server-conn ID と     │                       │
  │                         │  衝突しない値)         │                       │
  │                         │                       │                       │
  │                         │ EstablishRelayRequest │                       │
  │                         │ {target=C, slot_id=X} │                       │
  │                         ├──[既存 conn]─────────►│                       │
  │                         │                       │                       │
  │                         │                       │ slot_id 衝突 check    │
  │                         │                       │ expectedRelays[      │
  │                         │                       │   slot_id] = C を記録 │
  │                         │                       │                       │
  │                         │ EstablishRelayResponse│                       │
  │                         │ {status=Ok}           │                       │
  │                         │◄──[既存 conn]─────────┤                       │
  │                         │                       │                       │
  │                         │ SendHandshake(        │                       │
  │                         │   (transport,         │                       │
  │                         │    proxy_runner.Addr, │                       │
  │                         │    slot_id),          │                       │
  │                         │   priv1, hs1)         │                       │
  │                         ├──────────────────────►│                       │
  │                         │                       │ 初期 ECDH server↔proxy │
  │                         │                       │ activeConn at         │
  │                         │                       │ (server.Addr,slot_id) │
  │                         │                       │                       │
  │                         │                       │ accept handler:       │
  │                         │                       │ expectedRelays hit    │
  │                         │                       │ → SetProxy(           │
  │                         │                       │     activeConn.CID,   │
  │                         │                       │     (target.Transport,│
  │                         │                       │      target.Addr,     │
  │                         │                       │      slot_id))        │
  │                         │                       │   ※ allocate は      │
  │                         │                       │     synthetic CID    │
  │                         │                       │ → activeConn.Close() │
  │                         │                       │   (proxySettings 残) │
  │                         │                       │                       │
  │                         │ RehandshakeForProxy(  │                       │
  │                         │   priv2, hs2)         │                       │
  │                         │ → packet at slot_id   │                       │
  │                         ├──────────────────────►│ proxySettings hit    │
  │                         │                       ├─[forward, raw]──────►│ ECDH (server pubkey)
  │                         │                       │   target が新 active │
  │                         │                       │   conn at            │
  │                         │                       │   (proxy.Addr,slot)  │
  │                         │                       │                       │
  │                         │                       │◄─[forward, raw]──────┤ HandshakeAck
  │                         │◄──────────────────────┤                       │
  │                         │ 新 activeConn at      │                       │
  │                         │ (proxy.Addr, slot_id) │                       │
  │                         │ end-to-end with C     │                       │
  │                         │                       │                       │
  │                         │ SendMessage(          │                       │
  │                         │   DialGreeting{v=1})  │                       │
  │                         ├──────────────────────►│                       │
  │                         │                       ├─[forward, raw]──────►│ target.handleAcceptedConn:
  │                         │                       │                       │ first inbound = DialGreeting
  │                         │                       │                       │ → driveAfterConn 起動
  │                         │                       │                       │
  │                         │  (以降 Phase A 通常 flow)                     │
  │                         │◄──── PSK ─────────────[relay 越し]────────────┤
  │                         ├──── PSK ack ──────────[relay 越し]───────────►│
  │                         │◄──── RunnerHello ─────[relay 越し]────────────┤
  │                         │ → registry に C を    │                       │
  │                         │   追加                │                       │
  │                         ├──── HelloResponse ───[relay 越し]────────────►│
  │                         │                       │                       │
  │ DialRunnerResponse{Ok}  │                       │                       │
  │◄────────────────────────┤                       │                       │
```

### 順序制約

proxy_runner 側:
1. **EstablishRelayResponse{Ok}** を返す → expectedRelays に slot_id を記録済
2. server の `SendHandshake` が proxy 経由で来る → 初期 ECDH 完了で activeConn 確保
3. accept handler: expectedRelays 一致 → **SetProxy** を呼ぶ (この時点で proxySettings が effective)
4. **activeConn を Close** (proxySettings は残るので forward 機能維持)
5. server の RehandshakeForProxy → forwarded packet が target に到達 → target で end-to-end activeConn 作成

server 側:
1. **DialGreeting は rehandshake 完了後**に送出 (RehandshakeForProxy が返す ChanWithTimeout から新 conn 取得した後)。target が listen handler で正しく discriminate するため、first app message は DialGreeting

DialGreeting を proxy_runner ではなく server が送ることで、proxy_runner は target と一切
ECDH 不要、handler の責務が SetProxy 設定だけになる (大幅シンプル)。

---

## CLI surface

```
harness-cli server dial-runner <target-cid> [--via <proxy-cid>] [--server-cid <server-cid>]
  # --via 未指定 → Phase A の直接 reverse-dial (既存挙動、不変)
  # --via <proxy-cid> → 指定された registered runner を経由
  # <proxy-cid> は `harness-cli ls` 出力の id= 欄をコピペする想定
```

例:
```
# ls で見えた id をそのまま via に渡す
harness-cli --server-cid ws:A:8549-* server dial-runner ws:C:8540-* --via ws:192.168.3.14:52036-51357
```

### TUI / WebUI

既存の `server dial-runner` 拡張で `--via` を受ける。
- TUI: `server dial-runner <target> [--via <proxy-cid>]`
- WebUI: `harness.serverDialRunner(target, via?)` (2nd arg は optional CID 文字列)

---

## 既存コード変更箇所

### `runner/protocol/message.bgn`
- `DialRunnerRequest` に `via :RunnerID` 追加
- `DialRunnerStatus` に `via_not_found` / `via_relay_failed` 追加
- 新 `EstablishRelayRequest` / `EstablishRelayStatus` / `EstablishRelayResponse`
- `RunnerRequestType` に `establish_relay` 追加 + match

### `server/dial_runner_handler.go`
- `DialRunnerRequest.via.transport_len != 0` なら relay 経路へ:
  1. `via` を `RunnerID → ConnectionID` 変換、`registry` で exact-match 検索 → 見つからなければ `via_not_found`
  2. Resolved RunnerEntry の peer.Conn から CID を取得 (= via で渡された CID と同じ addr)
  3. slot_id (random u16) を選び、proxy_runner conn 上に `RunnerRequest{establish_relay, target, slot_id}` を送出
  4. proxy_runner からの `EstablishRelayResponse` を待つ
  5. 応答が Ok なら、`(transport, proxy_runner_addr, slot_id)` 宛に `SendHandshake` で **初期 ECDH** server↔proxy_runner 開始
  6. server endpoint で activeConn at (proxy.Addr, slot_id) が作成される
  7. server がこの conn で `RehandshakeForProxy(newKey, newHS)` を呼ぶ → 新 ECDH packet が proxy 経由で target に到達
  8. rh.C から新 conn (server↔target end-to-end) を取得
  9. **新 conn に DialGreeting{Version:1} を送出** (Phase A の direct dial の DialGreeting 送出箇所と同じ責務)
  10. その conn を OnDialed callback で server's handleConnection に渡す → 既存 Phase A flow (PSK + RunnerHello) が走る

### `runner/` 新ファイル: `runner/relay_handler.go`
- `RunnerRequest{establish_relay}` 受領で:
  1. target を `objproto.ConnectionID` に変換、slot_id collision check
  2. `expectedRelays[slot_id] = target` を session state に記録
  3. EstablishRelayResponse を server に send back (Sender 経由)
- target との ECDH は**行わない** — SetProxy の allocate 側は synthetic CID
- accept handler 側 (`runner/listen.go`):
  1. 新 conn の slot_id が `expectedRelays` にあれば
  2. `objproto.SetProxy(activeConn.CID, NewConnectionID(target.Transport, target.Addr, slot_id))`
  3. activeConn.Close (peer.Conn wrapper も close、proxySettings は残る)
  4. expectedRelays から slot_id を削除 (one-shot)

### `runner/session.go`
- session に endpoint reference を持たせる必要あり (relay_handler.go から `SetProxy` 呼ぶため)
- Phase A の `Session.ServerCIDForProxyAllocate` 同様、`Session.Endpoint` accessor を追加

### CLI / TUI / WebUI
- `cli/server_dial_runner.go`: `ServerDialRunner` を `--via` 引数受け取れるよう拡張
- `cmd/harness-cli/main.go`: `server dial-runner` に `--via` flag
- `tui/cmdline.go`: `ServerDialRunnerAction` に `Via string` field、parser 拡張
- `cmd/harness-webui-wasm/main.go`: `harness.serverDialRunner(target, via?)` の 2nd arg
- `webui/static/main.js`: cmd-input で `server dial-runner <target> [--via <proxy>]` 受付

---

## Trust モデル

### proxy_runner の信頼

- proxy_runner は既に server に PSK validation 通過済 (registered)
- server は registered runner からの application message を信頼する (今までも task assignment 等で同じ relationship)
- relay request は既存 server↔proxy_runner conn (PSK auth 済) 上を流れる → 新 auth 不要

### target_runner の信頼

- target_runner は Phase A の通常 PSK + Hello を server と end-to-end で行う
- proxy_runner は packet を復号できない (ECDH は target↔server で derived)
- proxy_runner は packet drop / 遅延は可能だが、改竄不可 (AEAD で server / target 側が検出)

#### PSK 認証の具体動作 (Phase A の挙動が relay 越しでも維持)

両端 (server / target_runner) で同じ `HARNESS_PSK` / `--psk-file` を設定する想定:

1. proxy_runner ↔ target ECDH 後、proxy_runner が target に DialGreeting を送出 (proxy↔target 鍵で暗号化)
2. server が rehandshake → **server↔target で新規 ECDH 完了** (proxy_runner は鍵 derive 不可)
3. target 側 driveAfterConn が **`cli.SendAndWaitPSK` で HARNESS_PSK を送出** (server↔target 鍵で暗号化、proxy_runner は復号不可)
4. server の `pskGate.Check` が **subtle.ConstantTimeCompare で validate**
   - 一致 → `PskAuthStatus_Ok` を返す → target 続行
   - 不一致 → `PskAuthStatus_BadPsk` を返す → target は `cli.PSKAuthError` で conn 切断
5. PSK ok なら target が RunnerHello → server が registry insert

設定の組み合わせ:

| server PSK | target PSK | 結果 |
|---|---|---|
| 設定 | 同じ値で設定 | 認証成立、registry insert |
| 設定 | 異なる値で設定 | `BadPsk` で切断、registry insert 失敗 |
| 設定 | 未設定 | target が空 PSK を送出 → server が異なる値として `BadPsk` で切断 |
| 未設定 | 設定 | server の `pskGate.authed = true` (初期状態) で target の PSK 送出を受けるが、validate しないため Hello 受け付け → registry insert |
| 未設定 | 未設定 | PSK 交換スキップ → registry insert |

つまり Phase A 既存挙動と完全一致。relay 経路でも PSK 認証は **end-to-end で encrypted** に走り、proxy_runner が compromised でも server/target の PSK secret は守られる。

### 攻撃モデル

- **Compromised proxy_runner**: target_runner の handshake を見ることはできるが、ECDH の secret key は持たないので AEAD の forge は不可。最大限できるのは「target との conn を切断する」「relay を承諾せず無視する」だけ
- **Compromised server**: 既に server は PSK 持ってるので、被害は別問題 (Phase A/B 共通)
- **External attacker on server↔proxy_runner network**: 既存 ECDH + PSK + AEAD で防御済 (Phase A 共通)

---

## Open questions

1. **proxy_runner が target dial に失敗した時の通信**: `EstablishRelayResponse{target_dial_failed}` を返すが、その後 proxy_runner が target に何度かリトライするかは未決定。MVP は「1 回だけ試して fail で即返す」で OK 想定
2. **slot_id 衝突回避の retry**: server が slot_id をランダム生成、proxy_runner で衝突なら `slot_collision` 返す。server が retry (Phase B の DialViaProxy パターンと同型) で 3 回まで再試行
3. **CLI / TUI / WebUI の via 引数構文**: 単一 `--via <cid>` flag。TUI も同形。WebUI cmd-input は `--via=<cid>` 形式で parse。Plan で具体実装を確定
4. **proxy_runner の disconnect 時の挙動**: 確立した relay 越しの server↔target conn は、proxy_runner が落ちると当然死ぬ (transport conn 消滅で packet flow が途絶える)。server / target は通常の disconnect detection (ping timeout) で気づく。本 spec は明示的な disconnect notification を実装しない
5. **target_runner が listen mode じゃない時のエラー**: target が dial mode の場合、ECDH 自体は完了するが PSK 送出方向が想定と違う。proxy_runner の DoECDHHandshake は成功するが、target 側で Phase A の listen-mode 経路に乗らない。spec 範囲外として「target は listen mode であること」を前提に
