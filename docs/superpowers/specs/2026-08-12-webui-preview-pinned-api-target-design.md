# WebUI HTML preview with a pinned API target

**Date:** 2026-08-12
**Status:** Approved (brainstorming)

## Problem

The WebUI's HTML preview renders **self-contained** files: bytes are pulled with
`filePullBytes` and dropped into a sandboxed iframe
(`webui/static/main.js:1211-1212`). A previewed page that wants to call an API —
a dev server or a service the task itself is running — has nothing to talk to.
The iframe deliberately omits `allow-same-origin`, so its origin is opaque and it
can reach nothing at all.

Meanwhile the WebUI *can already* reach a task's port: the raw connect pane opens
a forward whose client-side endpoint is in-process
(`cli/forward_endpoint.go:50`, bindings at `cmd/harness-webui-wasm/main.go:115-117`).
The two capabilities exist side by side and are unconnected.

Verbatim: *"webuiのポートforwardingとかをhtml previewの内側から使えるように
できたりしないんかなぁ..."*

## What already exists

- **Preview path**: `renderFilePreview` → `showHtmlPreview`
  (`webui/static/main.js:1330`, `:1201`) caches the pulled bytes and rebuilds the
  modal body for the render/source toggle without re-pulling. The iframe is
  `sandbox="allow-scripts"` + `srcdoc`.
- **Forward path**: `OpenRawForward` (`cli/forward_endpoint.go:50`) opens the data
  stream and registers the forward; `cli/raw_forward_wasm.go` holds per-pane
  slots with a monotonic generation so a close that races an in-flight open wins
  (stop-wins).
- **Request builder**: `BuildHTTPRequest` (`cli/httpreq.go:34`) renders HTTP/1.1
  bytes and supplies `Host`, `Content-Length` and `Connection: close`
  automatically, rejecting CR/LF anywhere in method / path / header (the
  request-smuggling boundary).
- **Capabilities**: `OpenPortForward` is unconditionally `ForwardLocal` and
  `RegisterPortForward` is direction-dependent (`server/capabilities.go:10-16`).
  A tunneled fetch is `direction=local` × `client_endpoint=in_process`, exactly
  what the raw pane already does — **no new capability, no schema change**.
- **Response parsing**: none exists anywhere. The raw pane renders bytes as
  text/hex. Measured 2026-08-12 in a scratch module: `net/http.ReadResponse`
  builds under `GOOS=js GOARCH=wasm` and decodes a chunked body correctly, so the
  parser is a standard-library call rather than new code.

## Decisions

1. **Scope is the page's own `fetch` / `XMLHttpRequest`.** Declarative
   subresources (`<img src>`, `<script src>`, `<link href>`, CSS `url()`) are not
   interceptable by a shim and are out of scope. Rendering a dev server's *page*
   (rather than a local file that calls one) is a different, larger design.
2. **One operator-pinned target per preview open.** The preview modal gains a
   `host:port` field; the task is already fixed by `fileTaskSelect`. **Left
   empty, nothing changes at all** — no shim is injected and `showHtmlPreview`
   behaves byte-for-byte as it does today. The pin is dropped when the modal
   closes.
3. **The whole request/response cycle lives in Go**, behind one new binding
   `harness.httpFetch`. The alternative — JS composing `rawOpen` + `rawSendHTTP`
   + accumulating `harness_rawData` — would put an HTTP response parser and a
   byte-accumulation state machine in `main.js`. Go reuses `BuildHTTPRequest`, so
   the bytes on the wire are identical across CLI / TUI / raw pane / preview,
   which is that function's stated reason for existing.
4. **The parent re-validates the target on every message.** The shim's own check
   is a convenience for the page, never a control: the page can `delete
   window.fetch` and call `parent.postMessage` directly.
5. **The parent identifies the sender with `event.source === iframe.contentWindow`,
   not `event.origin`.** Every sandboxed frame reports origin `"null"`, so origin
   comparison is not a check.
