# Named WebSocket dial targets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a dial record reverse-dial runners behind a name-routed front (`wss://host/path`), several of them on one IP:port, and fix the slow-dial socket race in objtrsf's WebSocket transport.

**Architecture:** objtrsf gains a named address (`100::/64` + 64-bit key, like vsock), a URL table held by the WebSocket leg, and first-socket-wins in the dial path. The harness server registers URL candidates in that table from its dial manager, adds `wss` to its dial legs, tightens kick, refuses named targets on the one-shot paths, and shows the answered URL as `RunnerInfo.link_target` on every surface.

**Tech Stack:** Go 1.26 module (toolchain 1.27.3), `golang.org/x/net/websocket`, `.bgn` → ebm2go (`make protoregen`).

**Spec:** `docs/superpowers/specs/2026-11-21-named-websocket-dial-targets-design.md` — read the Problem section (P1–P6) and Decisions D1–D16 before any task.

## Global Constraints

- Two repos. objtrsf: `/home/kforfk/workspace/objtrsf`, branch `main`, commit locally, **do not push until Task 8**. Harness: this worktree, branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`.
- While developing, the harness builds against the local objtrsf through a `replace github.com/on-keyday/objtrsf => /home/kforfk/workspace/objtrsf` line in `go.mod` that is **never committed** (Task 4 adds it, Task 8 replaces it with a real version).
- Stage by explicit path only; `git show --stat` after every commit.
- Harness tests: `make test` (strips inherited `HARNESS_*`), integration: `make test-integration`. This host is memory-tight (≈2 GiB free, integration binary ≈2 GB): if the integration binary is OOM-killed (`signal: killed`, `journalctl -k | grep 'Killed process'`), run the affected tests by `-run` in smaller groups; do not read an OOM kill as a test failure.
- Compile checks write no binaries into the worktree: `go build ./...`, `go vet ./...`, `go vet -tags integration ./...`.
- Wire: only `RunnerInfo` gains a field; regenerate with `make protoregen`; run `scripts/wire-skew-check.sh` before landing.
- D4 error texts name the rule; a query is refused with exactly `query is not supported`.
- No credential guidance in code comments beyond what D15 says; help text says the URL is displayed.

## Review Focus

1. **A URL record across a server restart.** The new server's table is empty until the dial manager re-registers the candidate; the record must reconnect the runner with the same address (D5). → Task 7 integration test restarts the server.
2. **`localhost` resolving to `::1` first while the proxy listens on `127.0.0.1`.** The dial must fall back to the next address, as `net.Dialer` does. → Task 7 uses `wss://localhost:<port>/…` on purpose.
3. **`cid=` copied from `ls` into a `by_conn_id` selector.** `wss:@<hex>-<id>` must parse back and pin a task to that runner. → Task 6 test.
4. **`runner dial rm` while connected.** `link_target` must clear when the link turns `dialed_out`. → Task 6 test.
5. **One proxied runner's upstream dies.** Only that runner's connection ends (named addresses are distinct, so `CloseConnectionsAt` cannot reach the other). → Task 7 integration test.

---

### Task 1: objtrsf — named address in `objproto`

**Files:**
- Create: `objproto/named_addr.go`, `objproto/named_addr_test.go`
- Modify: `objproto/objproto.go` (`ConnectionID.String`, `ParseConnectionID`)

**Interfaces:**
- Produces: `func NamedAddrPort(key [8]byte) netip.AddrPort`, `func NamedFromAddrPort(ap netip.AddrPort) (key [8]byte, ok bool)`. Text form `<ws|wss>:@<16 lowercase hex>-<id>`.

