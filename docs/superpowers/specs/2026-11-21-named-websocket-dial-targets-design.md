# Named WebSocket dial targets for dial records — design

## Problem

The motivating layout (operator, 2026-11-21): agent-runners in Cloudflare
Containers, each behind its own Worker host name, reverse-dialed by the
server through dial records (`runner dial add`). Every public Cloudflare host
name resolves to the same few anycast addresses, and the edge routes a
connection by its TLS SNI and HTTP `Host`. The same holds for any name-based
front: an nginx or Caddy virtual host, a path-routed reverse proxy. Today the
server cannot dial a runner behind such a front, and cannot keep two of them
apart.

- **P1 — a dial carries no host name.** A candidate is resolved to an IP when
  it is parsed (`cli/candidate.go:39-40`, `objproto.ParseOption_ResolveAddr`),
  and a `ConnectionID` holds only `Transport`, `netip.AddrPort` and `ID`
  (objtrsf `objproto/objproto.go:24-28`). The WebSocket dial builds its URL
  from that address: `Location.Host` and `Origin.Host` are
  `pkt.To.Addr.String()` (objtrsf `transport/websocket.go` send loop).
  x/net/websocket dials `wss` through a `tls.Dialer` (x/net
  `websocket/dial.go`), which takes SNI from the host it dials; that host is
  an IP literal, and Go sends no SNI for an IP. A name-routed front sees
  `Host: <ip>:<port>` and no SNI.
