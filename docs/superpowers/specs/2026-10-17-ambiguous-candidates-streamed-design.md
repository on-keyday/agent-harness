# Ambiguous-runner candidates: one rule, one body on a stream — design

Status: DECIDED (operator, 2026-10-17) except the rows marked **this spec**.

## Problem

The operator wants every runner to offer every agent the host can launch
(`runner.py up --agents all`, the step after this one). A runner then
advertises up to eleven profiles instead of two, and three things break.

**1. The interactive candidate list outgrows one datagram.** With no `--agent`
and an unpinned selector, `handleOpenInteractive` expands each matching runner
into one (runner, profile) row per advertised profile
(`expandInteractiveCombos`, `server/task_handler.go:1267`) and puts every row
inline in `OpenInteractiveResponse` (`:1446-1463`). Every task-control message
is one datagram over UDP, dropped with no error at either end when it exceeds
about 1170 bytes. Measured by encoding the response with eleven profile names,
cid `ws:192.168.3.14:52826-39936` and hostname `gmkhost` (a throwaway test in
`runner/protocol`, same method as
`2026-10-16-board-row-responses-streamed-design.md`):

| matched root | runners | rows | bytes |
| --- | --- | --- | --- |
| `/home/kforfk/workspace/mimml` | 1 | 11 | 935 |
| `/home/kforfk/workspace/remote-agent-harness` | 1 | 11 | 1100 |
| `/home/kforfk/workspace/sisaku-security/rule-review` | 1 | 11 | **1177** |
| same three roots | 2 | 22 | **1843–2327** |

One runner is enough to lose the response. `inlineRowsAllowed`
(`runner/protocol/response_rows_test.go`) admits this format on the reason
"about 95 bytes each: over budget at about 12 runners", which counts rows as
runners; they are (runner, profile) combos.

**2. The TUI picker cannot reach a tenth row.** `RunnerPickerModel.Pick`
accepts the digits 1–9 only (`tui/runnerpicker.go:33`).

**3. oneshot answers ambiguity differently, and less usefully.** `handleSubmit`
does not expand profiles: with no `--agent` it takes the one matching runner's
default profile (`:1088-1091`), and when several runners match it answers
`ambiguous: <hostname>, <hostname>` in `error_msg` (`:1077-1085`, resume
`:1200-1208`). Same input, different outcome by path. The string carries
hostnames only, which on one host read `gmkhost, gmkhost` and cannot be used to
pin a runner.

**4. A picked runner is pinned by connection.** Pickers retry with
`SelectorOpts{Runner: cid}`, which builds `ByConnId` (`cli/selector.go:106`).
That stops matching once the runner reconnects (`server/registry.go:382-388`).
An interactive open happens at once, so this has not mattered. A oneshot
retried through a picker can sit `Queued`, and the scheduler deliberately pins
by selector rather than by the submit-time runner because a reconnect changes
the connection id (`server/scheduler.go:84-90`). A `ByConnId` pin would strand
it.

## Decisions taken

**operator** = chosen in conversation (2026-10-17); **this spec** = chosen by
the author while writing.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Candidates move onto a trsf stream, as the board responses did (`respondWithBody` / `cli.ReadResponseBody`) | operator |
| D2 | One candidate row per RUNNER, carrying the list of profiles it offers, instead of one row per (runner, profile) | operator |
| D3 | oneshot uses the same ambiguity rule as interactive: with no resolved profile and an unpinned selector, a multi-profile runner is ambiguous | operator |
| D4 | A picked candidate is re-issued pinned by runner identity (`ByRunnerId`), falling back to the cid only for a runner with no identity | operator |
| D5 | The same candidate body serves `OpenInteractiveResponse` and `SubmitResponse` (fresh and resume) | operator |
| D6 | The TUI picker becomes two steps (runner, then agent), skipping a step with one choice, and accepts `j`/`k` + Enter as well as digits | operator |
| D7 | The candidate stream id rides inside the `ambiguous_runner` branch of each response, not in a field present for every status | this spec |
| D8 | `expandInteractiveCombos` is renamed `expandCombos` and called from both submit paths; the combos are grouped into rows by runner, in candidate order | this spec |