6. **A page-visible marker is provided and is explicitly not a security
   mechanism.** It lives in the page's own realm, so the page can forge anything
   it asserts; it exists only so an artifact can branch on "am I being
   previewed".
7. **`X-Harness-Preview: 1` is added in Go** on the `httpFetch` path only, and the
   page may not set or override it — same treatment as `Host` and `Connection`.
8. **No registry exemption.** Every tunneled fetch registers and deregisters a
   forward, upholding the raw-forward design's decision 4 ("a forward that is
   running but not listed must not exist").
9. **The shim is injected by position rule and never before the doctype.**

## Architecture

```
iframe (srcdoc, sandbox="allow-scripts", opaque origin)
  page fetch()/XHR
      └─ shim ── postMessage ──▶ parent (main.js)
                                   ├─ event.source identity check
                                   ├─ re-validate URL against the pin
                                   └─▶ window.harness.httpFetch(task, host, port, spec)
                                          └─ wasm/Go: OpenRawForward
                                               → BuildHTTPRequest (+X-Harness-Preview)
                                               → write → read to EOF
                                               → http.ReadResponse
                                               → Close (deregisters)
      Response ◀── postMessage ◀── {status, statusText, headers, body:Uint8Array}
```

### UI delta

`webui/index.html`: one input in the `file-preview-modal` header row, shown only
in render mode, defaulting to empty. `webui/static/main.js`: `filePreviewHtml`
gains an `api` field; `showHtmlPreview` injects the shim only when `api` is set.

### New binding

```
harness.httpFetch(taskIDHex, host, port, {method, path, headers, body})
  -> Promise<{status, statusText, headers: [[name,value],...], body: Uint8Array}>
```

Go-side rules, all of which are load-bearing:

- `Connection` is forced to `close` and cannot be supplied by the page. The read
  is read-to-EOF; a keep-alive response would hang until the far end times out.
- `Host` cannot be supplied by the page — `BuildHTTPRequest` derives it from the
  forward's own target.
- `X-Harness-Preview: 1` is appended unconditionally and cannot be overridden.
- `http.ReadResponse` is given a synthetic `*http.Request` carrying the method,
  not `nil`: with `nil` a `HEAD` response is mis-framed and the read blocks.
- The body read is bounded by a **new** cap of 8 MiB, enforced while reading in
  Go. `PREVIEW_MAX_BYTES` (1 MiB, `main.js:424`) is a file-preview limit and is
  not reused — an API response is a different thing with a different sane bound.
- The forward is closed on every path, including parse failure.

### Shim

Injected as the first script in the document. Insertion position, in order:
after the first `<head …>` tag's `>`; else after `<html …>`; else after
`<!doctype …>`; else at the very start. **Never before a doctype** — that drops
the page into quirks mode and makes the preview render differently from the real
thing, which is the worst failure mode this feature could have.

The shim replaces `window.fetch` and `XMLHttpRequest`. A request is tunneled when
its resolved URL is relative or its origin equals the pinned origin; anything
else rejects with a `TypeError`, matching how a real cross-origin failure
surfaces so pages fail in a familiar shape. Each tunneled call gets a request id;
replies are matched by id and ignored if the generation has moved on.

### Marker

Before any page script runs, the shim defines:

```js
Object.defineProperty(window, "__harness", {
  value: Object.freeze({
    v: 1,
    preview: "html-file",
    rel: "artifacts/rpg.html",
    api: { host: "127.0.0.1", port: 5173 },  // null when no pin
  }),
  writable: false, configurable: false, enumerable: true,
});
```

Three states are distinguishable from inside: no `__harness` (a plain browser),
`api: null` (previewed, no reach), `api` set (previewed with a target). The task
ID is deliberately absent — the page has no use for it.

## Security boundary

Today the iframe reaches nothing because its origin is opaque. This design opens
exactly one operator-named hole and nothing else:

- `allow-same-origin` stays off. The page still cannot touch the WebUI origin,
  its DOM, its storage, or the trsf connection. The only channel is a
  fixed-schema `postMessage`.
