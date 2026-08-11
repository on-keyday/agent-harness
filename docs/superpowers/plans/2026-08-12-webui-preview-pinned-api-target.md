# WebUI preview pinned API target — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an HTML file previewed in the WebUI call one operator-pinned `host:port` inside its task, via the existing in-process port forward.

**Architecture:** A shim injected ahead of the page's own scripts replaces `fetch`/`XMLHttpRequest` and relays requests over `postMessage` to the parent. The parent re-validates against the pin and calls a new `harness.httpFetch` wasm binding; the whole HTTP request/response cycle runs in Go on top of `OpenRawForward` + `BuildHTTPRequest` + `http.ReadResponse`. No schema change, no new capability.

**Tech Stack:** Go (`cli`, `cmd/harness-webui-wasm`, `GOOS=js GOARCH=wasm`), vanilla JS (`webui/static/main.js`, classic script), `webui/index.html`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-12-webui-preview-pinned-api-target-design.md`. Its decisions are binding.
- **Pin empty ⇒ zero behavioural change.** No shim, no listener, no marker.
- `sandbox="allow-scripts"` stays; **never** add `allow-same-origin`.
- The parent's re-validation is the security boundary. The shim and `window.__harness` are inside the page's realm and are never treated as trusted.
- Sender identity is `event.source === iframe.contentWindow`. `event.origin` is `"null"` for every sandboxed frame and must not be used as a check.
- The page may not set `Host`, `Connection`, `Content-Length` or `X-Harness-Preview`.
- Response body cap: **8 MiB**, enforced in Go while reading. Do not reuse `PREVIEW_MAX_BYTES`.
- Concurrency cap: **4** tunneled requests in flight per preview.
- Dark theme `#1e1e1e` / `#d4d4d4`; every UI change must be checked at desktop **and** 390px.
- Verify with `make check`, `make wasm-check`, `make test`, `make vet` — not ad-hoc `go build`.
- New pure JS helpers go at **column 0** in `main.js` (top level, outside the async IIFE), like `isHtmlExt` / `basename`. Classic script ⇒ they become globals ⇒ Playwright can call them directly. This repo has **no JS test runner** and none is to be added.

## File Structure

| File | Responsibility |
|---|---|
| `cli/httpfetch.go` (create, no build tag) | Portable one-shot HTTP over a raw forward: header sanitisation, send, bounded read, parse. |
| `cli/httpfetch_test.go` (create) | Unit tests for the pure halves (sanitise, parse, cap) and validate-before-dial. |
| `cmd/harness-webui-wasm/main.go` (modify, near `:115`) | `harness.httpFetch` binding. |
| `webui/index.html` (modify, `:250-260`) | Pin input in the preview modal header. |
| `webui/static/main.js` (modify) | Pin state, shim source + injection (top-level helpers), parent-side bridge. |
| `webui/static/style.css` (modify) | Pin input styling, 390px included. |

`RunHTTPRequestForward` (`cli/forward_stdio.go:50`) is deliberately **not** refactored to share code. It is `//go:build !js` and its job is "copy raw response bytes to an `io.Writer`"; the new function's job is "parse a response into a value". They share six lines (build, open, send) and differ in everything that matters. Sharing would mean moving a helper across the build-tag boundary to save less than it costs.

---

### Task 1: `cli/httpfetch.go` — portable request/response core

**Files:**
- Create: `cli/httpfetch.go`
- Create: `cli/httpfetch_test.go`

**Interfaces:**
- Consumes: `BuildHTTPRequest` (`cli/httpreq.go:34`), `OpenRawForward` (`cli/forward_endpoint.go:50`), `RawConn.Send` / `.Recv` / `.Close`.
- Produces:
  - `type HTTPFetchResult struct { Status int; StatusText string; Headers [][2]string; Body []byte; Truncated bool }`
  - `func HTTPFetch(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec) (*HTTPFetchResult, error)`
  - `func sanitizeFetchHeaders(in []string) []string`
  - `func parseHTTPFetchResponse(method string, raw []byte) (*HTTPFetchResult, error)`
  - `const httpFetchMaxBody = 8 << 20`

- [ ] **Step 1: Write the failing tests**

