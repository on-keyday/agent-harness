# PSK Authentication for harness-cli

**Date**: 2026-04-30  
**Status**: Approved

## Overview

Add optional Pre-Shared Key (PSK) authentication at the application-protocol layer. When a PSK is configured on the server, every WebSocket client (harness-cli, runner, TUI, WebUI) must present the correct PSK as its first protocol message before any other communication is permitted.

## Requirements

- PSK is optional: if `HARNESS_PSK` is not set on the server, behaviour is unchanged (backward compatible)
- All connection types are subject to PSK when it is configured (no exemptions)
- PSK is set by the operator at startup via `--psk` flag or `HARNESS_PSK` env var
- WebUI (WASM) reads PSK from the URL fragment (`#psk=<value>`)
- PSK comparison uses `crypto/subtle.ConstantTimeCompare` to avoid timing side-channels

## Wire Protocol

### Section 1: Enum definitions

Add to `trsf/wire/stream.bgn`:

```
# existing enum, adding psk_auth entry
enum ApplicationPayloadKind:
    :u8
    ping
    pong
    stream_data
    stream_cancel
    stream_ack
    stream_window_update
    pubsub
    task_control
    relay_control
    runner_control
    close
    agent_message
    psk_auth          # PSK authentication handshake
```

Add new formats (same file or `trsf/wire/psk.bgn`):

```
enum PskAuthStatus:
    :u8
    ok
    bad_psk

format PskAuthRequest:
    psk :[..]u8

format PskAuthResponse:
    status :PskAuthStatus
```

Update `trsf/wire/stream.go` to add `ApplicationPayloadKind_PskAuth = 12` consistent with the generated enum pattern. Add `PskAuthStatus`, `PskAuthRequest`, `PskAuthResponse` Go types in the same or a new file.

### Section 2: Message exchange

```
Client                          Server
  |                               |
  |-- [kind=psk_auth][psk_bytes] -->|   PskAuthRequest
  |<-- [kind=psk_auth][status]   --|   PskAuthResponse
  |                               |
  | (status=ok → normal dispatch) |
  | (status=bad_psk → close)      |
```

- Client sends `psk_auth` as the **first** application-layer message after the ECDH handshake completes.
- Server responds immediately.
- On `bad_psk` the server closes the connection; the client also closes and returns an error.
- The PSK is transported over the ECDH-encrypted channel so it is not exposed in plaintext on the wire.
- If the server has no PSK configured, it never sends a `PskAuthResponse` and the client must not send a `PskAuthRequest` (both sides skip the exchange).

## Server Changes

### `server.Config`

```go
PSK []byte // nil = no PSK enforcement
```

### `cmd/harness-server/main.go`

```
--psk  string  PSK passphrase (env: HARNESS_PSK; empty = disabled)
```

Resolved value is decoded as raw UTF-8 bytes and stored in `Config.PSK`.

### `server/handleConnection`

Add a per-connection authentication gate as a closure bool inside the `AutoReceive` callback:

```go
pskAuthed := len(s.cfg.PSK) == 0  // true immediately when PSK not configured

// inside AutoReceive callback:
if !pskAuthed {
    if kind != wire.ApplicationPayloadKind_PskAuth {
        // unexpected message before auth — drop and close
        conn.Close()
        return
    }
    req := &wire.PskAuthRequest{}
    req.Decode(msg.Data[1:])
    status := wire.PskAuthStatus_bad_psk
    if subtle.ConstantTimeCompare(req.Psk, s.cfg.PSK) == 1 {
        status = wire.PskAuthStatus_ok
    }
    sendPskAuthResponse(conn, status)
    if status != wire.PskAuthStatus_ok {
        conn.Close()
        return
    }
    pskAuthed = true
    return  // do not forward PSK message to dispatcher
}
// normal dispatch below
s.dispatcher.Dispatch(wrapped, msg.Data)
```

The gate is safe without additional locking because `AutoReceive` invokes the callback sequentially on a single goroutine.

## Client Changes

### PSK resolution (`cli/psk.go`)

Two build-tag variants of `GetPSK() []byte`:

**`//go:build !js`** (native):
```go
func GetPSK() []byte {
    v := os.Getenv("HARNESS_PSK")
    if v == "" {
        return nil
    }
    return []byte(v)
}
```

**`//go:build js`** (WASM):
```go
func GetPSK() []byte {
    hash := js.Global().Get("location").Get("hash").String()
    // parse "#psk=<value>"
    ...
    return []byte(value)
}
```

### PSK send helper (`cli/psk.go`)

```go
func SendPSK(ctx context.Context, conn objproto.Connection, psk []byte) error
```

Sends `PskAuthRequest`, waits for `PskAuthResponse`, returns error on `bad_psk` or timeout.

### Call sites

Each dial path calls `SendPSK` immediately after the underlying connection is established (before `SayHello` or any other RPC):

| File | Location |
|---|---|
| `cli/client.go` | after `peer.Dial` in `Dial()` |
| `cli/agent/conn.go` | after `peer.Dial` in `ConnectAgent()` |
| `runner/connect.go` | after `peer.Dial` in `Run()` |

`SendPSK` is a no-op when `psk` is `nil`, so no conditionals needed at call sites.

## WebUI

URL fragment format: `http://localhost:8539/#psk=<passphrase>`

The WASM `GetPSK()` implementation reads `window.location.hash`, strips the leading `#`, and parses the `psk=` key. The value is percent-decoded before use. If the fragment is absent or does not contain `psk=`, `GetPSK()` returns `nil` and no PSK message is sent (matching server behaviour when PSK is unconfigured).

## Non-Goals

- Key rotation / PSK versioning
- Per-task or per-runner PSK scoping (existing `HARNESS_AUTH_TICKET` covers that)
- HTTP-level authentication (handled at protocol layer)