- The pin must be typed by the operator per preview open. Default is no reach,
  so opening agent-generated HTML is as inert as it is today unless the operator
  deliberately does otherwise.
- Parent-side re-validation is the boundary. The shim and the marker are inside
  the page's own realm, so any secret they hold the page can read and any message
  they can send the page can forge. **Neither is ever used as a control.**
- No cookies, no credentials, no auth headers are attached. The response is
  handed back as opaque bytes.
- The existing capability gate is inherited unchanged: without `ForwardLocal` the
  fetch fails exactly as the raw pane would.

### Trade-offs

| Axis | Effect |
|---|---|
| Function | A previewed artifact can call an API inside its task, from a phone, where no local listener exists |
| Security | Reach goes from "nothing" to "one operator-named endpoint"; default-off keeps the current posture for anyone who does not opt in |
| Non-functional | One forward registration per fetch; a runaway page churns the registry |

## Registry and resource bounds

Registration is per connection and therefore per fetch. That is consistent, not
an accident: an unlisted running forward is what decision 4 forbids. The forward
list rides the ~5s snapshot poll (`POLL_INTERVAL_MS`, `main.js:7`, `:863`;
`forward ls` renders from that same snapshot, `:1887`), so a fetch lasting tens
of milliseconds will almost never be visible. If it does become noisy, the fix is
a display filter — never an unregistered forward.

Guards against a runaway page (e.g. `setInterval` issuing fetches):

- At most 4 tunneled requests in flight per preview; further requests queue.
- Closing the modal aborts everything in flight, and late replies are discarded
  by request id plus generation — the same stop-wins discipline as
  `previewSlots` / `rawSlots`.

## Scope / non-goals (YAGNI)

- Declarative subresources, and previewing a dev server's own page.
- WebSocket / EventSource: they do not fit the one-connection-one-request
  `Connection: close` model.
- TLS to the target, cookie jar, automatic redirect following.
- `direction=remote`; unrelated to this design.

## Known limitations

- Only scripted requests are tunneled. A page whose assets are external files
  still renders exactly as it does today (i.e. without them).
- Each fetch pays a fresh forward open. This is fine for a handful of API calls
  and bad for a chatty page; the concurrency cap makes that visible as slowness
  rather than as resource exhaustion.
- The insertion-position rule can be defeated by pathological input (a literal
  `<head` inside a comment preceding the real head). The fallback design, if that
  ever shows up in practice, is a shim-only bootstrap document that receives the
  page over `postMessage` and installs it with `document.open()` /
  `document.write()`. That removes string splicing entirely but rests on globals
  surviving `document.open()`, which is **not verified**; do not adopt it without
  measuring that first.

## Testing

- **Go**: `httpFetch` against a local listener — `Content-Length` and chunked
  bodies, a header-only 204, a `HEAD` request, a body exceeding the 8 MiB cap,
  and a page-supplied `Host` / `Connection` / `X-Harness-Preview` all being
  overridden rather than honoured.
- **JS**: shim URL classification (relative / pin-matching absolute /
  non-matching); the parent rejecting a message whose `event.source` is not the
  preview iframe; the parent rejecting an off-pin target sent by a page that
  deleted the shim; injection position across inputs with `<head>`, with
  `<!doctype>` only, and with neither — asserting the doctype is never displaced.
- **Playwright** (dark theme `#1e1e1e`/`#d4d4d4`, desktop **and** 390px): a real
  task serving a small HTTP endpoint, previewing an HTML file that fetches it,
  asserting the value reaches the DOM inside the iframe, that the iframe still
  has `sandbox="allow-scripts"` with no `allow-same-origin`, and that with the
  pin left empty the same page shows its no-API fallback.

## Implementation note

`httpFetch` is a new wasm binding, so this is **not** a JS-only change: the
WebUI's HTML/CSS/JS hot-reload, but the wasm must be rebuilt. Verify with
`make check` and `make wasm-check` rather than an ad-hoc `go build`.
