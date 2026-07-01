# AmbiguousRunner candidate picker — design

Date: 2026-07-02
Status: approved (design), pending implementation plan

## Problem

When two or more runners tie on longest-prefix `AllowedRoots` score for a
task's repo under the default `Any` selector, dispatch returns
`AmbiguousRunner` and fails. This happens routinely now that a repo can be
served by more than one runner slot (e.g. a second `--hostname`-differentiated
runner spawned on the same roots). Concretely it breaks the TUI's one-keypress
`r`/`R` resume: the resume falls back to `Any`, hits `AmbiguousRunner`, and the
user must drop to the command line to pin `--host`/`--runner` by hand.

The server already computes the candidate set (`Registry.Candidates` →
`[]RunnerEntry`) but throws it away on the ambiguous branch. The client never
sees which runners were candidates.

What the server sends today on ambiguity:

- **OpenInteractive path** (`server/task_handler.go:642-643`): bare
  `AmbiguousRunner` status, **zero** candidate info on the wire.
- **Submit path** (`task_handler.go:510-518`): stuffs `"ambiguous: host1,
  host2"` (hostnames only, no IDs) into the `ErrorMsg` string — a
  convention-in-payload smell, and un-pinnable since hostnames can collide.

## Goal

On `AmbiguousRunner`, carry the candidate runners back to the client so the
user can see them and pick one. Selection re-issues the same request pinned to
the chosen runner's ConnectionID.

## Scope

- **Path**: `OpenInteractive` only — covers both fresh interactive sessions
  (`session new` / TUI `i`) and interactive resume (`session new --resume` /
  TUI `r`/`R`). **Submit (oneshot) is out of scope**; its schema is untouched.
- **Surfaces**: all three UIs.
  - **TUI**: in-app runner-picker popup.
  - **WebUI**: modal picker.
  - **CLI (non-TUI)**: print candidates to stderr and exit non-interactively;
    user re-runs with `--runner <cid>`. No stdin prompt.
- **Trigger**: picker appears only when there are **≥2 candidates**. Exactly
  one candidate still auto-selects (unchanged behavior). Zero candidates is
  `NoRunnerForRepo` / `PinnedNotFound` as today.

## Non-goals

- No change to the Submit/oneshot dispatch path or its `ErrorMsg` string.
- No client-side re-implementation of the server's longest-prefix-match
  selection logic. The server remains the single source of truth for who the
  candidates are.
- No "always show the picker even for one candidate" behavior now (design
  leaves room for it as a future toggle, but it is not built).

## Design

### 1. Schema (`runner/protocol/message.bgn`)

New fixed-shape record for one candidate. The `cid` is
`objproto.ConnectionID.String()` — the exact string form the `--runner`
selector already parses (`cli/selector.go` `buildRunnerIDSelector`). Carrying
the **string cid** (not the typed `RunnerID`) sidesteps the zero-value
`RunnerID` encoder-panic invariant (`IpAddrLen` assertion) and gives the client
a value it can drop straight into `SelectorOpts{Runner: cid}`.

```
format RunnerCandidate:
    cid_len          :u16
    cid              :[cid_len]u8       # ConnectionID.String(); pass verbatim to --runner
    hostname_len     :u16
    hostname         :[hostname_len]u8
    matched_root_len :u16
    matched_root     :[matched_root_len]u8   # the AllowedRoots entry that tied for best score
    active_tasks     :u16
    max_tasks        :u16
```

`OpenInteractiveResponse` gains a conditional candidates block — the bytes
exist on the wire **only** when the status is `ambiguous_runner`, mirroring the
existing `if origin == NotifyOrigin.worker:` conditional-field pattern in
`NotifyRequest`:

```
format OpenInteractiveResponse:
    status :OpenInteractiveStatus
    task_id :TaskID
    stream_id :u64
    if status == OpenInteractiveStatus.ambiguous_runner:
        candidates_len :u16
        candidates     :[candidates_len]RunnerCandidate
```

Regenerate Go via `make protoregen ARGS='runner/protocol/message.bgn'` — do
not hand-edit the generated `message.go`.

### 2. Server (`server/task_handler.go`)

In `handleOpenInteractive`, the `len(cands) > 1` branch (currently
`return errResp(AmbiguousRunner)`) builds the candidates list from the
in-hand `cands []RunnerEntry`:

- `cid` = `entry.ID` (already `ConnectionID.String()`).
- `hostname` = `entry.Hostname`.
- `matched_root` = the **first** entry in `entry.AllowedRoots` whose
  `protocol.MatchLen(root, repo)` equals that runner's best score (recompute
  locally; `bestRootScore` already exists as a reference). "First" is a stable,
  deterministic tie-break when a runner lists multiple equally-matching roots;
  it is display metadata only and does not affect selection.
