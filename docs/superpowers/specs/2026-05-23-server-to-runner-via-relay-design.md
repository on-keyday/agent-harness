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

### proxy_runner が target_runner を dial する

proxy_runner が `EstablishRelay` を受けたら:
1. `objproto.DoECDHHandshake(ctx, ep, target_cid)` で target_runner と ECDH 完了
2. `objproto.SetProxy(targetActiveCID, (server_transport, server_addr, slot_id))` で
   slot_id 経由で server から来るパケットを target に転送する設定
3. server に DialGreeting 相当の応答を返す
4. server が `RehandshakeForProxy` で slot_id 上で新 ECDH → target_runner と end-to-end
   keys 共有

### 結果として server↔target_runner で Phase A の通常 flow が走る

- target_runner は `--listen` モードで稼働中、`DialGreeting` を expect している
  (Phase A 既存)
- proxy_runner は ECDH 完了直後に `DialGreeting{Version: 1}` を target に送る (Phase A
  の server がやってる動作と同じ)
- target_runner は PSK 送出 → server (relay 越し) が validate → RunnerHello 送出 →
  server が registry insert

target_runner 側のコードは何も変える必要が無い。

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
  │                         │                       │ DoECDHHandshake(C) ──►│ ECDH
  │                         │                       │◄──────────────────────┤ HandshakeAck
  │                         │                       │                       │
  │                         │                       │ DialGreeting{v=1} ───►│ (Phase A marker)
  │                         │                       │                       │
  │                         │                       │ SetProxy(             │
  │                         │                       │   targetActiveCID,    │
  │                         │                       │   (transport,         │
  │                         │                       │    server.Addr,       │
  │                         │                       │    slot_id)           │
  │                         │                       │ )                     │
  │                         │                       │                       │
  │                         │ EstablishRelayResponse│                       │
  │                         │ {status=Ok}           │                       │
  │                         │◄──[既存 conn]─────────┤                       │
  │                         │                       │                       │
  │                         │ SendHandshake(        │                       │
  │                         │   (transport,         │                       │
  │                         │    proxy_runner.Addr, │                       │
  │                         │    slot_id),          │                       │
  │                         │   newKey, newHS)      │                       │
  │                         │       ↓ proxy_runner receive(), proxySettings hit, forward to target
  │                         │       ├───────────────►       ───────────────►│ ECDH (server pubkey)
  │                         │       │               │       ◄───────────────┤ HandshakeAck
  │                         │       ◄───────────────┤    forward to server  │
  │                         │ 新 activeConn at      │                       │
  │                         │ (proxy_runner.Addr,X) │                       │
  │                         │ end-to-end with C     │                       │
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

### 順序制約 (Phase B から継承)

proxy_runner 側:
1. **SetProxy** が先 — slot_id への inbound が来た時点で確実に forward される状態を作る
2. **EstablishRelayResponse{Ok}** を返す
3. server が `SendHandshake` で slot_id 宛 packet を送り始める

target_runner 側: 何も特別な順序制約は無い (Phase A の listen-mode accept flow がそのまま動く)。

### `EstablishRelayResponse` を返してから DialGreeting を送る必要性

proxy_runner は target_runner に DialGreeting を send したいが、send の **タイミング**
は server の rehandshake 前でなければならない:

- target_runner は Phase A の logic で「最初の inbound = DialGreeting か agent_proxy_control
  かで discriminate」 (Phase B の `handleAcceptedConn`)
- DialGreeting を send しないと target_runner は server の rehandshake packet を
  agent_proxy_control と誤判定 (proxy ceremony を試みる) する

順序:
1. proxy_runner: DoECDHHandshake (target との初期 ECDH 完了 — これでまず activeConn ができる)
2. proxy_runner: SetProxy 設定
3. proxy_runner: **DialGreeting を target に送出** (target 側 listen handler が `DialGreeting` を first inbound として受け取り、driveAfterConn 経路に分岐)
4. proxy_runner: EstablishRelayResponse{Ok} を server に返す
5. server: RehandshakeForProxy で slot_id 上に新 ECDH → relay 越しに target で ECDH 完了
6. target 側 driveAfterConn は PSK 送出を待っている — server から PSK 来て validate, 続いて RunnerHello → server が registry insert

DialGreeting を Step 3 で proxy_runner が send することで、target_runner の listen handler は **既存 Phase A コード** で対応可能。target_runner 側変更ゼロ。

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
  4. proxy_runner からの `EstablishRelayResponse` を待つ (`RunnerMessage` 系の応答チャネル経由)
  5. 応答が Ok なら、`(transport, proxy_runner_addr, slot_id)` 宛に `SendHandshake` で新 ECDH 開始。transport は connMap の既存 entry を再利用して packet を送る
  6. Phase A 既存 flow に合流 (server endpoint が新 activeConn を pickup する)

### `runner/` 新ファイル: `runner/relay_handler.go`
- `RunnerRequest{establish_relay}` 受領で:
  1. target を `objproto.ConnectionID` に変換
  2. `objproto.DoECDHHandshake(ctx, session.endpoint, target_cid)` 実行
  3. SetProxy 呼び出し
  4. DialGreeting を target に送出
  5. EstablishRelayResponse を server に send back (Sender 経由)

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