- [ ] **Step 1: Write failing tests** in `objproto/named_addr_test.go`:
  - `TestNamedAddrRoundTrip`: `NamedFromAddrPort(NamedAddrPort(k))` returns `k, true`; the address is IPv6, in `100::/64`, port 0.
  - `TestNamedFromAddrPortRejects`: `1.2.3.4:0`, `[::1]:0` and `[100::1]:443` (right prefix, non-zero port) → `ok == false`. `VsockAddrPort(3, 5)` → `ok == true`: both live in `100::/64` with port 0, so `NamedFromAddrPort` cannot tell them apart and every caller checks the transport (`ws`/`wss`) first. The test pins that.
  - `TestNamedConnectionIDText`: `NewConnectionID("wss", NamedAddrPort(k), 4711).String() == "wss:@" + hex(k) + "-4711"`; `ParseConnectionID` of that string returns an equal `ConnectionID` with no DNS lookup (also with `ParseOption_ResolveAddr`); same for `ws`.
  - `TestNamedConnectionIDTextRejects`: `udp:@<16hex>-1`, `wss:@<15hex>-1`, `wss:@<16 non-hex>-1`, `wss:@-1` → error. `vsock:3:5-7` still prints and parses as before.
- [ ] **Step 2:** `go test ./objproto -run 'Named' -count=1` → FAIL (undefined).
- [ ] **Step 3:** Implement. Prefix bytes are `vsockPrefix` (same `100::/64`). `String()`: for transport `ws`/`wss` and `NamedFromAddrPort` ok → `fmt.Sprintf("%s:@%016x-%d", …)` over the key bytes. `ParseConnectionID`: after cutting transport and id, if `addrStr` starts with `@` → require transport `ws`/`wss` and exactly 16 hex digits, else error `named address: …`.
- [ ] **Step 4:** `go test ./objproto -count=1` → PASS (whole package).
- [ ] **Step 5:** Commit in objtrsf: `objproto: a named address — a 64-bit key in 100::/64 for ws/wss`.

### Task 2: objtrsf — `WebSocketTarget` and `WebSocketTargets`

**Files:**
- Create: `transport/websocket_target.go` (no build constraint), `transport/websocket_target_test.go`
- Modify: `transport/websocket_common.go` (`WebSocketConfig.Targets *WebSocketTargets`, doc comment: wasm ignores it)

**Interfaces:**
- Consumes: Task 1 `NamedAddrPort`.
- Produces:
  ```go
  type WebSocketTarget struct{ /* canonical *url.URL, unexported */ }
  func ParseWebSocketTarget(raw string) (WebSocketTarget, error) // D4 in full
  func (t WebSocketTarget) String() string    // canonical text
  func (t WebSocketTarget) Transport() string // "ws" | "wss"
  func (t WebSocketTarget) Key() [8]byte      // sha256(String())[:8]
  func (t WebSocketTarget) Host() string      // u.Host as canonical (host[:port]) — for Location/Origin
  func (t WebSocketTarget) URL() *url.URL     // a copy
  type WebSocketTargets struct{ /* sync.RWMutex + map[[8]byte]WebSocketTarget */ }
  func NewWebSocketTargets() *WebSocketTargets
  func (s *WebSocketTargets) Register(t WebSocketTarget) netip.AddrPort
  func (s *WebSocketTargets) Lookup(key [8]byte) (WebSocketTarget, bool)
  ```

- [ ] **Step 1: Write failing table test** `TestParseWebSocketTarget` — one row per D4 rule, asserting accept/reject, the canonical `String()`, and that each error message names its rule:
  - reject: `wss://h`, `wss://h/ws?`, `wss://h/ws?x=1`, `wss://h/ws#`, `wss://u@h/ws`, `wss:h/ws`, `wss:///ws`, `wss://hé.example/ws`, `wss://h./ws`, `wss://[fe80::1%25eth0]/ws`, `wss://h:99999/ws`, `wss://h:0/ws`, `http://h/ws`. The two `?` rows: `err.Error()` contains `query is not supported`.
  - accept → canonical: `wss://h/` → `wss://h/`; `WSS://H.Example.COM:443/WS` → `wss://h.example.com/WS`; `ws://h:80/ws` → `ws://h/ws`; `wss://h:/ws` → `wss://h/ws`; `wss://h:8443/ws` unchanged; `wss://[::1]:8443/ws` unchanged; `wss://h/a%2Fb` → `/a%2Fb`; `wss://h/a%41` → `/a%41`; `wss://h/a b` → `wss://h/a%20b`; `/ws`, `/ws/`, `/./ws` give three different `String()`s and keys.
