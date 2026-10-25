# IP link forwarding, stage 1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A tun device at each end of a forward (`harness-cli forward --ip`, TUI `L`), carrying whole IP packets over the existing datagram path, until `ping` answers between them inside `scripts/netem-lab`.

**Architecture:** One new forward shape, `ip × link`, registered like `-R` (the server waits for the runner's setup result). Packets ride `AppKind_ForwardDatagram` unchanged through the server's existing relay. A new package `iplink` holds the tun device and the pump; the client and the runner each keep a registry of links keyed by `forward_id`. The same change closes three existing gaps: taps on datagram forwards, runner-side forward state that outlives its connection, and rows whose runner end is gone.

**Tech Stack:** Go; `.bgn` schema regenerated with `make protoregen`; `golang.org/x/sys/unix` (already in `go.sum`); bubbletea for the TUI.

**Spec:** `docs/superpowers/specs/2026-10-25-ip-link-forward-design.md`. Read it whole before any task: every task's requirements include the section it names.

## Global Constraints

- **Paths.** Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd/` on branch `ip-link-forward`. A path without `.harness-worktrees/<hash>/` is the PARENT checkout. Check `git rev-parse --abbrev-ref HEAD` says `ip-link-forward` before the first edit of each task.
- **Read first:** `.claude/skills/implementation-pitfalls/SKILL.md` in full.
- **Test env.** Inside a harness task, every `go test` runs as `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID go test ...`. Without it two UDP-forward tests fail with "signature does not verify". Written below as `$GT` (e.g. `$GT ./server/ -run X -count=1`).
- **No stray binaries.** Compile-check with `go build ./...` or `make check`. Never `go build ./cmd/<x>/` in the tree.
- **Schema.** Every wire change is in Task 2 and nowhere else. `make protoregen` owns `runner/protocol/message.go`; hand-written protocol code goes in its own file.
- **Values the spec fixes:** MTU `1100`; tun name template `hlink%d`; `forward_link = 0x400000`; `all = 0x7fffff`; setup wait `remoteForwardBindTimeout` (5 s); datagram seq idle prune `60s`; error texts `ip links are Linux-only`, `ip links are Linux-only (this runner is <GOOS>)`, `ipv6 link addresses are not implemented`; runner details prefixed `runner: `; timeout detail `runner: no answer within 5s`.
- **ASCII in operator text.** Tap lines use `dirArrow` (`->` / `<-`), not `→`: a Windows console is a first-class client. The spec's `→` examples are rendered with `dirArrow`.
- **Commits.** One per task at least. Every message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Do not push and do not land; Task 13 ends at "ready to land".
- **Repo is public.** No LAN addresses, hostnames or private paths in committed files. `10.200.0.0/30` (the spec's example) is fine.

## Review Focus

Inputs the spec implies and no obvious test exercises. Each line names the test that pins it.

1. **Ctrl-C while packets flow.** The CLI must exit 0 with no `tun read failed` line, and the runner must not send `ForwardEnded` when the server closed the link on purpose. Pinned by `TestLinkRunReturnsNilAfterClose` (Task 6) and `TestIPLinkCloseSendsNoForwardEnded` (Task 7).
2. **`ForwardEnded` from a connection that does not hold the forward.** A stale or foreign runner connection must not be able to end someone's link. Pinned by `TestForwardEndedFromOtherConnIgnored` (Task 4).
3. **Two links on one client at once.** Datagrams must reach the device their `forward_id` names, never the other. Pinned by `TestDispatchDatagramRoutesToItsLink` (Task 8).
4. **Malformed operator input** in the CLI flags and the TUI line: no prefix, one token, three tokens, extra spaces, IPv6. Each needs an error that names the expected form. Pinned by `TestParseIPLink` and `TestParseIPLinkLine` (Task 8).
5. **A tap on a udp row.** Today it attaches and shows nothing. Pinned by `TestUDPForwardTapShowsDatagrams` (Task 12).

---

## Task 1: Walk the surface-parity checklist

**Files:**
- Modify: `docs/superpowers/plans/2026-10-25-ip-link-forward.md` (append `## Appendix A — surface parity walk`)

- [ ] **Step 1:** Read `.claude/skills/surface-parity-checklist/SKILL.md` in full. Walk items 1–39 against spec §10 and this plan. Give one verdict per number: `implemented in Task N`, `not applicable because …`, or `intentionally omitted because …`. Then walk S1–S6, which will all be `not applicable` (no agent preset changes).
- [ ] **Step 2:** Where a verdict contradicts this plan or spec §10d, add the missing step to the owning task. Say so in the appendix.
- [ ] **Step 3:** Commit: `plan: IP link — surface parity walk`.

---

## Task 2: Every wire change

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (`make protoregen`)
- Create: `runner/protocol/ip_link.go`, `runner/protocol/ip_link_test.go`
- Modify: `runner/protocol/capability_test.go`, `runner/protocol/port_forward_test.go`
- Modify, renames only: `server/runner_handler.go`, `server/server.go`, `server/port_forward.go`, `server/port_forward_test.go`, `runner/port_forward.go`, `runner/forward_datagram.go`

**Interfaces:**
- Produces (generated):
  - `ForwardProtocol_Ip`; `PortForwardDirection_Link`; `OpenPortForwardStatus_LinkFailed`.
  - `PortForwardCloseReason_RunnerGone`, `PortForwardCloseReason_EndpointFailed`.
  - `RunnerMessageType_ForwardSetupResult` (renamed, same ordinal) and `RunnerMessageType_ForwardEnded`.
  - `ForwardSetupResult{ForwardId, Ok(), Detail}`; `ForwardEnded{ForwardId, Detail}`; `IpLinkConfig{AddrLen, ClientAddr, RunnerAddr, PrefixLen, Mtu}`.
  - `Link() *IpLinkConfig` / `SetLink(IpLinkConfig) bool` on `RegisterPortForwardRequest`, `RunnerOpenPortForwardRequest` and `PortForwardInfo`. `SetLink` returns false unless `Protocol` is already `Ip`, so **set `Protocol` first**.
  - `Detail` on `RegisterPortForwardResponse`, `PortForwardClosed` and `ForwardTapForwardClosed`.
  - `ForwardTapRecordKind_Datagram` / `_DatagramGap`; `ForwardTapDatagramFate_{Relayed,DroppedOversize,DroppedCongestion,DroppedQueue}`; `ForwardTapDatagram`, `ForwardTapDatagramGap`.
  - `Capability_ForwardLink = 0x400000`; `Capability_All = 0x7fffff`.
- Produces (hand-written, `runner/protocol/ip_link.go`):
  - `func (p ForwardProtocol) CarriesDatagrams() bool` — udp or ip.
  - `const IpLinkMTU = 1100`.
  - `func NewIpLinkConfig(client netip.Prefix, runner netip.Addr) (IpLinkConfig, error)` — builds with `Mtu: IpLinkMTU`, then `Validate`.
  - `func (c *IpLinkConfig) Validate() error`.
  - `func (c *IpLinkConfig) ClientPrefix() netip.Prefix` and `func (c *IpLinkConfig) RunnerPrefix() netip.Prefix` — each end's own address with the shared prefix length; the zero `Prefix` when `AddrLen` is not 4 or 16.

- [ ] **Step 1: Edit `message.bgn`.** Make every change in spec §5, including `ForwardTapForwardClosed`'s detail, and §6b's `forward_link` bit with its comment. Copy the comments from the spec. Also:
  - Give `RegisterPortForwardRequest`'s "direction decides what the address pair means" block a `link` line: both address pairs are empty and both ports 0; `link` carries everything.
  - Reword `ClosePortForwardRequest`'s comment: "release whatever runner-side state this forward holds — a `-R` listener, a udp registration, a link's tun".
  - In the `Capability` comment block above `all`, update the literal it quotes.
  - Check that the new union arm names (`datagram`, `datagram_gap`) are not codec method names (`Read`, `Write`, `Encode`, `Decode`, `Append`). They are not.
- [ ] **Step 2: Regenerate.** Run `make protoregen`. Expect a large diff in `message.go`: that churn is normal for this generator. Then read the generated `SetClientAddr` / `SetRunnerAddr` on `IpLinkConfig`. Two arrays share `addr_len`, so note whether each setter writes `AddrLen`. `NewIpLinkConfig` must set both slices with equal lengths whatever the answer. Check every `SetLink` is a setter that allocates, not a dereference of a nil getter. If it dereferences, see `memory/bgn/project_bgn_match_arm_semantics.md`.
- [ ] **Step 3: Rename call sites so the tree builds.** Behaviour does not change here; details stay empty until Tasks 3 and 7.
  - `RemoteForwardBindResult` becomes `ForwardSetupResult`, and `SetRemoteForwardBindResult` / `RemoteForwardBindResult()` follow.
  - `OnRemoteForwardBindResult` becomes `OnForwardSetupResult`, and `handleRemoteForwardBindResult` becomes `handleForwardSetupResult`.
  - The runner's `sendBindResult(id, ok)` becomes `sendSetupResult(id uint64, ok bool, detail string)`, passing `""` for now.
  - Run `go build ./...` and `GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm/`. Expected: both succeed.
- [ ] **Step 4: Write the failing tests** in `runner/protocol/ip_link_test.go`:

```go
func TestCarriesDatagrams(t *testing.T) // tcp false, udp true, ip true
func TestIpLinkConfigValidate(t *testing.T) // table; each case (client prefix, runner addr) -> want error substring ("" = ok):
//   "10.200.0.1/30","10.200.0.2"   -> ""
//   "10.0.0.0/31","10.0.0.1"       -> ""               (RFC 3021: /31 has no network/broadcast)
//   "10.0.0.1/1","10.0.0.2"        -> ""               (lower bound)
//   "10.200.0.1/32","10.200.0.2"   -> "prefix /32 is outside 1-31"
//   "10.200.0.1/0","10.200.0.2"    -> "prefix /0 is outside 1-31"
//   "10.200.0.1/30","10.200.0.1"   -> "the two addresses are the same"
//   "10.200.0.1/30","10.200.0.9"   -> "10.200.0.9 is not inside 10.200.0.0/30"
//   "10.200.0.0/30","10.200.0.2"   -> "10.200.0.0 is the network address of 10.200.0.0/30"
//   "10.200.0.1/30","10.200.0.3"   -> "10.200.0.3 is the broadcast address of 10.200.0.0/30"
//   "fd00::1/64","fd00::2"         -> "ipv6 link addresses are not implemented"
// plus: a hand-built IpLinkConfig with AddrLen 5 -> "address length 5"; Mtu 1500 -> "mtu 1500 is not 1100"
func TestIpLinkConfigRoundTrip(t *testing.T) // inside RegisterPortForwardRequest, RunnerOpenPortForwardRequest, PortForwardInfo:
//   Protocol=Ip + SetLink(cfg) -> encode -> DecodeExact -> Link() equals cfg;
//   Protocol=Udp: SetLink returns false and the encoding equals the same message encoded before SetLink was tried
func TestForwardTapDatagramRoundTrip(t *testing.T) // Datagram and DatagramGap arms through ForwardTapRecord
```

  Every `Validate` error starts `ip link: `, except the IPv6 one, which is exactly `ipv6 link addresses are not implemented`. The spec quotes that text verbatim.

  In `capability_test.go`: `All` is `0x7fffff`; the OR includes `Capability_ForwardLink`; `ForwardLink == 0x400000`; `ForwardLocal|ForwardRemote` does not include `ForwardLink`.
- [ ] **Step 5:** Run `$GT ./runner/protocol/ -count=1`. Expected: FAIL (`CarriesDatagrams` undefined).
- [ ] **Step 6: Implement `runner/protocol/ip_link.go`.**
  - Validation order: address length (4 or 16; 16 gets the IPv6 text), mtu, prefix range, equality, membership, network/broadcast below /31.
  - Use `net/netip`. The "network" is `ClientPrefix().Masked()`. The broadcast is the last address of that prefix.
  - Comment it like `client_endpoint.go`: hand-written beside the generated code because `protoregen` owns `message.go`.
- [ ] **Step 7:** Run `$GT ./runner/protocol/ -count=1` and `go build ./...`. Expected: PASS / success. `./server/` may fail its capability-completeness tests until Task 3 adds the new bit's entries. That is expected and stated in the commit message.
- [ ] **Step 8:** Commit: `protocol: IP link wire changes — ip × link, setup detail, ForwardEnded, datagram taps, forward_link`.

---

## Task 3: Server — shape, capability, registration

**Files:**
- Modify: `server/port_forward.go`, `server/port_forward_registry.go`, `server/task_handler.go`
- Modify: `cli/verb/caps.go`, `cli/verb/caps_audit_table_test.go`, `server/scope_percap_completeness_test.go`
- Test: `server/port_forward_test.go`, `server/capabilities_test.go`

**Interfaces:**
- Consumes: Task 2's protocol symbols.
- Produces:
  - `func validForwardShape(p protocol.ForwardProtocol, d protocol.PortForwardDirection) bool`
  - `func forwardCapFor(d protocol.PortForwardDirection) protocol.Capability` — local → `ForwardLocal`, remote → `ForwardRemote`, link → `ForwardLink`.
  - `type setupResult struct{ ok bool; detail string }`; `addPending(id) chan setupResult`; `signalSetup(id uint64, ok bool, detail string)`. This replaces `signalBind`.
  - `portForward.link *protocol.IpLinkConfig` — set for ip rows; Task 5 lists it.

- [ ] **Step 1: Write the failing tests.**

```go
func TestValidForwardShape(t *testing.T)
// true exactly for: tcp×local, tcp×remote, udp×local, udp×remote, ip×link; false for tcp×link, udp×link, ip×local, ip×remote

func TestRegisterIPLink_SetupOk(t *testing.T)
// runner gets RunnerOpenPortForwardRequest{Direction: Link, Protocol: Ip, Link(): == request's}; signalSetup(1,true,"") -> Status Ok, row listed

func TestRegisterIPLink_SetupFailedCarriesDetail(t *testing.T)
// signalSetup(1,false,"TUNSETIFF: operation not permitted") -> Status LinkFailed,
// Detail "runner: TUNSETIFF: operation not permitted", registry empty, control stream closed, ClosePortForward{1} sent

func TestRegisterRemoteForward_BindFailedCarriesDetail(t *testing.T)
// tcp -R, signalSetup(1,false,"listen tcp 127.0.0.1:15432: bind: address already in use")
// -> BindFailed, Detail "runner: listen tcp 127.0.0.1:15432: bind: address already in use"

func TestRegisterIPLink_InvalidConfigRefused(t *testing.T)
// a /32 link -> InternalError, Detail has prefix "invalid ip link: ", runner received nothing

func TestRegisterRefusesIllegalShape(t *testing.T)
// tcp×link and ip×local -> InternalError, runner received nothing
```

  Rewrite `runRemoteRegister` to feed results through `handleForwardSetupResult` with a `detail` argument. In `capabilities_test.go`, add cases next to the existing "-R with only ForwardLocal" case:
  - Register a link with `ForwardLocal|ForwardRemote` → permission denied, `RequiredCap = ForwardLink`.
  - Register a link with `ForwardLink` alone → passes the gate.
  - Kill a link row with `ForwardLocal|ForwardRemote` → denied with `ForwardLink`.
- [ ] **Step 2:** Run `$GT ./server/ -run 'IPLink|IllegalShape|ValidForwardShape|BindFailedCarries|Capab' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Check order in `handleRegisterPortForward`: task, nil conn, route (existing), then `validForwardShape`, then for ip `Link() != nil && Validate() == nil`, then the existing remote×in-process refusal.
  - Validation failure answers `internal_error` with detail `invalid ip link: <err>`. My call: `link_failed` is reserved for an *end* failing (spec §5).
  - `link` takes the runner branch with `remote`. Generalise `registerRemoteForward`:
    - Send `Direction: req.Direction`. For ip, set `Protocol` and then `SetLink`.
    - On failure the status is `BindFailed` for remote and `LinkFailed` for link.
    - The detail is `"runner: " + result.detail`, or `"runner: no answer within " + remoteForwardBindTimeout.String()`.
  - Both capability gates in `task_handler.go` call `forwardCapFor`.
  - `cli/verb/caps.go`:
    - `GrantableCaps`: add `Capability_ForwardLink` after `ForwardRemote`. Its comment says it is implied by neither and implies neither.
    - `CapDescription`: `"put a network interface at each end of a forward (forward --ip): every service bound to all addresses on either host becomes reachable from the other, over any IP protocol; NOT implied by forward_local or forward_remote"`.
    - `capAuditTable`: `{tier: TierStrong}`.
  - `scope_percap_completeness_test.go`: `protocol.Capability_ForwardLink: {kind: capResolvedVia, via: "need"}`.
  - Update `caps_audit_table_test.go`'s `TierStrong` list.
- [ ] **Step 4:** Run `$GT ./server/ ./cli/verb/ ./runner/protocol/ -count=1`. Expected: PASS. A red `TestOpenInteractive*` or `TestHandleAttachSession_ViewMode*` is a known flaky family. Re-run the package once before reading it as yours.
- [ ] **Step 5:** Commit: `server: ip × link registration waits for the runner; forward_link gates create and kill`.

---

## Task 4: Server — runner-side state has an owner

**Files:**
- Modify: `server/port_forward.go`, `server/port_forward_list.go`, `server/port_forward_registry.go`, `server/forward_tap.go`, `server/runner_handler.go`, `server/server.go`
- Test: `server/port_forward_test.go`, `server/forward_tap_test.go`, `server/runner_handler_test.go`

**Interfaces:**
- Produces:
  - `func (pf *portForward) runnerHoldsState() bool` — `direction == remote || direction == link || protocolKind == udp`. The one predicate for both §6e and §6f.
  - `portForward.runnerCID string` — set when the runner accepted (`registerRemoteForward` success) or was instructed (`sendUDPForwardInstruction` success).
  - `teardownPortForward(pf, reason, detail string, notify bool) bool`; `closePortForward(pf, reason, detail string) bool`; `pushPortForwardClosed(pf, reason, detail string)`; `(pf) closeTaps(reason, detail string)`.
  - `func (h *TaskHandler) DropRunnerStateForwards(runnerCID string)`.
  - `func (h *TaskHandler) handleForwardEnded(conn ConnHandle, msg *protocol.ForwardEnded)`.
  - `RunnerHandler.OnForwardEnded func(ConnHandle, *protocol.ForwardEnded)` and `RunnerHandler.OnRunnerDisplaced func(objproto.ConnectionID)`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestTeardownTellsRunnerWhenItHoldsState(t *testing.T)
// ClosePortForward sent for: ip×link, udp×local, udp×remote, tcp×remote; NOT for tcp×local

func TestDropRunnerStateForwards(t *testing.T)
// rows link, udp -L, tcp -R, tcp -L, all with runnerCID "A"; plus a link with runnerCID "B".
// DropRunnerStateForwards("A") -> the first three gone, each control stream got Closed{Reason: RunnerGone, Detail: ""};
// tcp -L and B's link remain

func TestRunnerDisplacementCallsOnRunnerDisplaced(t *testing.T)
// RunnerHandler.Handle(hello) on conn A, then the same RunnerId's hello on conn B
// -> OnRunnerDisplaced called once with A's ConnectionID; a hello with no previous holder -> not called

func TestForwardEndedTearsDownWithDetail(t *testing.T)
// link row with runnerCID = runnerConn's; handleForwardEnded(runnerConn, {1, "read: file descriptor in bad state"})
// -> row gone; control got Closed{EndpointFailed, "runner: read: file descriptor in bad state"}; ClosePortForward NOT required

func TestForwardEndedFromOtherConnIgnored(t *testing.T)   // Review Focus 2
// same, sent from a different conn -> row still listed, no Closed record

func TestTapForwardClosedCarriesDetail(t *testing.T)
// a tap on a row torn down with EndpointFailed + detail -> last record ForwardClosed{Reason, Detail} as passed
```

- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Every existing `teardownPortForward` / `closePortForward` call passes `""` as the detail.
  - `teardownPortForward` sends `ClosePortForward` when `runnerHoldsState()`. Keep resolving the runner by identity there: on the `runner_gone` path the message reaches a session holding nothing, which is a no-op.
  - `DropRunnerStateForwards` loops over `list()`, matching on `runnerCID` and tearing down with `notify = true`.
  - `server.go`:
    - Call it beside `DropPortForwardsForConn` in the connection-teardown block.
    - Wire `OnRunnerDisplaced` to `func(id) { taskHandler.DropRunnerStateForwards(id.String()) }`.
    - Wire `OnForwardEnded` to `handleForwardEnded`.
  - `runner_handler.go`:
    - Capture `displaced := h.Registry.Add(entry)`. Call the hook when it is not `objproto.ConnectionID{}`. This is the only `Registry.Add` caller outside tests (`grep -rn 'Registry.Add\|registry.Add' server/ | grep -v _test`). Re-check that before relying on it.
    - Add a `case protocol.RunnerMessageType_ForwardEnded`, inline, with no OnChange (same shape as the setup-result case).
- [ ] **Step 4:** Run `$GT ./server/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `server: a forward whose runner state is gone ends — runner_gone, endpoint_failed, ClosePortForward for every runner-held forward`.

---

## Task 5: Server — relay, listing and taps for every datagram forward

**Files:**
- Modify: `server/forward_datagram.go`, `server/port_forward_list.go`, `server/port_forward_registry.go`, `server/forward_tap.go`, `server/forward_counters.go`, `server/forward_events.go`
- Test: `server/forward_tap_test.go`, `server/port_forward_test.go`; `server/fakes_test.go` if `fakeConn` lacks `SendDatagram` / `MaxDatagramSize`

**Interfaces:**
- Consumes: `CarriesDatagrams`, `portForward.link` (Task 3).
- Produces:
  - `func (pf *portForward) observeDatagram(flowID uint32, dir protocol.ForwardTapDirection, fate protocol.ForwardTapDatagramFate, payload []byte)` — advances the `(flow, dir)` seq whether or not a tap is attached, then offers.
  - `func (t *forwardTap) offerDatagram(flowID uint32, dir protocol.ForwardTapDirection, seq uint64, fate protocol.ForwardTapDatagramFate, data []byte)` — applies the filter and `max_record_bytes` like `offer`, and copies the payload.
  - `func (pf *portForward) pruneDatagramSeqs(now time.Time)` — deletes entries idle ≥ `datagramSeqIdle = 60 * time.Second`.
  - `func datagramFate(err error) protocol.ForwardTapDatagramFate` — `nil` → relayed; `trsf.ErrCongestionBlocked` → congestion; `trsf.ErrDatagramTooLarge` → oversize; anything else → queue.
  - `tapStreamKey{seq uint64; dir; datagram bool}` — a datagram key holds `uint64(flowID)` in `seq`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestDatagramTapRecordsFate(t *testing.T)
// addTap; observeDatagram(3, ToTarget, Relayed, 512 bytes) -> one Datagram record {FlowId 3, ToTarget, Seq 1, Relayed, Data == payload}
// then (3, ToTarget, DroppedOversize, ...) -> Seq 2, fate DroppedOversize

func TestDatagramTapLateOpenSeesRealSeq(t *testing.T)
// 5 observes on (3, ToTarget) before addTap, 1 after -> first record Seq 6; (3, FromTarget) starts at 1

func TestDatagramTapGapIsDatagramGap(t *testing.T)
// a tap whose sink blocks; push forwardTapQueueDepth+10 datagrams on flow 3 -> after unblocking,
// a DatagramGap{FlowId 3, ToTarget, DroppedBytes > 0} precedes the next Datagram; no record of kind Gap appears

func TestDatagramTapFilterAndTruncation(t *testing.T)
// filter FromTarget drops ToTarget datagrams; max_record_bytes 16 on 100 bytes -> Data len 16, TruncatedBytes 84

func TestDatagramSeqPrunedAfterIdle(t *testing.T)
// observe at t0; pruneDatagramSeqs(t0+59s) keeps it; (t0+60s) drops it; the next observe restarts at Seq 1

func TestPortForwardInfoIPRow(t *testing.T)
// ip×link row -> Counter(MaxDatagramSize) present, relay drop keys present and 0, Link() equals pf.link;
// tcp row -> no MaxDatagramSize, Link() nil

func TestRelayForwardDatagramIPForward(t *testing.T)
// Server{taskHandler, registry} + an ip×link row; a datagram from the client conn reaches the runner conn byte for byte
// and the tap sees Relayed; from a third conn -> nothing relayed, nothing offered
```

  The datagram tests use a `time.Time` parameter or a `now func()` field, so no test sleeps 60 s.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **Relay.** Gate on `pf.protocolKind.CarriesDatagrams()`. Decide the fate at each exit after the endpoint check, and call `pf.observeDatagram(dg.FlowId, dir, fate, dg.Payload)` once per datagram:
    - no runner, or `to == nil` → `DroppedQueue`;
    - oversize → `DroppedOversize`;
    - otherwise `datagramFate(sendErr)`.
    - The sender-not-an-endpoint and not-a-datagram-forward refusals are never offered.
  - **Gap records.** `forwardGapRecord` and `recordKey` handle the datagram kinds.
  - **Listing.** `portForwardInfo` emits the datagram group when `CarriesDatagrams()`, and calls `SetLink(*pf.link)` when `protocolKind == Ip`.
  - **Pruning.** `runStatsSweeper` calls `pf.pruneDatagramSeqs(now)` for every row on each tick. It goes in the loop, not inside `sweepForwardStats`, which returns early when nothing subscribes.
- [ ] **Step 4:** Run `$GT ./server/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `server: taps read datagram forwards; ip rows list their link and datagram counters`.

---

## Task 6: `iplink` — the device and the pump

**Files:**
- Create: `iplink/link.go`, `iplink/tun_linux.go` (`//go:build linux`), `iplink/tun_other.go` (`//go:build !linux`), `iplink/link_test.go`, `iplink/tun_linux_test.go` (`//go:build linux`)
- Modify: `go.mod` (`golang.org/x/sys` moves from indirect to direct; `go mod tidy`; no new module)

**Interfaces:**
- Produces:

```go
package iplink

// Device is one packet per Read and one packet per Write.
type Device interface {
	io.ReadWriteCloser
	Name() string
}
type Opener func(addr netip.Prefix, mtu int) (Device, error)
var ErrNotLinux = errors.New("ip links are Linux-only")
func Supported() bool                                // runtime.GOOS == "linux"
func OpenTun(addr netip.Prefix, mtu int) (Device, error)  // linux: real; elsewhere: ErrNotLinux
type Sender interface {
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}
type Link struct {
	Drops protocol.ForwardDropCounters
	// unexported: forwardID, dev, send, closed atomic.Bool
}
func NewLink(forwardID uint64, dev Device, send Sender) *Link
func (l *Link) Run(ctx context.Context) error  // device -> wire until the device fails or l closes; nil after Close or ctx end
func (l *Link) Deliver(payload []byte) error   // wire -> device, one Write
func (l *Link) Close() error                   // idempotent; marks closed BEFORE closing the device
func (l *Link) Name() string
func UpLine(name string, addr netip.Prefix, mtu int) string  // "ip link: hlink0 up, 10.200.0.1/30, mtu 1100"
```

- [ ] **Step 1: Write the failing tests** with a channel-backed fake `Device` and a recording fake `Sender`:

```go
func TestLinkCarriesPacketsBothWays(t *testing.T)
// a device packet P -> sender got AppKind_ForwardDatagram + ForwardDatagram{ForwardId 7, FlowId 0, Payload P};
// Deliver(Q) -> device read back Q

func TestLinkCountsOversize(t *testing.T)    // MaxDatagramSize 100, packet 200 bytes -> Drops oversize 1, nothing sent
func TestLinkCountsCongestion(t *testing.T)  // sender returns trsf.ErrCongestionBlocked -> Drops congestion 1
func TestLinkRunReturnsNilAfterClose(t *testing.T)  // Review Focus 1: Close while Run blocks -> Run returns nil
func TestLinkRunReturnsDeviceError(t *testing.T)    // device Read -> errors.New("boom") -> Run returns an error wrapping "boom"
func TestUpLine(t *testing.T)                // "ip link: hlink0 up, 10.200.0.1/30, mtu 1100"
func TestOpenTunWithoutCapabilityIsEPERM(t *testing.T)
// linux only; t.Skip when os.Geteuid() == 0 or /dev/net/tun is absent;
// OpenTun(10.200.0.1/30, 1100) -> errors.Is(err, unix.EPERM)
```

- [ ] **Step 2:** Run `$GT ./iplink/ -count=1`. Expected: FAIL (package empty).
- [ ] **Step 3: Implement.**
  - **`Run`.** Read into a 65535-byte buffer. Frame with `ForwardDatagram.Append([]byte{byte(appwire.AppKind_ForwardDatagram)})`. If longer than `MaxDatagramSize()`, call `Drops.NoteOversize()`. Otherwise call `Drops.NoteSendError(send.SendDatagram(b))`, the congestion-controlled mode (spec §7c).
  - **Error return.** A read error after `closed` is set returns nil. Any other read error returns `fmt.Errorf("tun read: %w", err)`.
  - **`tun_linux.go`, spec §8.**
    - Open with `unix.Open("/dev/net/tun", O_RDWR|O_CLOEXEC, 0)`.
    - `unix.NewIfreq("hlink%d")`, `SetUint16(IFF_TUN|IFF_NO_PI)`, then `unix.IoctlIfreq(fd, TUNSETIFF, ifr)`. Read the kernel-picked name with `ifr.Name()`.
    - On an `AF_INET` `SOCK_DGRAM` socket (closed before returning): `SIOCSIFADDR` via `SetInet4Addr`, `SIOCSIFNETMASK` from the prefix length, `SIOCSIFMTU` via `SetUint32`, then `SIOCGIFFLAGS` followed by `SIOCSIFFLAGS` with `IFF_UP` added.
    - Wrap each ioctl error as `fmt.Errorf("TUNSETIFF: %w", err)` (and likewise per call), so `errors.Is(err, unix.EPERM)` holds and the text names the call.
    - Then `unix.SetNonblock(fd, true)` and `os.NewFile`. Non-blocking puts the fd on Go's poller, so `Close` unblocks a pending `Read`; without it `TestLinkRunReturnsNilAfterClose` passes on the fake and Ctrl-C hangs on the real device.
  - **`tun_other.go`** returns `nil, ErrNotLinux`.
- [ ] **Step 4:** Run `$GT ./iplink/ -count=1`, `GOOS=windows go build ./iplink/` and `GOOS=js GOARCH=wasm go build ./iplink/`. Expected: PASS, success, success.
- [ ] **Step 5:** Commit: `iplink: a tun device and the pump that carries it`.

---

## Task 7: Runner — the link end, and releasing every datagram forward on disconnect

**Files:**
- Create: `runner/ip_link.go`, `runner/ip_link_test.go`
- Modify: `runner/port_forward.go`, `runner/forward_datagram.go`, `runner/connect.go`, `runner/session.go`, `runner/trsf_state.go`

**Interfaces:**
- Consumes: `iplink.*` (Task 6); `IpLinkConfig.RunnerPrefix/Validate` (Task 2).
- Produces:
  - `Config.TunOpener iplink.Opener` (nil means `iplink.OpenTun`), copied into `Session.openTun`.
  - `type ipLinks struct{ mu sync.Mutex; m map[uint64]*iplink.Link }` with `add`, `get`, `remove(id) *iplink.Link`, `closeAll()`, `dropsFor(id) (protocol.ForwardDropsBody, bool)`. The session accessor is `ipLinkRegistry()`.
  - `func (s *Session) startIPLink(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest)`.
  - `func (s *Session) endIPLink(id uint64, err error)` — remove, close, send `ForwardEnded`.
  - `func (s *Session) sendForwardEnded(forwardID uint64, detail string)`.
  - `func (s *Session) releaseForwardState()` — `rforwards.closeAll()`, `udpForwards.closeAll()`, `ipLinks.closeAll()`. `RunHandle.Close` calls only this.
  - `func (u *udpForwards) closeAll()` — closes every flow socket and every `-R` listening socket, empties both maps, and closes `u.done` so `sweep` returns.

- [ ] **Step 1: Write the failing tests.** Reuse the fake `Sender` that `runner/port_forward_test.go` already uses for `-R` bind results.

```go
func TestStartIPLinkEPERMAnswersSetupFailed(t *testing.T)
// openTun returns fmt.Errorf("TUNSETIFF: %w", unix.EPERM)
// -> ForwardSetupResult{ok 0, Detail "TUNSETIFF: operation not permitted"}; registry empty

func TestStartIPLinkNotLinuxDetail(t *testing.T)
// openTun returns iplink.ErrNotLinux -> Detail "ip links are Linux-only (this runner is " + runtime.GOOS + ")"

func TestStartIPLinkUnknownTask(t *testing.T)   // -> ok 0, Detail "unknown task"; opener never called

func TestIPLinkCarriesBothWays(t *testing.T)
// fake device; ok 1 sent; device opened with RunnerPrefix() and mtu 1100;
// handleForwardDatagram(fid) -> device got payload; device packet -> Sender got ForwardDatagram{fid, 0, pkt}

func TestIPLinkCloseSendsNoForwardEnded(t *testing.T)   // Review Focus 1
// ClosePortForward{fid} via the connect.go dispatch path -> device closed, registry empty, no ForwardEnded sent

func TestIPLinkDeviceFailureSendsForwardEnded(t *testing.T)
// device Read returns errors.New("boom") -> ForwardEnded{fid, Detail contains "boom"}; registry empty; a later ClosePortForward is a no-op

func TestReleaseForwardStateClosesDatagramForwards(t *testing.T)
// a udp -R bound on 127.0.0.1:0, a udp -L with one live flow, a link on a fake device
// -> releaseForwardState() -> the -R socket's port can be bound again, the flow socket is closed, the device is closed,
//    dropsFor/ForwardDrops answer false for all three, and sweep(ctx) returns although ctx is live

func TestForwardDropsFindsIPLink(t *testing.T)   // sessionTelemetry.ForwardDrops(fid) -> ok for a link id
```

- [ ] **Step 2:** Run `$GT ./runner/ -run 'IPLink|ReleaseForwardState|ForwardDropsFinds' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **`handleOpenPortForward`.** Branch on `Direction == Link` first.
  - **`startIPLink` setup.**
    - Check the task, then `Link() != nil && Validate() == nil`. A failed check sends detail `malformed link request: <err>`.
    - Open the device. Wrap `ErrNotLinux` as the GOOS text; any other error passes through as `err.Error()`.
    - Register the link, `sendSetupResult(ok)`, and log `UpLine` with the forward id.
  - **`startIPLink` pump.** `go func() { if err := l.Run(ctx); err != nil { s.endIPLink(id, err) } }()`.
  - **`handleForwardDatagram`.** Look up `ipLinkRegistry()` first. A `Deliver` error calls `endIPLink`.
  - **`ClosePortForward` dispatch.** Also calls `ipLinkRegistry().remove(id)` and closes it. `Close` sets `closed` first, so its `Run` returns nil and no `ForwardEnded` goes out.
  - **Setup failure details.** Every `-R` failure path now passes a reason: the `net.Listen` / `ListenUDP` error text, `unknown task`, `no stream creator wired`, `no sender wired`.
  - **`connect.go`.**
    - `RunHandle.Close` calls `session.releaseForwardState()`.
    - Rewrite the comment above `go session.udpForwardRegistry().sweep(ctx)` to state the facts: `ctx` is PersistLoop's and outlives the connection, so the sweep stops on `closeAll` instead. Spec §7b; read `cli/persist.go`'s `dial(ctx)` first to confirm.
  - **Telemetry.** `sessionTelemetry.ForwardDrops` checks the udp registry, then `s.ipLinks`. Reading the fields directly rather than through the lazy accessors keeps the existing comment's reasoning.
- [ ] **Step 4:** Run `$GT ./runner/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `runner: the link end of an ip forward; every datagram forward is released when its connection ends`.

---

## Task 8: Client library — register, run, route

**Files:**
- Create: `cli/ip_link.go`, `cli/ip_link_test.go`
- Modify: `cli/port_forward.go`, `cli/forward_datagram.go`

**Interfaces:**
- Consumes: `iplink.*`; `protocol.NewIpLinkConfig`.
- Produces:

```go
func ParseIPLink(local, peer string) (protocol.IpLinkConfig, error)  // --ip "10.200.0.1/30", --ip-peer "10.200.0.2"
func ParseIPLinkLine(s string) (protocol.IpLinkConfig, error)        // TUI: "<client-addr>/<prefix> <runner-addr>"
func IPLinkSpecString(c *protocol.IpLinkConfig) string               // "ip 10.200.0.1/30 <-> 10.200.0.2"
type IPLinkOptions struct {
	Open         iplink.Opener     // nil = iplink.OpenTun
	OnRegistered func(id uint64)
}
func RunIPLink(ctx context.Context, c *Client, taskIDHex string, cfg protocol.IpLinkConfig,
	logf func(string), opts IPLinkOptions) error
func (c *Client) RegisterIPLink(ctx context.Context, taskIDHex string, cfg protocol.IpLinkConfig) (trsf.BidirectionalStream, uint64, error)
type ForwardClosedError struct {
	Reason protocol.PortForwardCloseReason
	Detail string
} // Error() == closedReasonLine
func registerResponseError(r *protocol.RegisterPortForwardResponse) error  // nil for Ok
```

  - `serveForwardControl` now returns `*protocol.PortForwardClosed`: the record when one arrived, nil otherwise. Existing callers ignore it.
  - `closedReasonLine` takes `*protocol.PortForwardClosed`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestParseIPLink(t *testing.T)   // Review Focus 4
// "10.200.0.1/30","10.200.0.2" ok;
// "10.200.0.1","10.200.0.2"   -> `--ip wants <address>/<prefix>, got "10.200.0.1"`;
// "10.200.0.1/30","x"         -> `--ip-peer wants an address, got "x"`;
// "fd00::1/64","fd00::2"      -> "ipv6 link addresses are not implemented";
// "10.200.0.1/32","10.200.0.2" -> the Validate text (same function as the server)

func TestParseIPLinkLine(t *testing.T)   // Review Focus 4
// "10.200.0.1/30 10.200.0.2" and "  10.200.0.1/30\t 10.200.0.2  " ok, equal to ParseIPLink's;
// "10.200.0.1/30" and "a b c" -> "want <client-addr>/<prefix> <runner-addr>"

func TestIPLinkSpecString(t *testing.T)   // "ip 10.200.0.1/30 <-> 10.200.0.2"

func TestRegisterResponseError(t *testing.T)
// LinkFailed + "runner: x"           -> "forward: ip link failed: runner: x";
// BindFailed + "runner: x"           -> "forward: runner failed to bind the listen port: runner: x";
// BindFailed + ""                    -> "forward: runner failed to bind the listen port";
// InternalError + "invalid ip link: y" -> "forward: server refused: invalid ip link: y";
// existing NoSuchTask / RunnerOffline texts unchanged

func TestClosedReasonLine(t *testing.T)
// Killed -> "forward stopped: killed remotely"; TaskGone -> "forward stopped: task is no longer running";
// RunnerGone -> "forward stopped: the runner's connection ended";
// EndpointFailed + "runner: boom" -> "forward stopped: the runner's end failed: runner: boom"

func TestDispatchDatagramRoutesToItsLink(t *testing.T)   // Review Focus 3
// two registered links (ids 7, 8) on fake devices; a datagram naming 8 -> device 8 got it, device 7 did not;
// after unregistering 8 the same datagram is dropped silently;
// a device whose Write fails -> that entry's fail func is called once with the error

func TestForwardDropsFindsIPLink(t *testing.T)
```

- [ ] **Step 2:** Run `$GT ./cli/ -run 'IPLink|RegisterResponseError|ClosedReasonLine|RoutesToItsLink|ForwardDropsFindsIPLink' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **Shared registration.** Move `RegisterPortForward`'s body into `c.registerForward(ctx, body protocol.RegisterPortForwardRequest)`. Both `RegisterPortForward` and `RegisterIPLink` call it; `RegisterIPLink` sets `Protocol: Ip` and `Direction: Link` before `SetLink`. The status switch becomes `registerResponseError`, keeping the existing pickup-failure kill.
  - **`RunIPLink`, spec §7a:**
    1. Open the device with `cfg.ClientPrefix()` and `int(cfg.Mtu)`. On error return `fmt.Errorf("ip link: %w", err)`; nothing is registered.
    2. `logf(UpLine)`.
    3. `RegisterIPLink`. On error close the device and return the error.
    4. `NewLink(fid, dev, c.conn)`; add it to the process-wide registry, beside the two udp ones; defer unregister and `Close`.
    5. Run `l.Run(ctx)` in a goroutine. A non-nil error logs `ip link: <err>` and cancels the control loop's context.
    6. `serveForwardControl`. Return `&ForwardClosedError{…}` for a closed record, the tun error if that is what ended it, nil on the caller's ctx, and `errors.New("forward stopped: server connection lost")` on EOF.
  - **`dispatchDatagram`.** Looks up the link registry first. A registry entry holds the link and a `fail func(error)` that does what a `Run` error does: log `ip link: <err>` and cancel. A `Deliver` error calls `fail`, so a tun write failure ends the link the same way a read failure does (spec §7c).
  - **`forwardDrops`.** Searches it too.
- [ ] **Step 4:** Run `$GT ./cli/ -count=1` and `GOOS=js GOARCH=wasm go build ./cmd/harness-webui-wasm/`. Expected: PASS, success.
- [ ] **Step 5:** Commit: `cli: RunIPLink — tun, register, pump; register errors carry the runner's detail`.

---

## Task 9: Every renderer

**Files:**
- Modify: `cli/port_forward_list.go`, `cli/forward_snapshot_row.go`, `cli/forward_tap_render.go`, `cli/forward_tap.go`, `tui/portforward.go`
- Test: `cli/port_forward_list_test.go`, `cli/forward_protocol_render_test.go`, `cli/forward_tap_render_test.go`, `tui/portforward_test.go`

**Interfaces:**
- Consumes: `IPLinkSpecString`, `CarriesDatagrams`.
- Produces: `type TapRenderer struct{ … }`, `func NewTapRenderer(mode TapRenderMode) *TapRenderer`, `func (r *TapRenderer) Render(rec *protocol.ForwardTapRecord) []string`. It replaces `RenderTapRecord`, because the missed-datagrams line needs the previous seq per `(flow, dir)`. Both call sites are in `cli/forward_tap.go` and hold one renderer per stream. Update the doc comments that name `RenderTapRecord`: `cli/forward_tap.go`, `tui/tap_pump.go`, `cmd/harness-webui-wasm/main.go`.

- [ ] **Step 1: Write the failing tests.** `ipRow()` is a link row with link `10.200.0.1/30 ↔ 10.200.0.2`, 1 KiB each way, the datagram group at zero and mtu 1169.

```go
func TestPortForwardSpecStringIPLink(t *testing.T)  // "ip 10.200.0.1/30 <-> 10.200.0.2"
func TestPortForwardDirFlagLink(t *testing.T)       // "--ip"; the DIR column is widened so "--ip" aligns
func TestTrafficLineIPLinkHasNoConns(t *testing.T)
// no "conns=" in the line; has "to-target=1.0KiB", "mtu=1169", "relay: oversize=0 congested=0 queued=0"
// (match FormatByteCount's real output)
func TestDatagramCellIPLink(t *testing.T)           // "1169 0/0/0"
func TestConfigSpecRefusesLink(t *testing.T)        // ok == false although ClientEndpoint == OsSocket
func TestPortForwardJSONLink(t *testing.T)
// ip row -> "link":{"client_addr":"10.200.0.1","runner_addr":"10.200.0.2","prefix_len":30,"mtu":1100}; tcp row -> no "link" key
func TestForwardSnapshotRowLink(t *testing.T)       // "link" map present on the ip row, absent on tcp
func TestTapRendererDatagram(t *testing.T)
// Datagram{3, ToTarget, Seq 41, Relayed, 512B} -> header == tapHeader("flow 3", "->", ts, "#41 relayed 512B") then the hex body;
// TapText / TapRaw / TapJSON forms ("kind":"datagram","flow":3,"seq":41,"fate":"relayed",...)
func TestTapRendererMissedDatagrams(t *testing.T)
// seq 41 then 49 on (3, ->) -> a line tapHeader("flow 3", "->", ts, "missed 7 datagrams") before 49's header;
// 41 then 42 -> no such line; a different flow or direction does not count as a jump
func TestTapRendererDatagramGap(t *testing.T)       // tapHeader("flow 3", "gap", ts, "<bytes> missed ->")
func TestTapCloseReasons(t *testing.T)
// ForwardClosed{EndpointFailed, "runner: boom"} -> "... closed (endpoint failed: runner: boom)";
// {RunnerGone, ""} -> "closed (runner gone)"
func TestForwardsModalLinkRowConnsEmpty(t *testing.T)  // tui: the conns cell is "" for a link row, "0/0" for an idle tcp row
```

  Fate words: `relayed`, `dropped-oversize`, `dropped-congestion`, `dropped-queue`. Truncation appends the same `(truncated, X cut)` the data record uses.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The `conns=` segment is omitted on `Direction == Link`, an existence gate (memory: never gate on the value). The same gate empties the TUI conns cell.
  - `PortForwardDatagramCell` gates on `CarriesDatagrams()`.
  - `PortForwardConfigSpec` returns false for `Direction == Link` before its endpoint check. Its comment says why: a link's endpoint is `os_socket`, and the refusal is on the direction.
  - The spec string for an ip row whose `Link()` is nil (a newer peer's shape) is `ip (no link config)`.
- [ ] **Step 4:** Run `$GT ./cli/ ./tui/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `cli, tui: link rows and datagram tap records on every surface`.

---

## Task 10: The `forward --ip` verb, and the docs that list capabilities

**Files:**
- Modify: `cli/verb/table.go`; regenerate `cli/verb/actions_gen.go` (`go generate ./cli/verb/`)
- Modify: `cmd/harness-cli/dispatch.go`
- Test: `cli/verb/rules_test.go`, `cmd/harness-cli/forward_test.go`
- Modify: `runner/agentskills/supervising-workers/SKILL.md` and its mirrors `.claude/skills/supervising-workers/SKILL.md` and `.agents/skills/supervising-workers/SKILL.md` (`TestMirrorsMatchEmbeddedSkills` holds them byte-equal), `README.md` (the forward section)

**Interfaces:**
- Consumes: `cli.ParseIPLink`, `cli.RunIPLink`, `iplink.Supported`.
- Produces: `ForwardOpenAction.IP`, `ForwardOpenAction.IPPeer` (generated).

- [ ] **Step 1: Write the failing tests** in `cli/verb/rules_test.go`'s two tables:
  - Refused: `-L 1:h:2 --ip a/30 --ip-peer b` → "mutually exclusive". The same with `-R` and with `-W`.
  - Refused: `--ip a/30` alone → requires `ip-peer`; `--ip-peer b` alone → requires `ip`.
  - Refused: no flags → the at-least-one text now lists `--ip`.
  - Accepted: `--ip 10.200.0.1/30 --ip-peer 10.200.0.2`.
  - In `cmd/harness-cli/forward_test.go`: `forward <id> --ip 10.200.0.1/30 --ip-peer 10.200.0.2` parses to `ForwardOpenAction{IP: "10.200.0.1/30", IPPeer: "10.200.0.2"}`. So does the same flags-before-id spelling (ParsePermuted).
- [ ] **Step 2:** Run `$GT ./cli/verb/ ./cmd/harness-cli/ -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **`table.go`, spec §10a.**
    - Flags: `{Name: "ip", Type: FlagString, Default: "", Field: "IP", Help: "ip link: this client's tun address with the shared prefix, e.g. 10.200.0.1/30 (Linux; needs forward_link)"}` and `{Name: "ip-peer", …, Field: "IPPeer", Help: "ip link: the runner's tun address, inside --ip's prefix"}`.
    - `Exclusive`: `ip` with each of `L`, `R`, `W`. Reason: `a refused link must not leave other forwards running`.
    - `AtLeastOne`: `L R W ip`.
    - `Requires`: `ip-peer` needs `ip`, and `ip` needs `ip-peer`.
    - `Notes`: `--ip/--ip-peer: a tun device at each end carrying whole IP packets (Linux only, MTU 1100); exits non-zero when the link is closed`.
    - `ModalSurfaces`: `{Surface: TUI, At: "tui/portforward.go:DoStartIPLink"}`. Task 11 defines it; `modal_surface_test.go` resolves `At`, so this row fails until Task 11 lands. Either add the row in Task 11 or run Task 11 before this test step. **Chosen: the row is added in Task 11.**
    - Run `go generate ./cli/verb/`.
  - **`dispatch.go` `Forward`.** When `a.IP != ""`:
    - If `!iplink.Supported()`, return `errors.New("ip links are Linux-only")` before `h.dial()`.
    - Run `cli.ParseIPLink` before dialling too.
    - Dial, then call `cli.RunIPLink(fctx, c, a.TaskID, cfg, logf, cli.IPLinkOptions{})` and return its error. A `*ForwardClosedError` exits non-zero through the existing error path; `nil` after Ctrl-C exits 0.
  - **Docs.**
    - Add `forward_link` to the capability list in both `SKILL.md` copies. Mirror the `forward_tap` paragraph: not implied by `forward_local` / `forward_remote`, and why.
    - Add `--ip` to README's forward section. One example; state "Linux only, needs CAP_NET_ADMIN" (inside a user+net namespace is enough).
- [ ] **Step 4:** Run `$GT ./cli/verb/ ./cmd/harness-cli/ -count=1` and `make check`. Expected: PASS, success.
- [ ] **Step 5:** Commit: `harness-cli: forward --ip / --ip-peer`.

---

## Task 11: TUI — `L` starts a link

**Files:**
- Modify: `tui/keys.go`, `tui/actions.go`, `tui/portforward.go`, `tui/overlays.go`, `cli/verb/table.go` (the ModalSurfaces row from Task 10)
- Test: `tui/keys_test.go` / `tui/keys_dispatch_test.go`, `tui/portforward_test.go`

**Interfaces:**
- Consumes: `cli.ParseIPLinkLine`, `cli.RunIPLink`, `cli.ForwardClosedError`.
- Produces:
  - `ForwardLink` (the third `ForwardDirection`); its `flag()` is `"--ip"`.
  - `mainKeys.ForwardLink = "L"`.
  - `func DoStartIPLink(c *cli.Client, taskID, line string, id int, program *tea.Program) tea.Cmd`.

- [ ] **Step 1: Write the failing tests.**
  - Pressing `L` with a task selected opens `PortForwardModal` with `Mode() == ForwardLink` and placeholder `<client-addr>/<prefix> <runner-addr>  e.g. 10.200.0.1/30 10.200.0.2`.
  - `L` with no task gives `forward: no task selected`.
  - The existing "no duplicate main keys" test still passes. Run it and also `grep -n '"L"' tui/*.go` to confirm the grid's `L` is in its own dispatch (spec §10a).
  - `DoStartIPLink` with an unparsable line returns `PortForwardStatusMsg` whose line contains `want <client-addr>/<prefix> <runner-addr>`.
  - Use a real `*tea.Program` the way the existing `DoStartPortForward` tests do, or test the parse branch alone if none exist.
  - Feed real `tea.KeyMsg{Runes: []rune{'L'}}` through `App.Update`, not a direct `onForwardStart` call (memory: verify input, not just the render).
- [ ] **Step 2:** Run `$GT ./tui/ -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `keybindings`: a row `{Keys: []string{mainKeys.ForwardLink}, Scope: scopeTasks, Do: (*App).onForward, Short: "L ip link", Long: "start an ip link (a tun device at each end) for the selected task; stop it from f"}`.
  - `onForward` sends `ForwardLink` to `onForwardStart`, which opens the modal with `ForwardLink`.
  - `inPortForwardModal` Enter calls `DoStartIPLink` for that mode.
  - **`DoStartIPLink`.** Mirrors `DoStartPortForward`:
    - Parse first.
    - `program.Send(PortForwardStartedMsg{…, Direction: ForwardLink, Spec: line})`.
    - Run `cli.RunIPLink` in a goroutine with `forwardStatusLogf`, and `OnRegistered` sending `PortForwardRegisteredMsg`.
    - A non-nil error goes to `PortForwardStatusMsg{Line: forwardFailLine(taskID, err)}`; then `PortForwardStoppedMsg`.
    - Quitting the TUI cancels the ctx through the existing `activeForwards` cancel path. Check that the quit path does cancel them; if it does not, the tun lives until process exit, which is equivalent.
  - Add the `ModalSurfaces` row deferred from Task 10.
- [ ] **Step 4:** Run `$GT ./tui/ ./cli/verb/ -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `tui: L opens the forward modal in link mode`.

---

## Task 12: In-process end to end

**Files:**
- Create: `integration/ip_link_test.go` (build tag as the neighbouring files)
- Modify: `integration/udp_forward_test.go` (one tap assertion)

- [ ] **Step 1: Write the tests.** Copy the server, runner and task setup from `TestUDPPortForwardE2E` (own port, e.g. `18563`).
  - **`TestIPLinkE2E`.**
    - The runner gets `Config.TunOpener` returning a fake device. The client calls `cli.RunIPLink` with `IPLinkOptions{Open: fake}`. The fake is a pair of channels, not a kernel device.
    - **Round trip.** A packet written into the client fake comes out of the runner fake unchanged, and back.
    - **Listing.** `forward ls` shows `ip 10.200.0.1/30 <-> 10.200.0.2`, `--ip`, and the datagram group.
    - **Tap.** An open tap sees a `Datagram` record `relayed` per packet each way.
    - **Kill.** `KillPortForwardWith` makes `RunIPLink` return `*ForwardClosedError{Reason: Killed}`, and both fakes are closed.
    - **Setup failure.** A second link whose runner opener returns `EPERM` makes `RunIPLink` return an error containing `forward: ip link failed: runner: ` and `operation not permitted`.
  - **`TestUDPForwardTapShowsDatagrams`** (Review Focus 5). In `TestUDPPortForwardE2E`, open a tap before the round trip and assert at least one `Datagram` record each way.
- [ ] **Step 2:** Run `env -u … go test -tags integration ./integration/ -run 'IPLink|UDPPortForwardE2E' -count=1`. Expected: PASS. A failure here is a defect in Tasks 3–9; fix it in the owning code, not in the test.
- [ ] **Step 3:** Commit: `integration: an ip link end to end on fake devices; a tap on a udp forward`.

---

## Task 13: The milestone — netem-lab, wire skew, full suite

No product code unless a step fails.

- [ ] **Step 1: Full suite.** `make check`, `make vet`, `make wasm-check`, `make js-test`, then `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` and `make test-integration` with the same env. Expected: green. A red known-flaky server test (`TestOpenInteractive*`, `TestHandleAttachSession_ViewMode*`) is re-run once in a separate process before it counts.
- [ ] **Step 2: Wire skew.** `scripts/wire-skew-check.sh`. Expected: PASS (a new runner against an old server is rejected and recovers). It must report that the skew was exercised; exit 2 is a setup error, not a pass.
- [ ] **Step 3: Build what the lab will run.** Run `make build` in this worktree. Read `scripts/netem-lab/README.md` for how the lab picks its binaries, and make it run THIS worktree's `bin/`. A bare `harness-cli` in a worktree runs the PARENT's build. Prove which build runs by checking `bin/` mtimes against the commit.
- [ ] **Step 4: Spec §12 acceptance steps 1–9, in order.** Record each step's command and its observed output (device names, ping RTT before and after `shape`, tap lines, `ip link` listings) in `## Appendix B — acceptance` of this plan.
  - A failing step stops the walk. Diagnose with superpowers:systematic-debugging, fix in the owning task's files with a test, and restart from step 1.
  - `iperf3` (step 4): check `command -v iperf3` inside the lab. If it is absent, record that; do not install it.
  - Step 8 drives the real TUI with keystrokes.
- [ ] **Step 5:** Commit: `plan: IP link — acceptance recorded`. Then tell the operator stage 1 is ready to land. Landing (local main, FF, push, then `make build` in the main checkout) follows the landing-to-main skill once they have seen Appendix B.

---

## Appendix A — surface parity walk (2026-10-25, before implementation)

Items 1–39 of `.claude/skills/surface-parity-checklist/SKILL.md`, one verdict each.

1. done (T10) — `forward`'s row gains `ip` / `ip-peer`. The verb is `CmdlineSurfaces: CLI`, so no other command line parses them.
2. done (T10, T8) — pairing rules are declared (`Exclusive`, `Requires`, `AtLeastOne`). Address validity is `cli.ParseIPLink` → `IpLinkConfig.Validate`, which the server runs too.
3. see 4, 5, 6, 9.
4. done (T11) — `mainKeys.ForwardLink = "L"` plus its `keybindings` row (keys_test enforces the pair).
5. done (T11) — `PortForwardModal` gains `ForwardLink` mode.
6. omitted — the WebUI cannot open a tun (spec §10a). Listing and kill already exist and render through `cli` (T9).
7. n/a — no new WebUI-typable path (`forward` open is CLI-only).
8. n/a — no bridge function interprets these values.
9. n/a — not a spawn.
10. noted — CLI stdout/exit, TUI `PortForwardStatusMsg`, no WebUI.
11–14. n/a — no task field. The forward listing surfaces they do not name are walked under 31 and 39.
15. done (T3) — `GrantableCaps`, `CapDescription`, `capAuditTable` gain `forward_link`. `WriteCaps` iterates `GrantableCaps`, so the catalog follows.
16–18a. n/a — no task or runner field.
19–23. n/a — no task field. The wasm forward row is `cli.ForwardSnapshotRow` (T9), which gains `link`.
24. done — `--ip` exists only on forward open. On the other paths a link row is: killed under `forwardCapFor` (T3); tapped like any datagram row (T5); omitted from a workspace save (T9); listed everywhere (T9).
25. done (T2) — `link` is present exactly when `protocol == ip`. No zero-versus-absent ambiguity.
26. n/a — not a spawn.
27. done (T8) — one `registerForward` funnel for `RegisterPortForward` and `RegisterIPLink`. The CLI and the TUI both call `cli.RunIPLink`.
28. n/a — forwards are in memory only (spec §5).
28a. done — `RegisterPortForwardRequest{` has one literal (`cli/port_forward.go`). `RunnerOpenPortForwardRequest{` has three (`server/port_forward.go`), and only `registerRemoteForward`'s carries a link (T3).
29. done (T8, T11) — the up line names the device, address and mtu. The exit line names reason and detail. The TUI fail line names the task.
30. done (T11) — the TUI result goes to `a.cmdresult` via `PortForwardStatusMsg`. WebUI n/a.
31. done (T9) — `conns=` is omitted on `direction == link` (existence), and the datagram counters print their zeros.
32. done (T8) — one parser (`ParseIPLink`, which `ParseIPLinkLine` calls). One renderer (`IPLinkSpecString`). A link has no workspace grammar.
33. done (T10) — `--ip` without `--ip-peer` and the reverse refuse. `--ip` with `-L` / `-R` / `-W` refuses.
34. n/a — no column set changes. The conns cell is empty, not removed.
34a. n/a — no WebUI form.
35. done (T10) — README's forward section. The README's TUI cmdline verb list: n/a (no cmdline change).
36. done (T10) — `runner/agentskills/supervising-workers/SKILL.md` plus BOTH mirrors, `.claude/skills/` and `.agents/skills/`. The plan named only one mirror; see the T1 ruling.
37. at the end (T13) — anything implementation changes in the spec's meaning lands as an amendment there.
38. n/a — no screen-rendering change.
39. at the end (T13) — walk spec §10d row by row against the code.

S1–S6: n/a — no agent launch change.

**Item 39, walked at the end against spec §10d:**
- CLI `--ip` / `--ip-peer`: done.
- Workspace save omits link rows: done (`PortForwardConfigSpec`).
- TUI `L` and the modal's link mode: done.
- TUI cmdline: none, as the spec says.
- WebUI start: impossible, as the spec says; it lists and kills via `ForwardSnapshotRow`.
- CLI and JSON display: done.
- TUI spec cell, and the conns cell empty on link rows: done.
- `TapRenderer` with the new kinds and the two close reasons: done.
- `forward_link` in caps (`GrantableCaps` feeds every parser and listing): done.
- Server and runner: done.

No row is unbuilt.

## Appendix B — acceptance (2026-10-25, netem-lab `--name iplink`, binaries from this worktree's `make build`)

Before the lab, all green:
- `make check`, `make vet`, `make wasm-check`, `make js-test`;
- `make test` (126 packages ok) and `make test-integration`, both with the four `HARNESS_*` variables unset;
- `scripts/wire-skew-check.sh`: PASS, 3/3, between 09520e0b and c69ededa.

`forward --help` lists `--ip`, which shows the new code is in the binary under test. The vcs stamp names the parent commit; that is a known worktree quirk.

| step | command (abridged; `$L` = `netem-lab.py --name iplink`) | observed |
|---|---|---|
| 1 | `$L up --profile lan`; `$L exec srv -- harness-cli forward --ip 10.200.0.1/30 --ip-peer 10.200.0.2 <task>` | client `ip link: hlink0 up, 10.200.0.1/30, mtu 1100`; runner log `ip link: hlink0 up, 10.200.0.2/30, mtu 1100 fwd=1`. `exec` keeps CAP_NET_ADMIN |
| 2 | `exec srv -- ping -c5 10.200.0.2`; `exec cli -- ping -c5 10.200.0.1` | 5/5 each way, rtt avg 3.1 / 2.9 ms |
| 3 | `$L shape --profile wan-us`; ping again | rtt avg **151.3 ms** (+148): the packets cross the shaped path |
| 4 | iperf3, server on the runner's end | `lan`: 21.2 Mbit/s received. `wan-us`: 0.13 and 0.39 Mbit/s in two runs; relay `congested` rose 21 → 43 in one run. See spec Amendment 1 |
| 5 | `forward tap 1` while `ping -c5` runs | 10 lines, `flow 0 -> … #21196 relayed 84B` and `flow 0 <- …`, 5 each way, all relayed; seq continues from the flow's real count |
| 6 | `forward kill 1` | client prints `forward stopped: killed remotely` and exits **1** (re-run with `wait`); `hlink` count 0 in srv and cli; listing `(none)` |
| 7 | `kill -9` the client | client device gone at once; row and runner device gone after **61 s** (UDP peer death is noticed by the GC) |
| 8 | `harness-tui` in srv under tmux; `L`, type `10.200.0.1/30 10.200.0.2`, Enter | modal shows `--ip` and the placeholder; `ip link: hlink0 up` in the result pane; ping 3/3 each way; `q`: both devices gone at once |
| 9a | `kill -TERM` the runner | client exits 1 at once, `forward stopped: the runner's connection ended`; both devices gone |
| 9b | `kill -9` the runner, restart it with the same argv | runner's device gone at once; client told `runner_gone` after **61 s**, exit 1. Not bounded by the reconnect: a new process has a new RunnerID (spec Amendment 1) |

Two things went wrong in the run, both mine:
- A `pkill -f` matched its own shell.
- The first 9b `kill -9` hit the `netem-lab.py exec` wrapper, whose argv contains the runner's path. Two runners then ran at once and that measurement was discarded. The redo selected the PID by the process's own executable.

Cosmetic, found here: the CLI prints the close line twice — once from the control loop's log, once as the returned error.
