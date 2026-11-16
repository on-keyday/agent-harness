# Capability u64 widening — short design

Status: the widening itself was approved by the operator in conversation,
2026-11-16 (plain u64; a different capability model was raised and not
chosen). Step 2 of three in `2026-11-16-task-list-order-design.md` (ordering;
this; task labels). The WebUI representation below is my call.

## Intent

`enum Capability` is `:u32` with 30 bits used. Task labels need a `label` bit,
the 31st, and `TestCapabilityStaysWithinTheU32Budget` refuses it. Widen first,
as its own change, so no gate is ever merged to save bits.

## Scope

- `runner/protocol/message.bgn`: `enum Capability` becomes `:u64`; regenerate.
  No bit value changes, and `all` stays `0x3fffffff`.
- Go: every `uint32(<capability>)` conversion and every `uint32` field that
  holds caps becomes `uint64` — `server/wal.go` (the WAL's JSON numbers decode
  into the wider field unchanged, so there is no migration), `server/scope.go`,
  `server/taskstore.go`, `cli/verb/caps.go` (`CapInfo.Bit`),
  `cli/capability_error.go` (`OnesCount64`), `cmd/harness-webui-wasm/main.go`,
  and the tests that build WAL events.
- The budget test becomes the JS-exactness bound below.

## Who is affected

The fields typed `:Capability` are in client↔server formats only (TaskInfo,
SubmitRequest, OpenInteractiveRequest, WhoAmIResponse, PermissionDeniedResponse,
SetCapsRequest, ScopeOverride, ScheduleSpec, ScheduleDetail). No runner decodes
one, so the runners need no restart for this change; harness-cli, the TUI and
the server ship together, and the WebUI is embedded in the server.
`scripts/wire-skew-check.sh` runs on landing as usual.

A skew between them misreads every field after the first Capability (4 bytes
read as 8, or the reverse). `make build` on landing replaces the
`bin/harness-cli` that agents on the build host call, so the server upgrade
has to follow the landing at once: land only when the server can be
restarted right away.

## WebUI (my call)

JavaScript's bitwise operators work on signed 32-bit integers, so `mask & bit`
is wrong from bit 31. The cap masks stay plain Numbers (the wasm bridge already
passes them as float64), and every bit operation in `main.js` goes through
three helpers in `task_models.js` — `capHas(mask, bit)`, `capOr(a, b)`,
`capXor(a, b)` — that compute in BigInt and return Numbers. A Number holds an
integer exactly up to 2^53, so a Go test fails when `Capability_All` would use
more than 53 bits; past that the masks must cross the bridge as strings or
BigInts, which is a change for that day.

Rejected: masks as BigInt throughout the page (every literal, comparison and
the wasm boundary change, for bits nobody uses yet); bit tests in wasm (a
bridge call per chip per render).

## Non-goals

No new capability bit (labels adds the first). No change to how caps are
named, granted or displayed.
