# Registering the preview's pin instead of its fetches

**Date:** 2026-08-12
**Status:** Approved (brainstorming)

## Problem

`HTTPFetch` opens a raw forward per request, and `OpenRawForward` registers it
(`cli/forward_endpoint.go:50`). A preview fetch lasts tens of milliseconds, the
forward list rides the ~5s snapshot poll (`webui/static/main.js:7`, `:863`), and
`portForwardRegistry` is a live map with no history at all — `add` / `remove` /
`list` / `snapshot`, nothing retained (`server/port_forward_registry.go:40-144`).
So a registration exists for each fetch and **none of them is observable**.

Verbatim: *"port forwarding対象なのが一瞬しか表示されないからステルスっぽく
なっちゃってるのよな"*

That is worse here than for any other forward. The reach was granted to
attacker-influenced page content on the strength of the operator typing a
target; after that the operator has no view of what the page did with it, and no
way to take it back short of closing the modal.

The 2026-08-12 pinned-API-target spec anticipated the wrong direction: it said
registrations would be *noisy* and that the fix, if needed, was a display
filter. The opposite happened.

## Decisions

1. **The registered unit is the pin, not the connection.** A pinned preview
   registers once and keeps that registration for as long as the rendered
   preview exists.
2. **Per-fetch connections open without registering**, using `OpenPortForward`
   alone. The data path has never needed the registry — the raw-forward design
   records that `-L` worked with zero registrations and that
   `handleOpenPortForward` performs no registry lookup.
3. **This extends the raw-forward design's decision 5 rather than contradicting
   it.** That decision reads: *"One registration per connection. There is no
   listener to represent, so there is nothing longer-lived than a connection to
   register."* A pinned preview is exactly the thing it says does not exist — a
   client-side endpoint that outlives any single connection. Its decision 4
   ("a forward that is running but not listed must not exist") is served
   better, not weakened: what is running is a preview that can reach a target,
   and that is now what appears in the list, for its whole life, instead of
   thirty-millisecond rows nobody ever sees.
4. **`forward kill` on the pin entry revokes the preview's reach.** The
   registration is watched with `serveForwardControl`
   (`cli/port_forward.go:268`), so a kill from any surface drops the pin in the
   page-side bridge and every later fetch is refused. This is new: today a pin
   cannot be withdrawn without closing the modal.
5. **Lifetime is the rendered preview's realm.** Opened when a pin is applied,
   closed when the modal closes or the pin changes — the latter already rebuilds
   the iframe, because the shim has to exist before the page's scripts run.
6. **No schema change and no capability change.** `RegisterPortForward` with
   `direction=local` needs `ForwardLocal`, which is the same capability
   `OpenPortForward` already requires unconditionally
   (`server/capabilities.go:10-16`).

## Architecture

```
pin applied ──▶ RegisterPortForward(local, in_process, target=host:port)
                  │   forward_id, control stream held for the modal's life
                  │   └── serveForwardControl: a Closed event (forward kill)
                  │        drops the pin and notifies the page-side bridge
                  ▼
          `forward ls` shows one row for as long as the preview is open
                  │
   each fetch ────┴─▶ OpenPortForward(host, port)   [no registration]
                        → BuildHTTPRequest → write → read to EOF
                        → http.ReadResponse → close
```

`cli/preview_forward_wasm.go` holds the pin per key with the same shape as
`rawSlots` (`cli/raw_forward_wasm.go`): a keyed slot plus a monotonic
generation reserved before the RPC, so a preview closed while the registration
is still in flight discards it instead of installing an orphan (stop-wins).

Bindings:

```
harness.previewPinOpen(key, taskIDHex, host, port) -> Promise<number>   // forward_id
harness.previewPinFetch(key, {method, path, headers, body})
    -> Promise<{status, statusText, headers, body, truncated}>
harness.previewPinClose(key) -> Promise<void>
window.harness_previewPinClosed(key, reason)   // hook: killed from another surface
```

`HTTPFetch` keeps registering and stays as it is: a one-off fetch with nothing
longer-lived behind it is exactly the case decision 5 describes, so it remains
correct for any caller that is not holding a pin.

## Page-side behaviour

The bridge refuses a fetch when there is no live pin, which now includes the
revoked case. On `harness_previewPinClosed` the modal shows why, so a kill from
the CLI is visible to whoever is looking at the preview rather than appearing as
requests that silently start failing.

## Testing

- **Dummy harness:** with a preview open and pinned, `forward ls` shows one row
  for the whole time and it does not churn per fetch; `forward kill <id>` makes
  the next fetch from the page fail and surfaces the reason in the modal;
  closing the modal removes the row.
- **Playwright:** desktop and 390px, that a page fetching in a loop produces
  exactly one registry row.

## Known limitations

- The row does not say which preview it belongs to beyond the origin column
  (`origin_kind` + cid), so two previews pinned to different targets from the
  same browser are told apart by target only. Carrying a label would need a
  schema field and is not worth one yet.
- Individual fetches are no longer separately listed. They were not observable
  before either; what changes is that the authorisation now is.