- **P2 — two targets on one address collide in the transport.** The WebSocket
  transport keys live sockets by `netip.AddrPort` (`connectionMap`, objtrsf
  `transport/websocket.go:70-119`), and a handshake to an address that
  already has a socket goes out on that socket (objtrsf
  `transport/underlay_close.go:65-67`: "A new handshake does NOT replace an
  existing socket on its own"). Behind anycast every runner is at the same
  address, so runner B's handshake travels on runner A's socket and reaches
  runner A. A receive error on one socket calls `CloseConnectionsAt` for the
  address, which closes every objproto connection at it.
- **P3 — the dial manager matches runners by address.**
  `server/runner_dialer.go` `sameAddr` compares transport and address only.
  - Adopt (`adoptOrYield`) also requires the bound key. If every container
    carries the same runner key, the record for runner B finds runner A's
    connection at "its" address, and adopts it or yields as `duplicate`. B is
    never dialed.
  - Kick (`RecordsPointingAt`) does **not** check the key. Kicking runner A
    removes from `runner-dials.json` every record whose candidate resolves to
    A's address (`server/runner_kick_handler.go`, `RunnerDials.Remove`).
    Behind anycast, that is every record.
- **P4 — the server cannot dial `wss`.** Two gaps:
  - `runner dial add` refuses a `wss:` candidate as `invalid_candidates`:
    the server's `DialLegs` answers `ws`, `udp` and `vsock` only
    (`server/server.go:319-329`).
  - `buildEndpoint` gives the WebSocket leg no client TLS config
    (`server/server.go`, `buildEndpoint`: `WebSocketConfig{Logger, Path}`),
    and objtrsf refuses a `wss` dial with a nil TLS config ("wss dial needs
    WebSocketConfig.TLS") instead of falling back to `ws`. One-shot `server
    dial-runner wss:…`, which does not consult `DialLegs`, fails here.
- **P5 — a record cannot choose its URL path.** Every WebSocket dial uses the
  process-wide `cli.WebSocketPath` (`cli/path.go:11`, set from the server's
  `--ws-path`), which is also the server's own listen path. A front that
  routes by path cannot be used, and a path cannot differ per record.
- **P6 — a dial slower than the first handshake retransmit opens several
  sockets.** The send loop starts a dial in a goroutine when the address has
  no socket, and puts the socket in the map only after
  `websocket.DialConfig` returns (TCP, TLS and the upgrade; objtrsf
  `transport/websocket.go` send loop). objproto retransmits the handshake,
  byte for byte, through the same loop, 333 ms after the first send and
  doubling from there (objtrsf `objproto/objproto.go:1325`,
  `objproto/session.go`). Each retransmit that arrives while the first dial
  is still in progress misses the map and starts another dial, and each
  finished dial's `connMap.Set` replaces the previous entry. The runner sees
  each socket as a separate peer and answers each copy of the hello; the
  server keeps the first ack but sends on the last socket set. Read from the
  code, not observed: LAN dials finish well inside 333 ms. Behind a TLS
  front with a container that starts on demand, a dial of several seconds is
  the normal case.

## Scope

This spec covers P1–P5 for **dial records** (`runner dial add`), the
persistent reverse dial from the server, and P6 for every WebSocket dial
(the fix is in objtrsf's send loop and does not depend on the target kind).

Named targets are not added to the other places that dial:
- `agent-runner --server-cid` (forward dial). A runner hands the address it
  connected on to its agents as `HARNESS_SERVER_CID` (`runner/agentenv.go`).
  A named `ConnectionID` printed as text is a key into the server's table, not
  an address, so an agent could not dial it. Allowing it here would break the
  rule that lets non-IP `ConnectionID`s exist at all (D2).
- One-shot `server dial-runner` and `--via`. Their target travels as a binary
  `ConnID` (`DialRunnerRequest.target`, `EstablishRelayRequest.target`), which
  has no room for a URL. With `--via`, a relay runner would dial it, and the
  relay has no table.

## Decisions taken

**operator** = chosen by the human in conversation (2026-11-21). **this spec**
= chosen by the author while writing. Rows marked "this spec" are the ones
worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | A named target keeps the `ConnectionID` shape and the wire format. Its 16-byte address is `100::/64` with a 64-bit key in the low half, as vsock already does (objtrsf `objproto/vsock_addr.go`). The URL the key stands for lives in a table held by the WebSocket leg. Alternatives considered and not taken: a string or tagged-union address in `ConnectionID` (changes the struct, 44 non-test `.Addr` uses in objtrsf, and the wire `ConnID` that runner registration rides), and an IP→host-name table in the leg (anycast maps many names to one IP) | operator ("namedのあれでいきますか") |
| D2 | A named `ConnectionID` exists only inside the server that registered it. It is never handed to another node or process to dial. This is the condition the vsock design rests on ("non-IP ConnIDs are never handed to another node to dial"). It holds for dial records: listen-mode agents reach the server through the runner's proxy (`HARNESS_PROXY_VIA_RUNNER`, set by `ListenAndServe`, `runner/connect.go:129`), and the only `ConnID` the server hands a client to dial is `DialAt` for the direct data plane, which requires udp on both ends (`server/dataplane.go:121`, `server/file_transfer.go:238-240`) | this spec |
| D3 | `server dial-runner` and `--via` refuse a named target with the existing `DialRunnerStatus_InvalidTarget`, before dialing. Without the refusal, a cid copied from `ls` would dial when the server happens to hold its entry and fail when it does not | this spec |
| D4 | Candidate grammar. A dial-record candidate is either the existing form (`transport:host:port-id`, unchanged) or a URL `ws://host[:port][/path]` or `wss://host[:port][/path]`. The URL is rejected if it has user info, a query, a fragment, an empty host, or a scheme other than `ws`/`wss`. The server stores it in canonical form: scheme and host lower-cased, the port omitted when it is the scheme default (80 for `ws`, 443 for `wss`), and an empty path replaced by the server's `cli.WebSocketPath`. `runner dial ls` shows that stored text, and `duplicate_candidate` compares it | this spec |
| D5 | The key is the first 8 bytes of SHA-256 over the canonical URL text. The port in the `AddrPort` is 0, and `Transport` is the URL scheme (`ws` or `wss`). A key derived from the text, not allocated, gives the same `ConnectionID` address for the same candidate after a server restart, and in every process that computes it | this spec |
| D6 | Text form: `<transport>:@<16 lowercase hex>-<id>`, for example `wss:@3f2a9c0d11b7e604-4711`. `ConnectionID.String()` prints it for a `ws`/`wss` address in `100::/64` with port 0, and `ParseConnectionID` parses it back, with no DNS lookup. `@` cannot occur in an IP literal or a host name, so the form does not collide with the existing grammar. The round trip keeps `cid=` from `ls` usable in the `by_conn_id` selector | this spec |
| D7 | The table is a `*transport.WebSocketTargets`, created by the caller and passed in `WebSocketConfig.Targets` when the leg is built. This follows the vsock rule that a dial target that does not fit 16 bytes goes to the leg's constructor, never into the transport string. Entries are added at run time with `Register` and not removed. An entry is a few dozen bytes, and the table holds at most the candidates added since the server started; removing one safely would mean tracking which live connections still use its address, for no gain at that size | this spec |
| D8 | Dialing a named address: the send loop looks the key up in `Targets`. `Location` is the registered URL; `Origin` is `http://` or `https://` plus the same host. The leg's TLS config is used as is: `tls.Dialer` takes SNI from `Location`'s host, which is now the name. Certificate verification stays off, as for IP targets (`cli/tls_client.go`: objproto authenticates the peer). A key with no entry, a nil `Targets`, or a URL scheme that differs from `pkt.To.Transport` is logged and reported with `CannotSend`. There is no fallback to dialing the address | this spec |
| D9 | The server's WebSocket leg gets `TLS: cli.ClientTLSConfig()` in `buildEndpoint`, and `DialLegs` answers `wss` with the same condition as `ws` (`cfg.Addr != ""`). This fixes P4 for IP candidates too | this spec |
| D14 | (P6, found by the builder check.) The WebSocket send loop keeps one dial in flight per address. While a dial to an address is in progress, a handshake packet for that address is dropped with a debug log; objproto retransmits it, and once the socket is in the map the retransmit goes out on it. Any other packet for that address is reported with `CannotSend`, as for an address with no socket today. Holding packets until the dial finishes would deliver sooner, but it adds a queue, its bound and its drain; dropping costs at most one retransmit interval after the dial completes | this spec |
| D10 | The dial manager resolves a URL candidate by registering it in the server's `Targets` and returning the named `ConnectionID` with a random ID. Other candidates still go through `cli.ResolveCandidate`. This is a server-side `Resolve` passed in `dialManagerConfig.Resolve`, which already exists for injection. `cli.ResolveCandidate` and `agent-runner --server-cid` do not accept URLs (Scope) | this spec |
| D11 | `RecordsPointingAt` matches by address only when the record's bound key equals the runner's key, the condition adopt already applies. Named addresses fix P3 for named candidates by themselves; this closes the same hole for IP candidates, where two runners reachable at one IP:port (one NAT, one proxy) would otherwise lose each other's records on a kick | this spec |
| D12 | `RunnerInfo` gains `link_target`: the candidate text that answered, for a runner whose link is `dial_record`. It is empty for every other link. Surfaces print it, so a named runner reads as its URL, not as `@<hex>` | operator (accepted the proposal, 2026-11-21) |
| D13 | Verification runs on this machine with a path-routing TLS reverse proxy on `localhost`. It reproduces "several runners behind one IP:port, told apart only by what the dial sends" without DNS changes. Running against real Cloudflare needs a Workers Paid plan, which the operator has not decided on, and is not part of this work | this spec |

## Non-goals

- Running or testing on real Cloudflare Containers, or providing a Worker /
  Durable Object that forwards to a container (D13).
- Named targets in `agent-runner --server-cid`, `server dial-runner`, or
  `--via` (Scope, D3).
- Named UDP or vsock targets. A UDP reply is matched by its source IP:port,
  so a name would need the same table inside the UDP leg, with a new problem
  when one name resolves to several IPs.
- Verifying the server certificate on `wss` dials (D8).
- Removing table entries (D7).

## 1. objtrsf

### 1.1 Address (`objproto`)

A new file beside `vsock_addr.go`:

- `NamedAddrPort(key [8]byte) netip.AddrPort`: `100::/64` with `key` in the
  low 64 bits, port 0. Same prefix bytes as `vsockPrefix`. vsock and named
  addresses can share the prefix because `Transport` is part of the
  `ConnectionID` key (vsock design rule) and the two never share a transport.
- `NamedFromAddrPort(ap netip.AddrPort) (key [8]byte, ok bool)`.
- `ConnectionID.String()`: for `Transport` `ws` or `wss` and an address for
  which `NamedFromAddrPort` is ok, print `<transport>:@<hex>-<id>` (D6).
- `ParseConnectionID`: when the address part begins with `@`, parse exactly
  16 hex digits into a named address. Anything else after `@` is an error.
  Allowed for `ws` and `wss` only.

### 1.2 Target table (`transport`)

In a file without build constraints, so `WebSocketConfig` still compiles for
wasm:

```go
// WebSocketTarget is a dial target the address cannot hold: the URL a named
// ConnectionID stands for.
type WebSocketTarget struct{ url *url.URL } // canonical; see ParseWebSocketTarget

// ParseWebSocketTarget validates and canonicalizes raw (D4, minus the default
// path, which is the caller's convention). It does not register anything.
func ParseWebSocketTarget(raw string) (WebSocketTarget, error)
func (t WebSocketTarget) String() string            // canonical text
func (t WebSocketTarget) Transport() string         // "ws" or "wss"
func (t WebSocketTarget) Key() [8]byte              // D5

type WebSocketTargets struct { /* mutex + map[[8]byte]WebSocketTarget */ }
func NewWebSocketTargets() *WebSocketTargets
// Register stores t and returns the address that stands for it.
func (s *WebSocketTargets) Register(t WebSocketTarget) netip.AddrPort
func (s *WebSocketTargets) Lookup(key [8]byte) (WebSocketTarget, bool)
```

`ParseWebSocketTarget` rejects an empty path: objtrsf owns no path
convention (`websocket_common.go`), so the harness fills its default before
calling (§2.2). `WebSocketConfig` gains `Targets *WebSocketTargets`. The wasm
implementation ignores it. The WebUI dials only the server, and a named
address there has no table to resolve it, so the browser is handed
`[100::…]:0` and the dial fails, as it would for any unreachable address.

### 1.3 Dial (`transport/websocket.go`)

- In the send loop's handshake branch, before building `websocket.Config`: if
  `objproto.NamedFromAddrPort(pkt.To.Addr)` is ok, take the URL and `Origin`
  from `Targets` as in D8. For any other address the URL is built as today.
- `connMap` keeps its `netip.AddrPort` key. Named addresses are distinct per
  target, so P2 needs no change there.
- One dial in flight per address (D14): a set of addresses being dialed,
  guarded with `connMap`'s lock or its own. The address is added before the
  dial goroutine starts and removed after `connMap.Set` or on dial failure.
  This applies to every WebSocket dial, named or not.

## 2. Harness server

### 2.1 Endpoint (`server/server.go`)

`buildEndpoint` creates one `transport.NewWebSocketTargets()`, passes it in
the WebSocket leg's config with `TLS: cli.ClientTLSConfig()` (D9), and keeps
it on the `Server`. `DialLegs` gains `case "wss": return cfg.Addr != ""`. A
server with no WebSocket leg (no `--listen`) has no table, and `DialLegs`
refuses `ws` and `wss` there, so URL candidates are refused at `runner dial
add`.

### 2.2 Candidates (`cli/candidate.go`, `server/runner_dial_handler.go`)

- `cli.ParseCandidate` is unchanged. It is the grammar of `--server-cid`.
- New `cli.CanonicalDialRecordCandidate(text string) (string, error)`: a URL
  candidate is parsed with `transport.ParseWebSocketTarget` after an empty
  path is replaced by `cli.WebSocketPath`, and the canonical text is
  returned. Any other text goes through `ParseCandidate` and is returned as
  is.
- `runnerDialAdd` calls it for each candidate, stores the returned text, and
  runs `DialLegs`, `OwnerOfCandidate` and the byte limits on the stored text.
  `cli.CandidateTransport` already returns `ws`/`wss` for a URL (it cuts at
  the first `:`).

### 2.3 Dial manager (`server/runner_dialer.go`)

- `Resolve` (D10): URL text → `Targets.Register` →
  `objproto.NewRandomConnectionID(target.Transport(), addr)`.
- `RecordsPointingAt` (D11): the address clause also requires
  `l.rec.Key.Equal(e.Key)`.
- Adopt, the runtime-duplicate rule and `owner` are unchanged. They see a
  distinct address per named candidate, and the same address for the same
  candidate across attempts and restarts (D5).
- The candidate that answered is carried to the registry entry: `DialOrigin`
  and the adopt path's `SetLink` gain the candidate text, stored on
  `RunnerEntry` as `LinkTarget` and cleared wherever `LinkRecord` is cleared
  (`dialManager.Stop` sets the link to `dialed_out`).

### 2.4 One-shot dial and `--via` (`server/dial_runner_handler.go`)

`Handle` and `HandleWithVia` return `DialRunnerStatus_InvalidTarget` when
`objproto.NamedFromAddrPort` is ok for the target's address and its transport
is `ws`/`wss` (D3), with a log line saying named targets are dialed only
through dial records.

## 3. Wire (`runner/protocol/message.bgn`)

- `RunnerInfo`: append

  ```
      # The dial-record candidate that answered, as stored (canonical) — set
      # only when link == dial_record. A named cid (cid=ws(s):@<hex>-<id>) is a
      # key into the server's table; this is the URL it stands for.
      link_target_len :u16
      link_target :[link_target_len]u8
  ```

  Bounded by `dialMaxCandidateBytes` (512) because it is a stored candidate.
- `ConnID`: extend the comment to say that a 16-byte address in `100::/64`
  is not an IP. With transport `vsock` it is (cid u32, port u32); with `ws` or
  `wss` it is a named-target key that means something only to the server that
  registered it. This also closes the vsock design's open item that the
  schema did not say so.
- No other format changes. `RunnerInfo` is not on the path the harness
  upgrades itself through, so there is no transition shim
  (implementation-pitfalls Pitfall 10). `scripts/wire-skew-check.sh` runs as
  usual. The plan checks every message that embeds `RunnerInfo` for the
  control-message datagram limit.

## 4. Surfaces

Input stays where it is: `runner dial add` on the CLI, the TUI command line
and the WebUI command input (2026-11-04 spec D19). Only its help text changes.

| Surface | What |
| --- | --- |
| `cli/verb/table.go` `runner dial add` | Help names the URL form, the default path, and that URLs are for dial records only |
| `ls` runner rows (`cli/list.go`) | `target=<text>` when `link_target` is set. `ls --json` carries `link_target` on every runner, empty when unset |
| TUI runner detail (`tui/detail.go` `formatRunnerDetail`) | A `target:` line when set |
| TUI runner table | Unchanged. `dial:<8 hex>` in the Link column already names the record |
| WebUI runner rows | `target` in the row meta when set |
| wasm snapshot (`cmd/harness-webui-wasm/main.go`) | `linkTarget` in the runner conversion |
| `runner dial ls` | Unchanged; it already prints the stored candidate text, now canonical |
| README | Reverse-dial section: URL candidates, why (name-routed fronts), and that they are dial-record only |

The implementation walks `surface-parity-checklist` items 1–39 for this
field and this verb option.

## 5. Verification

objtrsf unit:
- `ParseWebSocketTarget`: accepted and rejected inputs from D4, canonical
  text, and the same key for inputs that canonicalize to the same text.
- `String` ↔ `ParseConnectionID` round trip for `ws:@…` and `wss:@…`; `@`
  with a non-`ws`/`wss` transport is rejected; vsock text is unchanged.
- Dial: an `httptest` TLS server on `127.0.0.1`, two targets
  `wss://localhost:<port>/a` and `wss://localhost:<port>/b`. The server sees
  two upgrade requests, one per path, each with `Host: localhost:<port>` and
  SNI `localhost` (read from `r.TLS.ServerName`), and two sockets stay up at
  once. Closing one socket leaves the other connection up. Negative control,
  written first against the current dial path: two IP `ConnectionID`s at
  `127.0.0.1:<port>` produce **one** upgrade request and one socket, with
  `Host: 127.0.0.1:<port>` and no SNI (P1, P2).
- Dial coalescing (D14): a TLS server whose accept is delayed past several
  retransmit intervals (for example 1.5 s). Exactly one upgrade request
  arrives and the handshake completes. Red first against the current code,
  which opens one socket per retransmit.
- A named address with no table entry, or a scheme mismatch, reaches
  `CannotSend`.

Harness unit:
- `CanonicalDialRecordCandidate` table, including the default path.
- `runner dial add`: a URL is stored canonical; two spellings of one URL give
  `duplicate_candidate`.
- `RecordsPointingAt` with two records whose candidates resolve to one
  address and different keys: kicking one runner removes only its own record.
  Red first against the current code.
- One-shot `server dial-runner` and `--via` with a named target →
  `invalid_target`, and nothing is dialed.
- `link_target` from each origin: set by a record's dial, set by adoption,
  cleared on `runner dial rm`; rendered in `ls`, `ls --json`, the TUI detail
  and the WebUI row.

Integration (in-process, `integration/` or `server` package):
- Server with a WebSocket leg; two listen-mode runners on `ws` with the
  **same** runner key; a TLS reverse proxy on `127.0.0.1` that routes
  `/a/ws` to runner A and `/b/ws` to runner B and records each request's
  `Host` and SNI. Two records, `wss://localhost:<p>/a/ws` and
  `wss://localhost:<p>/b/ws`.
  - Both runners register; their `cid`s differ; each `link_target` is its
    own URL.
  - The proxy saw `Host: localhost:<p>` and SNI `localhost` on both.
  - A task runs on each.
  - `runner kick` A: A's record is gone, B's record and connection stay.
  - Stopping A's upstream does not end B's connection.

Dummy-harness E2E, as the operator would type it (`dummy-harness` skill): the
same two-runner layout behind the same proxy, driven by `harness-cli runner
dial add wss://…`, `runner dial ls`, `ls`, `runner kick`, then the same rows
read in the TUI (dummy PTY) and the WebUI.