```go
package cli

import (
	"strings"
	"testing"
)

// The page controls spec.Headers. BuildHTTPRequest lets a user-supplied header
// win over the automatic one, so anything that would displace Host /
// Connection / Content-Length, or forge the preview marker, has to be dropped
// before it gets there.
func TestSanitizeFetchHeadersDropsControlledNames(t *testing.T) {
	got := sanitizeFetchHeaders([]string{
		"Host: evil.example",
		"connection: keep-alive",
		"Content-Length: 999",
		"x-harness-preview: 0",
		"X-App: keep",
	})
	if len(got) != 1 || got[0] != "X-App: keep" {
		t.Fatalf("want only the app header, got %q", got)
	}
}

// Content-Length framing, the common case.
func TestParseHTTPFetchResponseContentLength(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.StatusText != "OK" {
		t.Fatalf("status = %d %q", res.Status, res.StatusText)
	}
	if string(res.Body) != "{}" {
		t.Fatalf("body = %q", res.Body)
	}
	var ct string
	for _, h := range res.Headers {
		if strings.EqualFold(h[0], "Content-Type") {
			ct = h[1]
		}
	}
	if ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
}

// Chunked must be de-chunked, not handed to the page verbatim.
func TestParseHTTPFetchResponseChunked(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "hello" {
		t.Fatalf("body = %q", res.Body)
	}
}

// A HEAD response has no body however its headers are framed. Parsing it with
// a nil request makes ReadResponse believe a body is coming and the read hangs
// or mis-frames, so the method must be carried into the parse.
func TestParseHTTPFetchResponseHEADHasNoBody(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n"
	res, err := parseHTTPFetchResponse("HEAD", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 0 {
		t.Fatalf("HEAD body = %q, want empty", res.Body)
	}
}

func TestParseHTTPFetchResponseNoContent(t *testing.T) {
	res, err := parseHTTPFetchResponse("GET", []byte("HTTP/1.1 204 No Content\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 204 || len(res.Body) != 0 {
		t.Fatalf("got %d / %q", res.Status, res.Body)
	}
}

// Oversize bodies are truncated rather than refused: a page that asks for
// something huge should see the prefix and a flag, not lose the response.
func TestParseHTTPFetchResponseTruncatesOversizeBody(t *testing.T) {
	body := strings.Repeat("a", httpFetchMaxBody+100)
	raw := "HTTP/1.1 200 OK\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n" + body
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != httpFetchMaxBody || !res.Truncated {
		t.Fatalf("len=%d truncated=%v", len(res.Body), res.Truncated)
	}
}

// A bad spec must be rejected before anything is dialled — same property
// TestRunHTTPRequestForwardValidatesBeforeDial asserts for the CLI path. A nil
// *Client is safe only while validation runs first.
func TestHTTPFetchValidatesBeforeDial(t *testing.T) {
	if _, err := HTTPFetch(t.Context(), nil, "task", "h", 1, HTTPRequestSpec{Path: "relative"}); err == nil {
		t.Fatal("want a validation error, got nil")
	}
}
```

Add the tiny helper the oversize test uses at the bottom of the test file:

```go
func itoa(n int) string { return strconv.Itoa(n) }
```

(and import `strconv`).

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cli/ -run 'HTTPFetch|SanitizeFetch' -v`
Expected: compile failure — `undefined: sanitizeFetchHeaders`, `undefined: parseHTTPFetchResponse`, `undefined: HTTPFetch`.

- [ ] **Step 3: Implement `cli/httpfetch.go`**

```go
package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
)

// httpFetchMaxBody bounds the body handed back to a caller that must hold the
// whole thing in memory (the WebUI preview does). It is deliberately NOT
// PREVIEW_MAX_BYTES: that is a file-preview limit for bytes shown as text, and
// an API response is a different thing with a different sane bound.
const httpFetchMaxBody = 8 << 20

// HTTPFetchResult is one parsed HTTP response. Headers keep wire order and
// allow repeats, so Set-Cookie and friends survive; a map would not.
type HTTPFetchResult struct {
	Status     int
	StatusText string
	Headers    [][2]string
	Body       []byte
	Truncated  bool
}

// fetchControlledHeaders are the names the caller of HTTPFetch may not supply.
// Host and Content-Length are derived from the forward and the body;
// Connection must stay "close" because the read below runs to EOF and a
// keep-alive response would park it until the far end times out;
// X-Harness-Preview is a marker this side stamps, so letting it be overridden
// would make it worthless.
var fetchControlledHeaders = []string{"host", "connection", "content-length", "x-harness-preview"}