- `active_tasks` = `len(entry.ActiveTasks)`, `max_tasks` = `entry.MaxTasks`.

Same for the resume branch of `handleOpenInteractive` (the `resuming` case runs
the identical `Candidates` gate). The bare `errResp` helper stays for the other
statuses.

### 3. Client plumbing (`cli`, native + wasm)

Introduce a typed error carrying the candidates, returned in place of the flat
`fmt.Errorf` on the ambiguous status — the same "judge by type, not by string
match" discipline as the existing `ErrPinnedNotFound` sentinel:

```go
type RunnerCandidate struct { Cid, Hostname, MatchedRoot string; ActiveTasks, MaxTasks int }
type AmbiguousRunnerError struct { Candidates []RunnerCandidate }
func (e *AmbiguousRunnerError) Error() string { /* "ambiguous_runner: N candidates ..." */ }
```

`openInteractive` (native) and `InteractiveWithSelectorArgsAndCaps` (wasm) map
the response's candidates into `[]cli.RunnerCandidate` and return
`&AmbiguousRunnerError{...}` when status is `ambiguous_runner`. Callers use
`errors.As`. Both build-tag variants get the same treatment (sibling-parity).

### 4. UIs

**TUI** (`tui/`): a new small picker model (following the existing
`popup` / `boardModal` / `filepicker` conventions). When
`InteractiveReadyMsg.Err` is an `*AmbiguousRunnerError`, open the picker
listing candidates as e.g. `gmkhost  [2/8]  /home/.../harness   ws:...-30218`.
On selection, re-issue the same resume/open command pinned to
`SelectorOpts{Runner: chosen.Cid}`.

**WebUI** (`webui/` + wasm glue): the wasm layer surfaces the candidate list to
JS (structured, not a flat error string); JS renders a modal picker consistent
with the dark/mobile-aware WebUI; selection re-dials pinned to the chosen cid.

**CLI (non-TUI)** (`cmd/harness-cli/session.go`): catch the typed
`*AmbiguousRunnerError`, print a candidate table to stderr (cid / host /
matched_root / active-max), and exit non-zero with a hint to re-run with
`--runner <cid>`.

### 5. Resume composition (builds on 98a8286)

The `r`/`R` path already does: **last-runner pin (`AssignedTo`) → on
`PinnedNotFound`, retry `Any`**. This design appends the final rung: **on `Any`
→ `AmbiguousRunner`, open the picker**. Full ladder:

1. Pin to the runner the task last ran on (`AssignedTo`).
2. If that runner is gone (`PinnedNotFound`), retry with `Any`.
3. If `Any` is ambiguous (≥2 candidates), show the picker; the user's choice
   pins the re-issue.

### 6. Testing

- **server**: unit test that the `handleOpenInteractive` ambiguous branch fills
  `candidates` with the expected cids/hostnames/roots for a ≥2-candidate
  registry (both fresh and resume).
- **cli**: round-trip test that an `ambiguous_runner` response decodes into
  `*AmbiguousRunnerError` with the candidate list intact (native; wasm via the
  shared decode path).
- **tui**: pure-function test that picker selection maps a chosen candidate to
  `SelectorOpts{Runner: cid}` (same granularity as the existing
  `resumeSelectorOpts` test).
- **wasm-check / vet / full test suite** green; `make build` after landing.

## Risks / trade-offs

- **Schema change** touches a generated wire type; must regenerate rather than
  hand-edit, and the conditional-field addition is backward-incompatible for a
  client/server version skew on the ambiguous branch only (single-user dogfood
  → acceptable; rebuild both). Non-ambiguous responses are byte-identical to
  today (conditional block absent), so the common path is unaffected.
- **WebUI is the heaviest slice** (wasm↔JS marshalling + new modal). Accepted:
  the user chose to build all three surfaces together.
- **Staleness window**: the candidate list is a point-in-time snapshot; a
  runner could disconnect between the picker rendering and the user's
  selection. The re-issue then returns `PinnedNotFound`, surfaced as a normal
  error (user can retry). No special handling needed.

## One-line summary

Carry the server-computed candidate runners back inside the
`OpenInteractive` `ambiguous_runner` response (conditional schema block, string
cids), surface them via a typed `AmbiguousRunnerError`, and let TUI/WebUI pick
inline (CLI prints for `--runner` re-run) — completing the resume ladder
last-runner-pin → Any → picker.
