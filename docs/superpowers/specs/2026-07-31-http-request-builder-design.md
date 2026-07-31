# HTTP request builder for raw forwards

Spec B of two. Depends on `2026-07-31-tui-raw-connect-multipane-design.md`
(spec A) for the TUI pane the form lives in.

## Problem

A raw forward carries bytes to a port on the runner's side. The overwhelmingly
common thing to send is an HTTP request, and today every surface makes the
operator type it by hand:

- The TUI sends one line at a time with `\r\n` appended (`SendLine`). Headers
  work; a body does not, because the trailing CRLF is unconditional and
  `Content-Length` then never matches what arrives.
- The WebUI's send box takes text or hex, so the whole request — request line,
  `Host`, `Content-Length`, the blank line — is the operator's problem, with
  CRLF handling done by hand.
- `forward -W` splices stdin, so a request must be produced by another tool and
  piped in.

Nothing in the repo builds an HTTP request today (`grep` for `net/http` and
`HTTP/1.1` across `cli/` and `tui/` finds nothing).

## Decisions

1. **One builder, shared by all three surfaces**, in `cli/httpreq.go`, so the
   bytes on the wire cannot differ between them.
2. **Bytes are built directly, not via `net/http`.** The auto-header rules below
   are ours, and `http.Request` would either fight them or hide them. It also
   keeps the wasm build free of the server-side half of `net/http`.
3. **HTTP/1.1 only.** No HTTP/2, no `Transfer-Encoding: chunked`.
4. **Three headers are supplied automatically, and a user-supplied header of the
   same name always wins:**
   - `Host:` = the forward's `host:port`, port included even when it is 80 —
     writing it unconditionally removes a "which form did it send?" question.
   - `Content-Length:` when the body is non-empty.
   - `Connection: close` — a raw pane is one connection, and leaving it open
     invites a reader that never sees EOF.
5. **CR and LF anywhere in method, path, header name or header value is an
   error** and nothing is sent. This is the header-injection / request-smuggling
   boundary; it is validation, not sanitisation, so the caller sees a message
   rather than silently different bytes.
6. **The request goes out in a single `Send`.** Not `SendLine` — see the Problem
   section; nothing may append to the assembled bytes.
7. **The CLI takes ordinary flags, not a mini-syntax.** A `--http "GET /x"`
   string reads well until the first header or body and then needs a parser and
   an escaping rule of its own.

## Architecture

### `cli/httpreq.go`

```go
// HTTPRequestSpec is what a surface collects from the operator.
type HTTPRequestSpec struct {
    Method  string   // empty = GET
    Path    string   // "/healthz"; must start with "/" (or be "*")
    Headers []string // "Name: value", in order; a name given here suppresses
                     // the matching automatic header
    Body    []byte
}

// BuildHTTPRequest renders spec as HTTP/1.1 request bytes addressed to
// host:port. Returns an error for a malformed method/path/header or for CR/LF
// in any field.
func BuildHTTPRequest(spec HTTPRequestSpec, host string, port int) ([]byte, error)
```

Output shape:

```
POST /api HTTP/1.1\r\n
Host: 127.0.0.1:8080\r\n
Content-Length: 7\r\n
Connection: close\r\n
<user headers, in order>\r\n
\r\n
{"a":1}
```

No trailing CRLF after the body. An empty body ends the request at the blank
line.

Validation: method is a non-empty HTTP token; path starts with `/` or is `*`;
each header parses as `name: value` with a token name; no CR/LF in any field.

### CLI — `harness-cli forward -W`

New flags: `--http-method` (default GET), `--http-path`, `--http-header`
(repeatable), `--http-body` (literal, or `@file`, or `-` for stdin).

`--http-path` switches the mode: the built request is written to the forward,
then the response is streamed to stdout until the far end closes. stdin is not
spliced — a request that is fully specified by flags has nothing to read from
it. Without `--http-path` the command behaves exactly as it does today.

### TUI — a mode inside the pane (spec A)

`ctrl+t` toggles the active pane between raw byte entry and the HTTP form; the
key is deliberately not a printable one, since the pane's text input consumes
those. `ctrl+a`/`ctrl+e`/`ctrl+r` in the submit popup are the precedent.

The form has four fields — method (cycled with `←`/`→`), path, headers, body.
Headers and body are `textarea`s (as `tui/popup.go` already uses), one header
per line, so no separator syntax has to be invented. `tab` moves between
fields, `Enter` builds and sends, `ctrl+t` returns to byte entry. A build error
becomes the pane's note and nothing is sent.

### WebUI — a form in the raw panel

The same four fields beside the existing send box, toggled so raw byte sending
is never taken away. Submitting calls a new `harness.rawSendHTTP(paneKey, spec)`
which builds in wasm via the same `BuildHTTPRequest` and sends through the
existing `SendRawPane`, so the browser never assembles bytes itself.

## Failure modes

- **Build error** (bad path, CR in a header): reported in place, nothing sent.
- **Body given but the far end is not HTTP**: not our problem; the bytes are
  exactly what was asked for.
- **Response larger than the pane ring**: the existing ring bound applies
  unchanged; the pane is a debugging window.
- **Pane not connected**: send is refused as today.

## Verification

- Table tests for `BuildHTTPRequest`: automatic `Host`/`Content-Length`/
  `Connection`; user-supplied header suppresses each of them; header order
  preserved; body verbatim with no trailing CRLF; CR/LF in method, path, header
  name and header value each rejected; malformed path rejected.
- One integration test: raw forward to a real `httptest` server, send a built
  `GET` and a built `POST` with a body, assert the parsed response and that the
  server saw the expected method, path, headers and body.
- One live pass in the WebUI against a dummy harness for the form wiring.
- The TUI form is covered by unit tests on the bytes it produces; no live pass —
  the byte sequence is the whole claim.

## Out of scope

TLS (an `https://` target needs a TLS client, not a request builder), cookie or
auth helpers, response pretty-printing, saved/replayable requests, HTTP/2.