// sanitizeFetchHeaders drops the controlled names. It filters rather than
// erroring: these headers are supplied by a previewed page through fetch()
// init, not typed by an operator, so there is nobody to show a message to.
func sanitizeFetchHeaders(in []string) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		name, _, ok := strings.Cut(h, ":")
		if ok {
			lower := strings.ToLower(strings.TrimSpace(name))
			controlled := false
			for _, c := range fetchControlledHeaders {
				if lower == c {
					controlled = true
					break
				}
			}
			if controlled {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

// parseHTTPFetchResponse turns raw response bytes into a value.
//
// The synthetic request is load-bearing: with a nil *http.Request,
// ReadResponse cannot know the method and frames a HEAD response as though the
// Content-Length it advertises were a real body.
func parseHTTPFetchResponse(method string, raw []byte) (*HTTPFetchResult, error) {
	req, err := http.NewRequest(method, "http://forward/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read one byte past the cap so a body that exactly fills it is not
	// mislabelled as truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpFetchMaxBody+1))
	if err != nil {
		return nil, err
	}
	truncated := false
	if len(body) > httpFetchMaxBody {
		body = body[:httpFetchMaxBody]
		truncated = true
	}

	headers := make([][2]string, 0, len(resp.Header))
	for name, values := range resp.Header {
		for _, v := range values {
			headers = append(headers, [2]string{name, v})
		}
	}

	statusText := strings.TrimSpace(strings.TrimPrefix(resp.Status, itoaStatus(resp.StatusCode)))
	return &HTTPFetchResult{
		Status:     resp.StatusCode,
		StatusText: statusText,
		Headers:    headers,
		Body:       body,
		Truncated:  truncated,
	}, nil
}

func itoaStatus(code int) string {
	return strings.TrimSpace(http.StatusText(code)[:0] + fmtInt(code))
}

// HTTPFetch sends one request over a raw forward and returns the parsed
// response. It is the portable counterpart to RunHTTPRequestForward, which
// copies raw bytes to an io.Writer for the CLI; the two share a build step and
// nothing else, so they stay separate.
//
// The spec is validated and built before anything is dialled, so a bad request
// costs an error rather than a forward that is established, registered and
// torn down.
func HTTPFetch(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec) (*HTTPFetchResult, error) {
	spec.Headers = append(sanitizeFetchHeaders(spec.Headers), "X-Harness-Preview: 1")
	req, err := BuildHTTPRequest(spec, host, port)
	if err != nil {
		return nil, err
	}
	method := spec.Method
	if method == "" {
		method = "GET"
	}
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if err := rc.Send(req); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		data, eof, rerr := rc.Recv(ctx)
		if len(data) > 0 {
			// Bound the accumulation too, not just the parsed body: a far end
			// streaming forever must not grow this without limit. The slack
			// above the cap covers status line and headers.
			if buf.Len() < httpFetchMaxBody+64<<10 {
				buf.Write(data)
			} else {
				break
			}
		}
		if eof {
			break
		}
		if rerr != nil {
			if buf.Len() == 0 {
				return nil, rerr
			}
			break
		}
	}
	return parseHTTPFetchResponse(method, buf.Bytes())
}
```

Replace the `itoaStatus` / `fmtInt` placeholder above with the direct form —
`strconv.Itoa(resp.StatusCode)` — and import `strconv` instead:

```go
statusText := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
```

Delete `itoaStatus` and `fmtInt`; they exist in this draft only to be removed.

- [ ] **Step 4: Run the tests**

Run: `go test ./cli/ -run 'HTTPFetch|SanitizeFetch' -v`
Expected: PASS.

- [ ] **Step 5: Measure the wasm size cost of `net/http`**

`net/http` is currently absent from every non-test file in `cli/`, so this task
pulls it into the wasm build for the first time. `webui/static/main.wasm` is
8,675,525 bytes today and is fetched over a LAN by a phone.

```bash
ls -l webui/static/main.wasm            # before (8675525)
make webui-build
ls -l webui/static/main.wasm            # after
```

Decision rule: a delta above **+2 MiB** is not acceptable for this feature.
If it is exceeded, replace `http.ReadResponse` with `net/textproto` for the
status line and headers plus a small chunked decoder in `parseHTTPFetchResponse`
— the function is already isolated for exactly this reason, and its tests do not
change. Record the measured delta in the commit message either way.

- [ ] **Step 6: Full verification and commit**

```bash
make check && make wasm-check && make test && make vet
git add cli/httpfetch.go cli/httpfetch_test.go
git commit -m "feat(cli): parse one HTTP response over a raw forward (portable)"
```

---

### Task 2: `harness.httpFetch` wasm binding

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (binding table near `:115`, handler beside `harnessRawSendHTTP` near `:901`)

**Interfaces:**
- Consumes: `cli.HTTPFetch`, `cli.HTTPFetchResult`, `cli.HTTPRequestSpec`; the file's existing `currentClient()`, `rejectErr()`, `rootCtx`, and the promise-executor shape used by `harnessRawSendHTTP`.
- Produces:
  `harness.httpFetch(taskIDHex, host, port, {method, path, headers, body}) -> Promise<{status, statusText, headers: [[name,value],…], body: Uint8Array, truncated: boolean}>`
  where `headers` in the argument is an array of `"Name: value"` strings and `body` is a `Uint8Array` or absent.

- [ ] **Step 1: Add the binding to the table**

In the `js.ValueOf(map[string]any{…})` block containing `"rawSendHTTP"` (`:117`), add:

```go
"httpFetch": js.FuncOf(harnessHTTPFetch),
```

- [ ] **Step 2: Implement the handler**

Place it directly after `harnessRawSendHTTP` so the two HTTP-shaped bindings
stay together. Reuse whatever the file already uses to turn a JS spec object
into a `cli.HTTPRequestSpec` (`harnessRawSendHTTP` parses the same shape — read
it and call the same helper rather than writing a second parser; if it parses
inline, extract that into a helper in this step and have both call it).

```go
// harnessHTTPFetch sends one HTTP request over a fresh in-process forward and
// resolves with the parsed response. Unlike rawOpen/rawSend it holds nothing:
// the forward is opened, used and closed inside one call, so a page issuing N
// fetches leaves nothing behind.
//
//	harness.httpFetch(taskIDHex, host, port, {method, path, headers, body})
//	  -> Promise<{status, statusText, headers, body, truncated}>
func harnessHTTPFetch(this js.Value, args []js.Value) any {
	executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
		resolve, reject := promiseArgs[0], promiseArgs[1]
		go func() {
			if len(args) < 4 {
				rejectErr(reject, errors.New("httpFetch: want (taskIDHex, host, port, spec)"))
				return
			}
			c, err := currentClient()
			if err != nil {
				rejectErr(reject, err)
				return
			}
			spec, err := httpSpecFromJS(args[3])
			if err != nil {
				rejectErr(reject, err)
				return
			}
			res, err := cli.HTTPFetch(rootCtx, c, args[0].String(), args[1].String(), args[2].Int(), spec)
			if err != nil {
				rejectErr(reject, err)
				return
			}
			body := js.Global().Get("Uint8Array").New(len(res.Body))
			js.CopyBytesToJS(body, res.Body)
			headers := js.Global().Get("Array").New(len(res.Headers))
			for i, h := range res.Headers {
				pair := js.Global().Get("Array").New(2)
				pair.SetIndex(0, h[0])
				pair.SetIndex(1, h[1])
				headers.SetIndex(i, pair)
			}
			resolve.Invoke(js.ValueOf(map[string]any{
				"status":     res.Status,
				"statusText": res.StatusText,
				"headers":    headers,
				"body":       body,
				"truncated":  res.Truncated,
			}))
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}
```

- [ ] **Step 3: Build and commit**

```bash
make wasm-check && make check
git add cmd/harness-webui-wasm/main.go
git commit -m "feat(webui): expose harness.httpFetch to the page"
```

---

### Task 3: pure JS helpers — pin parsing, shim source, injection position

**Files:**
- Modify: `webui/static/main.js` (top level, column 0, beside `isHtmlExt` at `:4818`)

**Interfaces:**
- Produces (all global because `main.js` is a classic script):
  - `parsePinnedTarget(text) -> {host, port, origin} | null`
  - `previewShimSource(pin, rel) -> string` (the JS text to inject, no `<script>` tags)
  - `injectPreviewShim(html, scriptText) -> string`
  - `previewFetchTargetAllowed(rawURL, pin) -> boolean`

- [ ] **Step 1: Implement the helpers**

```js
// parsePinnedTarget accepts "host:port" or "port" (host defaults to 127.0.0.1,
// which is what a task's own dev server almost always binds). Returns null for
// anything it cannot read, which is how the caller decides not to inject.
function parsePinnedTarget(text) {
  const s = String(text || "").trim();
  if (!s) return null;
  let host = "127.0.0.1", portStr = s;
  const idx = s.lastIndexOf(":");
  if (idx >= 0) { host = s.slice(0, idx).trim() || "127.0.0.1"; portStr = s.slice(idx + 1).trim(); }
  if (!/^\d+$/.test(portStr)) return null;
  const port = Number(portStr);
  if (port < 1 || port > 65535) return null;
  if (/[\s/\\?#@]/.test(host)) return null;
  return { host, port, origin: `http://${host}:${port}` };
}

// previewFetchTargetAllowed decides whether a URL the page asked for may be
// tunneled. Relative URLs resolve against the pinned origin, so they are always
// in scope; an absolute URL must match the pin exactly.
//
// This runs in BOTH realms on purpose: inside the iframe so the page gets a
// familiar failure, and again in the parent, where it is the actual control.
// The parent's copy is the one that matters — the page can delete the shim.
function previewFetchTargetAllowed(rawURL, pin) {
  if (!pin) return false;
  let u;
  try { u = new URL(String(rawURL), pin.origin + "/"); } catch { return false; }
  return u.origin === pin.origin;
}

// injectPreviewShim puts scriptText into html so it runs before any of the
// page's own scripts.
//
// Position rules, in order: after the first <head …> tag, else after <html …>,
// else after <!doctype …>, else at the very start. NEVER before a doctype —
// displacing it drops the page into quirks mode, so the preview would render
// differently from the real thing, which is the worst failure this feature
// could have.
function injectPreviewShim(html, scriptText) {
  const tag = `<script>${scriptText}<\/script>`;
  const at = (re) => { const m = re.exec(html); return m ? m.index + m[0].length : -1; };
  let pos = at(/<head\b[^>]*>/i);
  if (pos < 0) pos = at(/<html\b[^>]*>/i);
  if (pos < 0) pos = at(/<!doctype\b[^>]*>/i);
  if (pos < 0) pos = 0;
  return html.slice(0, pos) + tag + html.slice(pos);
}

// previewShimSource is the text of the shim. It runs inside the iframe's own
// realm, so nothing it holds is a secret and nothing it asserts is trusted by
// the parent — see the parent's re-validation.
function previewShimSource(pin, rel) {
  const cfg = JSON.stringify({ origin: pin.origin, host: pin.host, port: pin.port, rel: rel || "" });
  return `(() => {
  "use strict";
  const CFG = ${cfg};
  Object.defineProperty(window, "__harness", {
    value: Object.freeze({
      v: 1,
      preview: "html-file",
      rel: CFG.rel,
      api: Object.freeze({ host: CFG.host, port: CFG.port }),
    }),
    writable: false, configurable: false, enumerable: true,
  });
  let seq = 0;
  const pending = new Map();
  window.addEventListener("message", (ev) => {
    const m = ev.data;
    if (!m || m.__harnessFetch !== "reply") return;
    const p = pending.get(m.id);
    if (!p) return;
    pending.delete(m.id);
    if (m.error) p.reject(new TypeError(m.error));
    else p.resolve(m);
  });
  function send(url, init) {
    const id = ++seq;
    return new Promise((resolve, reject) => {
      let u;
      try { u = new URL(String(url), CFG.origin + "/"); } catch { reject(new TypeError("bad url")); return; }
      if (u.origin !== CFG.origin) { reject(new TypeError("Failed to fetch")); return; }
      pending.set(id, { resolve, reject });
      const headers = [];
      const h = (init && init.headers) || {};
      if (h && typeof h.forEach === "function") h.forEach((v, k) => headers.push(k + ": " + v));
      else for (const k of Object.keys(h)) headers.push(k + ": " + h[k]);
      parent.postMessage({
        __harnessFetch: "request", id,
        url: u.href,
        method: (init && init.method) || "GET",
        path: u.pathname + u.search,
        headers,
        body: (init && init.body != null) ? String(init.body) : null,
      }, "*");
    });
  }
  window.fetch = (input, init) => {
    const url = (input && typeof input === "object" && "url" in input) ? input.url : input;
    return send(url, init).then((m) => new Response(m.body, {
      status: m.status, statusText: m.statusText, headers: m.headers,
    }));
  };
  window.XMLHttpRequest = class {
    constructor() { this.readyState = 0; this._h = []; this.status = 0; this.responseText = ""; this.response = ""; }
    open(method, url) { this._m = method; this._u = url; this.readyState = 1; }
    setRequestHeader(k, v) { this._h.push(k + ": " + v); }
    send(body) {
      send(this._u, { method: this._m, headers: {}, body }).then((m) => {
        this.status = m.status;
        this.responseText = new TextDecoder().decode(m.body);
        this.response = this.responseText;
        this.readyState = 4;
        if (this.onreadystatechange) this.onreadystatechange();
        if (this.onload) this.onload();
      }).catch(() => {
        this.readyState = 4;
        if (this.onerror) this.onerror();
      });
    }
  };
})();`;
}
```

- [ ] **Step 2: Commit**

```bash
git add webui/static/main.js
git commit -m "feat(webui): preview shim source, pin parsing and injection position"
```

(These are verified live in Task 5 — this repo has no JS test runner and none is
being added. `injectPreviewShim` and `parsePinnedTarget` are globals precisely so
Playwright can call them directly.)

---

### Task 4: modal UI and the parent-side bridge

**Files:**
- Modify: `webui/index.html:250-260`
- Modify: `webui/static/main.js` (`:407-412` state, `:1201` `showHtmlPreview`, `:1153` teardown)
- Modify: `webui/static/style.css`

**Interfaces:**
- Consumes: `parsePinnedTarget`, `previewShimSource`, `injectPreviewShim`, `previewFetchTargetAllowed` (Task 3); `window.harness.httpFetch` (Task 2).

- [ ] **Step 1: Markup**

In `#file-preview-modal`'s `.preview-header`, before `#file-preview-close`:

```html
<input id="file-preview-api" class="preview-api" type="text" hidden
       placeholder="API host:port" title="Let the rendered page fetch this task port (empty = no network)">
```

- [ ] **Step 2: State and teardown**

Beside the existing preview state (`:407-412`):

```js
  const filePreviewApi = document.getElementById("file-preview-api");
  let previewPin = null;      // {host, port, origin} or null
  let previewGenApi = 0;      // bumped on every close/re-render; stale replies are dropped
  let previewInFlight = 0;
  const PREVIEW_FETCH_MAX_INFLIGHT = 4;
```

In the modal teardown (`:1153`, where `filePreviewHtml = null`):

```js
    previewPin = null;
    previewGenApi++;          // in-flight replies belong to a preview that is gone
    previewInFlight = 0;
    filePreviewApi.hidden = true;
    filePreviewApi.value = "";
```

- [ ] **Step 3: Inject only when pinned**

In `showHtmlPreview` (`:1201`), replace `iframe.srcdoc = text;` with:

```js
      // Empty pin => the page gets no shim, no marker and no reach: byte-for-byte
      // the behaviour that shipped before this feature.
      previewPin = parsePinnedTarget(filePreviewApi.value);
      previewGenApi++;
      previewInFlight = 0;
      iframe.srcdoc = previewPin
        ? injectPreviewShim(text, previewShimSource(previewPin, rel))
        : text;
```

and show the input for HTML previews only, next to the toggle:

```js
    filePreviewApi.hidden = mode !== "render";
```

Re-rendering when the operator types a target: on `change` of `#file-preview-api`,
if `filePreviewHtml && filePreviewHtml.mode === "render"`, call `showHtmlPreview()`
again. The iframe is rebuilt from the cached bytes; nothing is re-pulled.

- [ ] **Step 4: The parent bridge**

```js
  // The page's shim relays fetch() here. Everything in the iframe is inside the
  // page's own realm, so nothing it sends is trusted: the target is re-checked
  // against the pin the OPERATOR typed, and the sender is identified by window
  // identity because every sandboxed frame reports origin "null".
  window.addEventListener("message", async (ev) => {
    const m = ev.data;
    if (!m || m.__harnessFetch !== "request") return;
    const iframe = filePreviewBody.querySelector("iframe.preview-iframe");
    if (!iframe || ev.source !== iframe.contentWindow) return;

    const gen = previewGenApi;
    const pin = previewPin;
    const reply = (payload) => {
      if (gen !== previewGenApi) return;          // preview closed or re-rendered
      iframe.contentWindow.postMessage({ __harnessFetch: "reply", id: m.id, ...payload }, "*");
    };
    if (!pin || !previewFetchTargetAllowed(m.url, pin)) {
      reply({ error: "Failed to fetch" });
      return;
    }
    if (previewInFlight >= PREVIEW_FETCH_MAX_INFLIGHT) {
      reply({ error: "too many concurrent requests" });
      return;
    }
    previewInFlight++;
    try {
      const taskID = fileTaskSelect.value;
      const res = await window.harness.httpFetch(taskID, pin.host, pin.port, {
        method: m.method, path: m.path, headers: m.headers, body: m.body,
      });
      reply({ status: res.status, statusText: res.statusText, headers: res.headers, body: res.body });
    } catch (e) {
      reply({ error: e.message || "Failed to fetch" });
    } finally {
      previewInFlight--;
    }
  });
```

- [ ] **Step 5: Style**

In `style.css`, beside `.preview-toggle`:

```css
.preview-api {
  background: #2a2a2a;
  color: #d4d4d4;
  border: 1px solid #3a3a3a;
  border-radius: 3px;
  padding: 2px 6px;
  min-width: 8em;
  max-width: 12em;
}
@media (max-width: 600px) {
  .preview-api { min-width: 6em; max-width: 7em; }
}
```

- [ ] **Step 6: Commit**

```bash
make check
git add webui/index.html webui/static/main.js webui/static/style.css
git commit -m "feat(webui): pin an API target for the rendered HTML preview"
```

---

### Task 5: end-to-end on a dummy harness

**Files:** none — this task produces evidence, and a fix commit if it finds one.

- [ ] **Step 1: Bring up a dummy harness**

Follow the `dummy-harness` skill. `scripts/dummy-harness.sh up --detach --agent fake --name PREVIEWPIN`, then eval its env.

- [ ] **Step 2: Give the task something to serve**

Inside the task's worktree, start a trivial HTTP server on a port and write two
files: `probe.html`, which fetches `/api` and writes the result into the DOM and
also renders `window.__harness ? "previewed" : "plain"`, and the endpoint itself
returning a recognisable JSON body plus echoing back whether it saw
`X-Harness-Preview`.

- [ ] **Step 3: Drive the WebUI with Playwright MCP**

Open `<webui>/#psk=<psk>`, Files tab, select the task, preview `probe.html`:

1. **Pin empty** — the page must render its no-API fallback, and
   `window.__harness` must be undefined inside the iframe.
2. **Pin the served port** — the fetched value must appear in the DOM, and the
   endpoint must report having seen `X-Harness-Preview: 1`.
3. Assert the iframe still has `sandbox="allow-scripts"` and **no**
   `allow-same-origin`.
4. Evaluate `injectPreviewShim("<!doctype html><html><head></head><body>x</body></html>", "0")`
   and confirm the doctype is still first.
5. Repeat 1–2 at **390px**, and screenshot both widths.

- [ ] **Step 4: Off-pin rejection**

In the iframe, `delete window.fetch` then `parent.postMessage({__harnessFetch:"request",id:99,url:"http://127.0.0.1:9/",method:"GET",path:"/",headers:[]}, "*")`.
The parent must reply with an error and must not open a forward. Confirm with
`harness-cli forward ls` that nothing was registered for port 9.

- [ ] **Step 5: Tear down and record**

`scripts/dummy-harness.sh down`. Keep the screenshots (UI screenshots are
deliverables — name them and report their paths; do not delete them).

---

### Task 6: land

- [ ] **Step 1:** Follow the `landing-to-main` skill for `remote-agent-harness` (Mode A local-trunk: rebase the task branch onto current `main`, fast-forward `main`, FF-push to origin, never force).
- [ ] **Step 2:** `make build` in the main checkout after the push.
- [ ] **Step 3:** `harness-cli notify` the operator that the work landed, naming the commit range and the screenshot paths.

## Self-Review

**Spec coverage:** decisions 1–9 map to Task 3/4 (scope, pin, shim, marker), Task 1 (Go-side cycle, forced headers, cap, `X-Harness-Preview`), Task 4 (parent re-validation, `event.source`, concurrency, generation), Task 1 + design (no registry exemption — nothing is added, `OpenRawForward` already registers and `Close` deregisters).

**Deviation from the spec, deliberate:** the spec's testing section asks for JS unit tests. This repo has no JS test runner and adding one would mean installing packages, which is out of scope for this change. The JS logic is therefore made globally reachable (Task 3) and verified by evaluating it directly under Playwright in Task 5. The Go-side and injection-position properties are still covered.

**Open risk carried into execution:** the `net/http` wasm size delta (Task 1 Step 5) is unmeasured. The fallback is scoped to one function.