- [ ] **Step 2:** `TestWebSocketTargetKeyStable`: two spellings that canonicalize equal give equal `Key()`; `Key()` equals `sha256.Sum256([]byte(String()))[:8]`. `TestWebSocketTargetsRegister`: `Register` twice → same address; `Lookup(NamedFromAddrPort(addr))` returns the target; unknown key → false.
- [ ] **Step 3:** `go test ./transport -run 'WebSocketTarget' -count=1` → FAIL.
- [ ] **Step 4:** Implement. `?`/`#` and control characters are checked on the raw string before `url.Parse`. Non-ASCII: any byte ≥ 0x80 in `u.Hostname()`. Zone: `strings.Contains(u.Hostname(), "%")`. Path: `u.EscapedPath()`, must be non-empty. Port: `strconv.Atoi`, 1–65535, dropped when it equals the scheme default.
- [ ] **Step 5:** `go test ./transport -count=1` and `GOOS=js GOARCH=wasm go vet ./transport` → PASS.
- [ ] **Step 6:** Commit: `transport: WebSocketTarget — a dial URL a named address stands for`.

### Task 3: objtrsf — named dial and first socket wins

**Files:**
- Modify: `transport/websocket.go` (`connectionMap.setIfAbsent`; send loop handshake branch; `startTransportLoops` gains `targets *WebSocketTargets`; `WebSocketEndpointEx` passes `cfg.Targets`)
- Create: `transport/websocket_dial_test.go`

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces: no new exported API beyond `WebSocketConfig.Targets` (Task 2).

