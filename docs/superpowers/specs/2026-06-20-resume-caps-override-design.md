# Resume capability override

Date: 2026-06-20
Status: design — approved (chat), implementing

## Problem

A task's capabilities are fixed at creation (`caps_child = caps_parent ∩
requested`) and resume reinstates them verbatim (capabilities feature, "Resume").
But a common operation is to **re-grant a different capability set and reuse the
same task id** on resume. Today resume ignores `RequestedCaps` entirely, so this
is impossible without creating a new task.

## Decision

On resume, allow an **explicit** capability override: the resumed task's caps
become `caps_resumed = callerCaps(resumer) ∩ RequestedCaps`, bounded by the
**resumer's** authority (operator = full → arbitrary re-grant including widening
beyond the original creator; an agent resumer cannot exceed its own caps). The
new caps are **persisted** (survive restart and subsequent resumes).

Override is **opt-in and explicit** — plain resume keeps the persisted caps
unchanged. This prevents the dangerous default (a confined task silently widening
to full just by being reattached, since the session default caps are full).

Authority model mirrors spawn: at create it was `creator ∩ requested`; at resume
it is `resumer ∩ requested` — the only change is the authority is the (live)
resumer instead of the (possibly-dead) original parent.

## Scope

- Schema: a presence bit on the resume-capable requests.
- Server: resume path applies + persists the override when the bit is set.
- CLI: explicit `--caps` on a resume command sets the override.
- TUI + WebUI: an opt-in "apply caps on resume" toggle (default OFF) that, when
  ON, sends the override with the session caps; plain resume keeps persisted.

### Non-goals

- Changing spawn (create) semantics (unchanged).
- Auto-applying session caps to resume by default (explicitly rejected —
  accident/widening risk).
- Bounding the override by the original creator's caps (rejected: creator caps
  aren't persisted, creator may be dead, and operator-as-root should be able to
  widen).

## Wire

Add `resume_caps_override :u1` to `SubmitRequest` and `OpenInteractiveRequest`
(beside the existing `detachable`/`x11_enabled` u1 bitfields). The override
*value* reuses the existing `requested_caps` field. Semantics:

- **Create path:** `resume_caps_override` is ignored (create always uses
  `requested_caps` as `caps_creator ∩ requested_caps`).
- **Resume path:** if `resume_caps_override == 1`, set the task's caps to
  `callerCaps(resumer) ∩ requested_caps` and persist; if `== 0`, keep the
  persisted caps (current verbatim behavior).

No `Capability` enum change; no new request/response type.

## Server

- `TaskStore.Resume(...)` gains `capsOverride bool, newCaps protocol.Capability`.
  When `capsOverride`, it sets `e.Capabilities = newCaps` as part of the atomic
  resume, and writes a **new WAL event `task_caps_changed{TaskID, Capabilities}`**
  (a dedicated event — NOT an `omitempty` field on `task_resumed` — so a
  deliberate override to `Capability_None` (0) is unambiguous on replay).
  `ReplayEvents` gains a `task_caps_changed` case that sets `t.Capabilities`.
- The resume branches in `handleSubmit`/`handleOpenInteractive` compute
  `newCaps = intersectCaps(callerCaps(cid), req.RequestedCaps)` and pass
  `(capsOverride = req.ResumeCapsOverride==1, newCaps)` to `Resume`. When the bit
  is 0 they pass `(false, _)` and caps are untouched.
- `callerCaps` and `intersectCaps` are the existing capabilities helpers.

## CLI

A resume command (`submit --resume`, `session new --resume`, `interactive
--resume`) that was given an **explicit `--caps`** sets `resume_caps_override=1`
and `requested_caps = ParseCaps(--caps)`. Detect explicit setting with
`flag.Visit` (not the value — since `--caps ""`/absent both parse to All).
Without `--caps`, `resume_caps_override=0` (keep persisted). The `*AndCaps`
builders gain a way to carry the override bit (e.g. a small options struct or an
extra bool param), threaded only on the resume-capable entry points.

## TUI / WebUI

Per-session **`applyCapsOnResume` bool, default OFF**:

- **TUI:** `caps --on-resume on|off` sets it; `caps` (show) also prints its
  state. When ON, resume actions send `resume_caps_override=1` + `sessionCaps`;
  when OFF (default), resume sends override=0 (keep persisted). Plain
  reattach/resume is always safe.
- **WebUI:** a checkbox `□ apply caps on resume` near the cap chips, default
  unchecked, bound to a JS `applyCapsOnResume`. When checked, the resume spawn
  calls send `resumeCapsOverride: true` + `caps: spawnCaps`; unchecked → no
  override. (The wasm `harnessSubmit`/`harnessStartInteractive` read
  `opts.resumeCapsOverride` and set the request bit.)

## Error handling

- Resume with override by an under-privileged agent: `intersectCaps` silently
  drops bits the resumer lacks (no error; bounded re-grant). Operator → no drop.
- Override to `Capability_None` is a valid deliberate full-confine on resume.

## Testing

- Server: override resume sets+persists `callerCaps ∩ requested` (operator →
  requested; an agent resumer → bounded); WAL `task_caps_changed` round-trips and
  replay restores the new caps; **plain resume (override=0) leaves caps
  unchanged** (regression guard against silent widening); override to None works.
- CLI: `--caps` present on resume → override bit set + value; absent → bit clear
  (via `flag.Visit`).
- TUI: `caps --on-resume on` flips the bool; resume sends override only when ON;
  default OFF keeps persisted.
- WebUI: Playwright — checkbox default off (resume keeps caps); checked + chips
  set → resume re-grants; verify dark/390px. (Live verification pending server
  rebuild, per the embed constraint.)
