# Can a client reach a runner directly? — probe results

- Date: 2026-09-06
- Status: probe complete, nothing implemented
- Scope: measurement only. The design this fed into is
  `2026-09-06-file-transfer-without-server-decrypt-design.md`, which
  deliberately does NOT build any of what is measured here.

Hosts are written `<linux-host>` and `<windows-host>`; both sit on one
LAN with no NAT between them. Ports are the ones the run actually used.

## Why this was measured

The server splices two connections for every file transfer, which costs a
measured 1.8x (see the design doc's Problem section). Removing the middle box
entirely — the client talking to the runner directly — removes 3.0x instead.
Before designing that, the question was whether it is reachable at all: this
project's runners dial outward and were assumed to have no inbound path.

## What was run

Three probes, all throwaway, none kept.

**1. In-process, loopback.** Three endpoints in one process, shaped like
production: a `EndpointModeMutual` UDP endpoint on an ephemeral port (a
dial-mode runner, `runner/connect.go:638`), a second Mutual endpoint standing
in for the server, and a `EndpointModeClient` endpoint (a CLI client,
`cli/dial_endpoint_native.go:34`). The runner dialed the server; the server
read the runner's connection id off its accept channel; the client then dialed
that address with a fresh 16-bit id.

| | result |
| --- | --- |
| runner → server dial | ok |
| server observes runner's reflexive address | ok — `udp:127.0.0.1:37037-1` |
| client → runner direct ECDH at that address | **ok** — `udp:127.0.0.1:37037-16962` |
| conn surfaced on the runner's accept channel | ok — `udp:127.0.0.1:37149-16962` |

The last row's address is the client's, not the runner's: each side names its
peer by the address the peer's packets arrive from, and only the 16-bit id is
shared. A discovery mechanism therefore has to carry an address, not a
connection id — the id is the dialer's to choose.

**2. Live runners, no punch.** From `<linux-host>`, the same client shape
dialed all three live `<windows-host>` runners at the `udp:` addresses
`harness-cli ls` prints, with fresh ids and an 8 s budget.

```
udp:<windows-host>:58553  → ecdh: message receive timeout (8.0s)
udp:<windows-host>:61352  → ecdh: message receive timeout (8.0s)
udp:<windows-host>:52180  → ecdh: message receive timeout (8.0s)
```

`ls` was re-read afterwards and reported the same three ports, still
registered — so these sockets were alive and exchanging packets with the
server at the moment the dials were refused. The addresses were not stale.
ICMP echo to the same host is also dropped.

**3. Live runner host, with a punch.** A throwaway Go program was built and
run on `<windows-host>` inside a task worktree (Go 1.25.7 windows/amd64). It
bound a Mutual UDP endpoint on a known port and called
`objproto.Endpoint.SendProbe` toward `<linux-host>:45999` every 500 ms.
`<linux-host>` bound its client endpoint on exactly 45999 — the address being
punched toward — and dialed twice from that one socket:

| target | punched toward us first? | result |
| --- | --- | --- |
| the probe's own socket, `:45998` | yes | **ECDH complete, 33 ms** |
| a live runner's socket, `:58553` | no | timeout, 10 s |

Windows side: `ACCEPTED: inbound conn from udp:<linux-host>:45999-27393`,
`punches sent=240 failed=0`. The id `27393` is the one the punched dial used,
so the accepted connection is that dial and not something else.

Same client socket, same LAN, same peer host, same budget, one run. The only
difference between the two rows is whether that Windows socket had sent a
packet toward `<linux-host>:45999` first.

## Findings

**F1. A dial-mode runner already accepts inbound handshakes.** Its endpoint is
`EndpointModeMutual` on both underlays (`runner/connect.go:631,638`), and the
comment above it says why: so a dial-mode runner can serve as a Phase C relay
proxy. On UDP the socket is bound regardless of mode, so the mode is the only
thing gating acceptance. No `--listen` flag and no relaxation of the
`--server-cid` / `--listen` exclusivity (`cmd/agent-runner/main.go:155`) is
needed for the transport to accept a client.

**F2. The address a client would dial is already published.** For an inbound
connection objproto builds the id from the source address of the datagram
(`objproto/objproto.go`, `NewConnectionID(transport, from, …)` in `receive`),
so `RunnerEntry.ID` — rendered as `id=` by `harness-cli ls` — is the runner's
own reflexive address. The server is already the rendezvous point and no STUN
equivalent has to be added.

**F3. Unsolicited inbound is dropped, and NAT has nothing to do with it.**
Probe 2 failed on a NAT-free LAN. A punch is therefore not a NAT workaround
that could be skipped in a simple deployment; it is on the only path there is.

**F4. A punch is sufficient, and needs no new transport API.**
`objproto.Endpoint.SendProbe` is on the exported interface
(`objproto/session.go:43`) and probe 3 used nothing else. A production punch
would be a runner request carrying an address, whose handler calls `SendProbe`
— the same shape as `EstablishRelay` (`runner/relay_handler.go`).

**F5. The punch has to name the exact socket that will dial.** Probe 3 bound
the client on 45999 and punched toward 45999. That is not incidental: the
mapping a firewall opens is per remote address AND port. So the client must
create its data-plane endpoint first and tell the server which port it got —
and `transport.UDPEndpoint` does not report the port it bound
(`trsf/throughput_test.go`'s `freeUDPPorts` comment states this). Either
objtrsf grows an accessor or the client picks explicit ports.

**F6. Nothing in a dial-mode runner would service the accepted connection.**
`GetNewActiveConnectionChannel` has exactly two non-test readers,
`runner/listen.go:107` and `server/server.go:827`. Probe 1's fourth row shows
objproto surfaces the connection; in dial mode no accept loop is reading. The
loop and its first-payload dispatch already exist in `runner/listen.go` and
would need a third `AppKind` arm.

**F7. This path exists only on the UDP underlay.** A dial-mode runner's WS
endpoint is built with a nil mux and registers no HTTP listener
(`runner/connect.go:619-621`), so there is no socket for a client to connect
to. Every Linux runner in the current fleet is `ws:`; the three UDP runners are
the Windows ones. A WebUI client can never take this path at all, because
browsers have no raw UDP.

**F8. A connection is bound to an address, with no recovery.** The connection
id contains the address, and `migrat|path validation|rebind|address change`
matches nothing in `objproto/` or `trsf/`. Two consequences that probe 3 did
not exercise, because it ran on one LAN: a firewall or NAT that maps per
destination gives the peer a different port than the server observed, and the
dial lands on an id nobody holds; and a mapping that changes mid-session drops
the connection with no path-validation to recover it. Today every connection is
outbound toward one fixed server, which is why neither has been seen.

## What a direct data plane would cost

Ordered as they would have to be built, from F1–F8:

1. A runner request that carries an address and punches toward it (F4).
2. An accept loop in dial mode plus a third first-payload arm (F6).
3. Runner-side authorization of the client — see the design doc; this one is
   shared with the server-forwards-packets route and is the only item that is.
4. Discovery: the runner's address is published (F2), but the client's own
   data-plane port is not, and the server needs it (F5).
5. Ordering: punch, then dial, then a retry window if the first dial races the
   punch.
6. A relay path retained for `ws:` runners and for the WebUI (F7).
7. An objtrsf accessor for the bound port, which is a separate module and so a
   publish plus a `go.mod` bump (F5).

The design doc takes the route that needs only item 3, and records why.

## Amendment — items 4 and 7 are probably not on the path (2026-09-06)

Written while designing the other route, and it reverses part of the list
above. Items 4 and 7 exist because F5 says the punch must name the exact port
the client will dial from, and `transport.UDPEndpoint` does not report the port
it bound. Both were reasoned from probe 3, where each side bound an explicit
port.

**The client does not need a new socket.** It already holds one open to the
server, and the server already observes its address — that is where the
control connection's packets come from. A second connection from that same
socket differs only in the 16-bit id. So the address a punch must name is one
the server has without being told, and the client learns nothing it did not
already have. The explicit ports in probe 3 were for the convenience of running
the experiment from two directions at once, not a property of the mechanism.

That leaves five items, and item 5's ordering window is the only one of them
this design work has not already reduced.

This is a reversal of the list, not a correction of the measurements: probes
1–3 stand, and F5's constraint — the punch and the dial must name the same
address and port — is exactly what reusing one socket satisfies.

## What was not measured

- Any deployment with a NAT between client and runner (F8's two failure modes).
- Whether a punched path survives idle periods, or how often it must be
  re-punched.
- Throughput over a direct path. The ladder's `udp` rung prices it at 3.0x the
  splicing relay, but that is one process on loopback, not two hosts.