- [ ] **Step 1: Write the P6 test first** — `TestSlowFrontKeepsOneConnection` (subtests `ws`, `wss`): `httptest` server whose handler sleeps 1.5 s before `mux.ServeHTTP` (reuse `wsServer`'s shape from `websocket_tls_test.go`); handshake with an 8 s wait; sleep 2.5 s; assert `len(ListActiveConnections()) == 1` on the server, then client `SendMessage("ping")` is received by the server connection within 1 s and its `SendMessage("pong")` reaches the client within 2 s.
- [ ] **Step 2:** Run it → FAIL (three server connections, nothing delivered). Keep the output in the commit message body as the red evidence (one line).
- [ ] **Step 3: Write the guard** — `TestHungFirstUpgradeStillConnects`: the first request blocks until test cleanup, later ones are served; handshake within 1 s succeeds and a ping is delivered. Run → PASS on current code (it must keep passing).
- [ ] **Step 4: Write the named-dial tests**:
  - `TestNamedTargetsShareAnAddress`: one TLS `httptest` server on `127.0.0.1:<p>` whose handler records `r.Host`, `r.TLS.ServerName`, `r.URL.Path`, strips the `/a` or `/b` prefix and serves two separate Server-mode endpoints (`/a/ws` → A, `/b/ws` → B). Client leg with `Targets` holding `wss://localhost:<p>/a/ws` and `wss://localhost:<p>/b/ws`; handshake to both named CIDs concurrently. Assert: two requests, `Host == "localhost:<p>"`, `ServerName == "localhost"`, paths `/a/ws` and `/b/ws`; A and B each hold one connection; a message to each arrives only at its own endpoint; closing A's underlying listener-side socket leaves B's connection active.
  - `TestNamedTargetsNegativeControl`: two IP `ConnectionID`s `wss:127.0.0.1:<p>-1` and `-2` → **one** upgrade request, `Host == "127.0.0.1:<p>"`, `ServerName == ""`.
  - `TestNamedAddressWithoutEntryCannotSend`: named CID whose key is not registered, and one whose target scheme is `ws` while the CID says `wss` → handshake fails within 2 s and the server saw no request.
- [ ] **Step 5:** Run → the named tests FAIL (no `Targets` use yet); the negative control PASSES.
- [ ] **Step 6:** Implement. `setIfAbsent(addr, conn) (existing *WebSocketConn, stored bool)` under `connMapLock`. In the handshake goroutine: if `pkt.To.Transport` is `ws`/`wss` and `objproto.NamedFromAddrPort` is ok → `targets.Lookup` (nil `targets`, miss, or `t.Transport() != pkt.To.Transport` → log + `CannotSend`, return); `Location = t.URL()`, `Origin = {Scheme: http|https, Host: t.Host()}`. Otherwise build the URL as today. After a successful dial: `setIfAbsent`; when not stored → `ws.Close()`, send `pkt.Data` on `existing` (`CannotSend` on error), return without `connChan <- conn`. TLS config is the leg's, unchanged (D8).
- [ ] **Step 7:** `go test ./... -count=1` in objtrsf → PASS (all packages; Step 1 now green, Step 3 still green). `GOOS=js GOARCH=wasm go vet ./transport` → clean.
- [ ] **Step 8:** Commit: `transport: dial a named target by its URL; the first socket to land wins` (body: P6 red line from Step 2, and that the hung-first guard passed before and after).

### Task 4: harness — candidates, `wss` leg, dial-manager resolve

**Files:**
- Modify: `go.mod` (temporary `replace`, **do not stage**), `cli/candidate.go`, `server/server.go` (`buildEndpoint`, `DialLegs`, `Server` field, `dialManagerConfig.Resolve`), `server/runner_dial_handler.go`
- Create/extend tests: `cli/candidate_test.go`, `server/runner_dial_handler_test.go`, `server/runner_dialer_test.go`

**Interfaces:**
- Produces: `func CanonicalDialRecordCandidate(text string) (string, error)` in `cli`; `func IsURLCandidate(text string) bool` (`strings.Contains(text, "://")`); `func resolveDialCandidate(targets *transport.WebSocketTargets) func(string) (objproto.ConnectionID, error)` in `server`; `Server.wsTargets *transport.WebSocketTargets`.

- [ ] **Step 1:** Add the `replace` line; `go build ./...` passes.
- [ ] **Step 2: Failing tests**:
  - `TestCanonicalDialRecordCandidate`: three D4 rows pass through (`WSS://H:443/WS` → `wss://h/WS`, `wss://h` rejected, `wss://h/ws?` rejected with `query is not supported`); `ws:1.2.3.4:80-*` returned unchanged; `CandidateTransport` of a URL is `wss`.
  - `TestRunnerDialAddStoresCanonicalURL`: add `WSS://Agent-1.Example.COM:443/ws` → stored `wss://agent-1.example.com/ws`; a second add of `wss://agent-1.example.com:443/ws` → `duplicate_candidate`; on a server config with `Addr == ""` → `invalid_candidates`.
  - `TestDialLegsWSS`: `DialLegs("wss") == (cfg.Addr != "")`.
  - `TestResolveDialCandidateURL`: `resolveDialCandidate(t)("wss://h/ws")` → transport `wss`, address `NamedAddrPort(key)`, and `t.Lookup(key)` holds the target; two calls → same address, different random IDs; an IP candidate → same result as `cli.ResolveCandidate`.
- [ ] **Step 3:** `make test` scoped: `go test ./cli ./server -run 'Canonical|DialAdd|DialLegs|ResolveDialCandidate' -count=1` (with the `GO_TEST_ENV` unsets) → FAIL.
- [ ] **Step 4:** Implement. `buildEndpoint`: `targets := transport.NewWebSocketTargets()`; WebSocket leg config gets `TLS: cli.ClientTLSConfig(), Targets: targets`; keep `targets` on `Server` (`nil` when no ws leg). `DialLegs`: `case "ws", "wss": return cfg.Addr != ""`. `runnerDialAdd`: `canon, err := cli.CanonicalDialRecordCandidate(text)` replaces `ParseCandidate`; byte limits, `DialLegs`, `OwnerOfCandidate` run on `canon`. Wire `Resolve: resolveDialCandidate(s.wsTargets)` where `newDialManager` is configured; `targets == nil` + URL → error `no WebSocket leg for a URL candidate`.
- [ ] **Step 5:** The Step 3 command → PASS; `go build ./... && go vet ./...` clean.
- [ ] **Step 6:** Commit (harness, without `go.mod`): `server: dial records take wss:// URLs; the WebSocket leg dials wss`.

### Task 5: harness — refuse named one-shot targets; kick matches by key

**Files:**
- Modify: `server/dial_runner_handler.go` (`Handle`, `HandleWithVia`), `server/runner_dialer.go` (`RecordsPointingAt`)
- Tests: `server/dial_runner_handler_test.go`, `server/runner_dialer_test.go`

- [ ] **Step 1: Failing tests**:
  - `TestDialRunnerRefusesNamedTarget` and the `HandleWithVia` twin: target `ConnIDFromObjproto(NewConnectionID("wss", NamedAddrPort(k), 1))` → `DialRunnerStatus_InvalidTarget`, and the injected dial func was never called.
  - `TestRecordsPointingAtRequiresKeyForAddressMatch`: two records whose IP candidates resolve to one address, bound to different keys; an entry on key A at that address → only A's record id returned. Run → FAIL on the B id being included (red first, P3).
- [ ] **Step 2:** Implement: a helper `isNamedTarget(cid objproto.ConnectionID) bool` (transport `ws`/`wss` and `NamedFromAddrPort` ok) used by both handlers before dialing, with the log line `dial-runner: named targets are dialed only through dial records`. `RecordsPointingAt`: `hit = sameAddr(a, e.ID) && l.rec.Key.Equal(e.Key)`.
- [ ] **Step 3:** `go test ./server -run 'NamedTarget|RecordsPointingAt|Kick' -count=1` → PASS.
- [ ] **Step 4:** Commit: `server: one-shot dials refuse named targets; kick matches a record's address only on its key`.

### Task 6: harness — `link_target` on the wire and every surface

**Files:**
- Modify: `runner/protocol/message.bgn` (`RunnerInfo` append; `ConnID` comment), regenerated `runner/protocol/message.go`
- Modify: `server/dial_runner_handler.go` (`DialOrigin.Candidate string`), `server/runner_dialer.go` (set it in `attempt`; adopt path), `server/registry.go` (`RunnerEntry.LinkTarget`, `SetLink(id, link, record, target string)`), `server/runner_handler.go:149` area, `server/server.go` (`SetLink` wiring), `server/task_handler.go` `toRunnerInfo`
- Modify surfaces: `cli/list.go` (both runner row renderers + JSON struct near line 502), `tui/detail.go` (`formatRunnerDetail`, after the `link:` line ~320), `cmd/harness-webui-wasm/main.go` (runner conversion ~1742: `"linkTarget"`), `webui/static/main.js` (runner row meta, beside `link`), `cli/verb/table.go` (`runner dial add` help), `README.md` (reverse-dial section)

**Interfaces:**
- Produces: wire `RunnerInfo.link_target_len :u16; link_target :[link_target_len]u8` (Go: `LinkTarget []byte`, `SetLinkTarget`); `RunnerEntry.LinkTarget string`.

- [ ] **Step 1:** Edit `message.bgn` exactly as spec §3 (append after `link_via`, with the spec's comment; extend the `ConnID` comment). `make protoregen`. `go build ./...` passes.
- [ ] **Step 2: Failing tests**:
  - `TestLinkTargetFromEachOrigin` (server): record dial with a URL candidate → entry `LinkTarget` = stored text; adoption (D20) sets it; `dialManager.Stop` (record removed) → link `dialed_out`, `LinkTarget == ""`; one-shot and dialed-in → `""`. `toRunnerInfo` copies it.
  - `TestByConnIDSelectsNamedRunner`: a registry entry whose `ID` is a named CID; selector `by_conn_id` built from `ParseConnectionID(entry.ID.String())` matches it.
  - Rendering: `ls` row contains `target=wss://h/ws` when set and no `target=` when empty; `ls --json` has `"link_target":""` on a runner without it (checklist 31: never elide); TUI detail shows `target:` only when set; wasm conversion carries `linkTarget`.
- [ ] **Step 3:** Run them → FAIL.
- [ ] **Step 4:** Implement the server plumbing and the four renderers. WebUI: show `target <text>` in the runner row meta when non-empty, escaped as the existing meta is.
- [ ] **Step 5:** `make test` → PASS (scope down by `-run` only if OOM-killed, per Global Constraints).
- [ ] **Step 6:** Walk `surface-parity-checklist` items 1–39 for `link_target` and the `runner dial add` URL form; record each verdict in the commit body or a short note under `docs/superpowers/plans/` (one line per item).
- [ ] **Step 7:** Commit: `runner list: link_target — the URL a dial record answered on, on every surface`.

### Task 7: harness — integration test behind a path-routing TLS proxy

**Files:**
- Create: `integration/named_dial_record_e2e_test.go` (`//go:build integration`)

- [ ] **Step 1: Write `TestNamedDialRecordsBehindOneAddress`** modelled on `integration/dial_record_hold_e2e_test.go`: two `runner.ListenAndServe` runners (both `testRunnerKey`, hostnames `named-a`/`named-b`, `WSListen` on free ports); an `httptest.NewTLSServer` proxy on `127.0.0.1` with `httputil.ReverseProxy` routing `/a/ws` → `http://<runnerA>/ws`, `/b/ws` → `http://<runnerB>/ws`, recording `Host` and `TLS.ServerName`; a dial store with records `wss://localhost:<p>/a/ws` and `wss://localhost:<p>/b/ws`; `server.New(Config{Addr: …, RunnerDials: …, RunnerKeys: testRunnerKeys()})`. Assert:
  1. both runners registered with `Link == dial_record`, distinct `ID`s, each `LinkTarget` its own URL;
  2. the proxy saw `Host == "localhost:<p>"` and SNI `localhost` for both;
  3. a `-p` task runs on each (selector by hostname);
  4. restart: stop the server, start a new one on the same data dir and dial store → both re-register with the same `LinkTarget`s (Review Focus 1);
  5. cancel runner B's `ListenAndServe` context: B deregisters within 15 s, A stays registered (Review Focus 5);
  6. `runner kick` on A (`cli` kick with A's identity prefix): A's record is gone from `RunnerDials.List()`, B's record is still there.
- [ ] **Step 2:** Scoped integration run: `go test -tags integration ./integration -run TestNamedDialRecordsBehindOneAddress -count=1` (with the `GO_TEST_ENV` unsets) → PASS.
- [ ] **Step 3:** Negative control: extract the objtrsf tree at the Task 2 commit into the scratchpad (`git -C /home/kforfk/workspace/objtrsf archive <task2 sha> | tar -x -C <scratch>/objtrsf-old`), point the `replace` there, rerun → FAIL (no host name reaches the proxy; the two records do not reach two runners). Restore the `replace`. One line of the result goes in the commit body.
- [ ] **Step 4:** Commit: `integration: two dial records behind one TLS address, told apart by URL`.

### Task 8: land

- [ ] **Step 1: Dummy-harness E2E** (`dummy-harness` skill), typed as the operator would: the Task 7 layout with a tiny proxy (reuse the test's proxy via `go run` of a scratch program in the scratchpad, never committed): `harness-cli runner dial add --key <prefix> wss://localhost:<p>/a/ws`, `runner dial ls`, `ls` (shows `target=`), `runner dial add … wss://localhost:<p>/a/ws?x` → `query is not supported`; then the same runner rows in `harness-tui` (dummy PTY: detail popup shows `target:`) and the WebUI (Playwright: row meta shows `target`). Screenshots go to the scratchpad and are reported by path.
- [ ] **Step 2:** objtrsf: `go test ./... -count=1` once more, then FF-push `main` (objtrsf landing policy, Mode A).
- [ ] **Step 3:** Harness: drop the `replace`, `go get github.com/on-keyday/objtrsf@<pushed sha>`, `go mod tidy`; `make test`; `go vet -tags integration ./...`; `scripts/wire-skew-check.sh`.
- [ ] **Step 4:** Commit `go.mod`/`go.sum`: `deps: objtrsf <short sha> (named WebSocket targets, first socket wins)`.
- [ ] **Step 5:** Land with `scripts/land.py` (spec, plan and code together), then `make build` in the main checkout. Classify the landed paths before saying what needs a restart (server: `server/`, wire; runners: objtrsf transport is in every binary).