## 1. Wire (D1, D2, D5, D7)

```
format RunnerCandidate:
    runner_id :RunnerID          # identity; zero for a runner that predates it
    cid_len :u16
    cid :[cid_len]u8             # ConnectionID.String(), display and fallback pin
    hostname_len :u16
    hostname :[hostname_len]u8
    matched_root_len :u16
    matched_root :[matched_root_len]u8
    active_tasks :u16
    max_tasks :u16
    profiles_len :u8
    profiles :[profiles_len]AgentProfileName   # what the caller may pick here

format RunnerCandidatesBody:
    candidates_len :u16
    candidates :[candidates_len]RunnerCandidate
```

`RunnerCandidate.profile` (one name) is replaced by `profiles`. For each row it
holds the profiles the ambiguity rule (§2) offered on that runner: every
advertised profile when none was resolved, or the one resolved profile.

`OpenInteractiveResponse`'s `ambiguous_runner` branch replaces
`candidates_len` / `candidates` with `candidates_stream_id :u64`.
`SubmitResponse` gains the same branch:

```
format SubmitResponse:
    status :SubmitStatus
    task_id :TaskID
    error_msg_len :u16
    error_msg :[error_msg_len]u8
    if status == SubmitStatus.ambiguous_runner:
        candidates_stream_id :u64
```

The server stops writing the hostname list into `error_msg` for this status.
`OpenInteractiveResponse` leaves `inlineRowsAllowed`; the guard then fails if a
list is put back inline.

`RunnerID` is a plain `[16]u8` (`message.bgn:746`), so the zero value encodes.
The comment on today's `RunnerCandidate` about a zero-value encoder panic
describes the address-shaped type this replaced.

## 2. Ambiguity rule (D3, D8)

Both paths resolve a profile first, then expand:

| path | resolved profile when `--agent` is absent |
| --- | --- |
| interactive, fresh | none |
| interactive, resume, pinned selector | the task's recorded profile (unchanged, `:1426-1430`) |
| interactive, resume, unpinned | none (unchanged: the picker is how an operator switches agent) |
| oneshot, fresh | **none** (was: the matching runner's default) |
| oneshot, resume | the task's recorded profile (unchanged, `:1177-1182`) |

`expandCombos(cands, profile, selectorKind)` then applies the existing three
rules: a resolved profile gives one combo per runner advertising it; none with
an unpinned selector gives runner × advertised profiles; none with a pinned
selector gives each runner's default. More than one combo is
`ambiguous_runner`. Exactly one is bound, and its profile is recorded on the
task.

A oneshot with no `--agent` on a single multi-profile runner therefore answers
`ambiguous_runner` where it used to run the default profile. No caller in the
repo or the live fleet depends on the old behaviour: the live task list held no
oneshot task, and the in-repo bare `submit` lines are examples and one test
script (§5).

## 3. Server (D1, D8)

- `respondWithBody` (`server/agent_taskcontrol.go:71`) moves to its own file
  and its log text stops saying "board response". The ordering rule it exists
  for is unchanged: the response first, then the body, on a goroutine.
- `handleOpenInteractive`, `handleSubmit` and `handleSubmitResume` return the
  response together with an optional `*RunnerCandidatesBody`. Their callers
  (`task_handler.go:371`, `:461`) send through `respondWithBody` when a body is
  present and as today otherwise.
- One function builds the body from combos, grouping by runner in the order
  `Registry.Candidates` returned them. All three handlers call it.

## 4. Client (D1, D4)

- `cli.RunnerCandidate` gains `RunnerID string` (32-hex, empty when zero) and
  replaces `Profile` with `Profiles []string`.
- `cli.AmbiguousRunnerError` is returned by `Submit` as well as by the
  interactive opens. One function reads it: it takes the response's stream id
  and calls `cli.ReadResponseBody`. The native open
  (`cli/open_interactive_native.go:69`), the wasm open
  (`cli/open_interactive_wasm.go:165`) and `roundTripSubmit` (`cli/submit.go:54`)
  call it.
- `RunnerCandidate.Pin() SelectorOpts` returns `{Runner: RunnerID}` when the
  identity is known and `{Runner: Cid}` otherwise. Every picker re-issues
  through it, so no surface chooses the pin form itself.

## 5. Surfaces

| Surface | Change |
| --- | --- |
| CLI, `session new` / `interactive` | `formatAmbiguousCandidates` prints one line per runner: hostname, `[active/max]`, root, `--runner <id>` (omitted when one runner), and `--agent` choices. Exit 3, as now |
| CLI, `submit` | Same table and exit 3 via the same formatter, instead of `submit ambiguous_runner: ambiguous: …` |
| TUI, interactive opens | The picker (`tui/runnerpicker.go`) becomes two steps per D6. Its caller (`tui/app.go:1300`) is unchanged apart from the candidate type |
| TUI, submit (the `s` popup `tui/overlays.go:530`, the cmdline `tui/dispatch.go:467`, resume `tui/app.go:2224`) | An `AmbiguousRunnerError` opens the same picker; a pick re-issues `DoSubmitWithOpts` with `Pin()` and the chosen agent. The pending-request bookkeeping the interactive path keeps (`pendingInteractive`) gains a submit form |
| WebUI, interactive opens | `pickRunnerAndRetry` (`main.js:4408`) draws one row per runner with one button per agent; a click re-issues with `runner: c.pin, agent` |
| WebUI, submit (`runCmd` `submit`, `main.js:7301`) | Routed through `routeAmbiguous` like the interactive paths, re-issuing `harness.submit` |
| wasm (`cmd/harness-webui-wasm/main.go:2683`, and the `submit` export `:1026`) | Both reject with `code: "ambiguous_runner"` and `candidates: [{runnerId, cid, pin, hostname, matchedRoot, activeTasks, maxTasks, profiles}]`. `pin` is `Pin().Runner`, computed in Go |
| `supervising-workers` skill (embedded + two mirrors), README examples | Bare `submit` lines gain `--agent`, and the skill says what an `ambiguous_runner` answer looks like and how to re-run |
| `scripts/e2e-oneshot-progress.sh:291` | Its no-`--agent` case asserts the ambiguity answer instead of a run on the default profile |

## 6. Version skew

The runner decodes none of these formats. Readers are `harness-cli` (agents run
it from the runner host's `bin/`), the TUI and the WebUI's wasm.

- Old client × new server: only an ambiguous answer differs. The old client
  decodes the stream id as a row count and fails; every other status is
  byte-identical.
- Order: server, then `make build` on every runner host, then the TUI and a
  WebUI reload.
- `scripts/wire-skew-check.sh` covers runner × server, which this does not
  change. It is run anyway.

## Testing

- **Guard:** `OpenInteractiveResponse` leaves `inlineRowsAllowed`;
  `TestResponseRowsAreNotInline` must stay green with no new entry.
- **Size:** a test encodes both responses' ambiguous form and asserts they are
  fixed-size and under 1170 bytes, and that a `RunnerCandidatesBody` for two
  runners × eleven profiles round-trips.
- **Rule (§2):** server tests for each row of the table — in particular a fresh
  oneshot with no `--agent` on one runner with two profiles answers
  `ambiguous_runner` with one candidate carrying both, and a oneshot resume does
  not.
- **Pin:** `Pin()` returns the identity when present and the cid when zero; a
  scheduler test that a task submitted `ByRunnerId` is dispatched to the same
  runner after it reconnects under a new cid.
- **Live, reproduced first:** `scripts/netem-lab` (1500-byte MTU) with one
  runner advertising eleven profiles on a long root. Before: `session new`
  without `--agent` from a UDP client hangs. After: the candidates arrive.
- **Every surface in §5 driven once** on a dummy harness: the CLI by its printed
  spelling, the TUI by keystroke (both picker steps, and `j`/`k`), the WebUI by
  click at 1280px and 390px.

## Out of scope

- `runner.py up --agents all` itself. It lands after this, because it is what
  makes the candidate list large.
- `profile_unavailable`'s `error_msg` (`profileUnavailableMsg`, `:1309`): it
  lists distinct profile NAMES, so it stays around 100 bytes with every preset
  advertised.
- A send-side size check on responses (the board change's reasoning holds).

## Amendment (2026-10-17, during implementation)

- **The oneshot resume path keeps its runner-level check.** §2 says the
  resume rows are unchanged; running resume through `expandCombos` as well
  would have newly expanded a legacy task whose recorded profile is empty.
  `handleSubmitResume` therefore still asks "more than one runner?" and
  returns the candidates, each offering the recorded profile or the runner's
  default.
- **The TUI re-issue travels in the result message.** `pendingInteractive` and
  `pickerArmed` are gone; `InteractiveReadyMsg.Retry` and
  `SubmitResultMsg.Retry` carry the function that re-issues the request that
  failed. `RunnerPickerModel.Key` returns the pick's label for the result line.
- **The TUI command line's `submit` honours `--host` / `--runner` / `--ip`.**
  It parsed them and passed an empty host before; `DoSubmitWithOpts` now takes
  `SelectorOpts`, which made the drop visible.

## Verification (2026-10-17)

**Over UDP, before and after** — `scripts/netem-lab/netem-lab.py --name amb up
--profile lan -- --agent-profiles <16 names>` (the runner advertised 17 with
the lab's own default), then from the lab's client namespace:

| Command | Before (`d4186f09`) | After (`70e27871`) |
| --- | --- | --- |
| `harness-cli session new -d --repo $REPO` | no output, killed by `timeout 30` (exit 124); server log `handleOpenInteractive: ambiguous ... combos=17` | the candidate table, exit 3, 354 ms |
| `harness-cli submit --repo $REPO --task x` | — | the candidate table, exit 3, 281 ms |
| `harness-cli submit --agent python ...` | — | a task id, exit 0 |

The "after" rows used this tree's `bin/harness-cli` by absolute path. The lab's
`exec cli -- harness-cli` resolves PATH, which here was an older build: against
the new server it decoded zero candidates and its old formatter panicked on
`cands[1:]`. That is §6's old-client case, and the reason the deploy order puts
`make build` on every runner host right after the server.

**Every surface in §5, on a dummy harness** with two runners on one root
(`python,bash` and `bash,sandbox-bash`):

- CLI: `session new -d` and `submit` printed the two-line table with
  `--runner <identity> --agent <…|…>` and exited 3; `submit --runner <id>
  --agent bash` from that table ran on the named runner.
- TUI (this tree's `harness-tui` inside a dummy session, driven by keystroke):
  `S` opened step 1 with both runners; `j`, Enter opened step 2 on the second;
  `1` opened `bash` there — `ls --json` shows the task `from=tui` assigned to
  that runner, and the result line read `pinned runner: dummy-amb … (bash)`.
  The command line's `submit --task echo-from-tui` opened the same picker;
  `1`, `2` submitted to `gmkhost`/`bash` with the prompt intact.
- WebUI (Playwright): Compose's open with the default agent showed one block
  per runner with an agent button each; clicking `bash` under `dummy-amb`
  opened it there. The command input's `submit --task echo-webui` opened the
  same modal and the pick submitted to `gmkhost`/`bash`. At 390px the modal is
  390 wide with no horizontal scroll. Screenshots (not committed):
  `webui-ambiguous-picker-1280.png`, `webui-ambiguous-picker-390.png` in the
  worktree root.

**Wire skew:** `scripts/wire-skew-check.sh` — PASS (`1bd0e9fe -> 70e27871`).
The runner decodes none of the changed formats.
