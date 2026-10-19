# Resume without spawn — `resume`, `resume_exact`, and the seat — design

## Problem

`spawn` is the only bit that authorizes a resume. `requiredCap` maps both
`submit` and `open_interactive` to `Capability_Spawn`
(`server/capabilities.go:38-39`), and the resume branch is the same request
with a non-zero `resume_task_id` (`server/task_handler.go:1053`, `:1386`). So a
task can either create new tasks AND resume, or do neither.

- **P1.** An agent driving a bash session through `exec_cowrite` types `exit`
  by mistake. The task goes terminal and the agent cannot bring it back: it
  does not hold `spawn`, and the operator does not want it to be able to
  create tasks.
- **P2.** Two different grants are wanted, and neither is expressible:
  - **A** — "may bring a task back, but only exactly as it was": no new
    prompt, args, agent, kind, caps or scope.
  - **B** — "may resume with changed settings, but must not create a task":
    the task count, worktree count and `ls` stay bounded.
- **P3.** Whoever opens an interactive session gets its control seat. The
  server attaches the opener's stream as the session's first control attach
  (`server/task_handler.go:1530` onward); `-d` is the client opening and then
  dropping that stream (`tui/interactive.go:320`, `stream.Close()` after
  `OpenInteractive`). The wire has no way to open without taking the seat. So a
  holder of A who is not otherwise allowed to type into the session could type
  into the one it just revived — bash commands, or prompts to a claude running
  with that task's caps. The same asymmetry already exists for `spawn`: a
  spawn-only agent can type into a session it opens, while re-entering it later
  needs `exec_cowrite` or stronger.
- **P4.** `spawn` bundles two different operations: creating a task, which
  names no existing task, and resuming one, which does. Its scope can only ever
  bound the second — the two resume gates (`server/task_handler.go:1148`,
  `:1392`) are the only readers of `spawn`'s scope — so a `--scope-for
  spawn=…` reads as bounding creation while bounding resumes.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-19). **this spec** =
the author chose it while writing; those rows are the ones to check. Withdrawn
rows stay, struck through, with what replaced them.

| # | Decision | Decided by |
| --- | --- | --- |
| ~~D1~~ | ~~Three ranked powers, `spawn ⊇ resume ⊇ resume_exact`~~ — **withdrawn**, replaced by D20 | operator, withdrawn by operator |
| D2 | `resume` = resume with changed settings, no new task id (P2-B). `resume_exact` = resume with every setting taken from the record (P2-A) | operator |
| D3 | Names `resume` and `resume_exact`; the CLI spells A as `--resume <id> --exact` | operator |
| D4 | An explicit wire flag `resume_exact :u1`. The server never downgrades a plain resume to an exact one depending on the caller's caps | operator |
| D5 | `all` widens `0xfffff` → `0x3fffff` | operator |
| D6 | Every terminal status (Succeeded / Failed / Cancelled) may be resumed under either bit, as `spawn` resumes today. Whether to allow it at all is decided by granting the bit | operator |
| D7 | The seat follows `exec_*` for EVERY open, a fresh one included: opening without detaching requires `exec_control`. A new wire flag `detached :u1` opens without attaching anyone, and `-d` sets it | operator |
| D8 | A resumed task keeps its recorded caps unless the request re-grants them, under either bit — unchanged from today, even when the record exceeds the resumer's caps | operator |
| D9 | Bit values `resume = 0x100000`, `resume_exact = 0x200000` | this spec |
| D10 | Under `resume_exact` the request carries NO settings: an empty prompt, empty `extra_args`, an empty `agent_profile`, `resume_caps_override = 0`, `scope_present = 0`, and a path / `event_stream` that matches the recorded kind. Anything else is refused with `resume_exact_mismatch`, never ignored | this spec |
| D11 | Under `resume_exact` the caller may still choose the runner (selector), `resume_conversation`, `x11`, `detached` and the initial size. None of them changes what the task runs or what it may do | this spec |
| ~~D12~~ | ~~The scope that binds a resume is the bit being EXERCISED, the attach rule~~ — **withdrawn**, replaced by D19 | this spec, withdrawn by operator |
| D13 | Attenuation stays plain `creator & requested`: a creator holding `resume` but not `resume_exact` cannot hand `resume_exact` to a child — the same limit the attach ranks already have | this spec |
| D14 | With `detached = 1` the initial PTY size travels in the request, since there is no client stream to carry the window-size frame | operator |
| ~~D15~~ | ~~A scope override naming `spawn` is refused~~ — withdrawn with D12, **reinstated as D21** under D20 | — |
| D16 | No skew guard. Rollout is server first, then every client; § Wire states what a skewed pair actually does | this spec (builder check) |
| D17 | Tasks recorded with the old `all` (`0xfffff`) are not rewritten; they render as a name list instead of `all` | this spec (builder check) |
| D18 | An exact resume with an Any selector is pinned by the SERVER to the task's last runner (`AssignedTo`), falling back to Any when that runner is not connected — the TUI `r` behaviour (`openResumeWithRetry`), moved server-side so every client gets it | operator |
| D19 | A resume target is allowed when ANY bit the caller HOLDS that permits this resume has the target in that bit's own scope | operator (replacing D12) |
| D20 | The powers are SPLIT: `spawn` authorizes creating a task and nothing else; `resume ⊇ resume_exact` is a rank checked by implication, like the attach ranks. `spawn` implies neither | operator (replacing D1) |
| D21 | A scope override whose mask contains `spawn` is refused (`validateScope`): under D20 `spawn` names no task, so the override would bound nothing. None is in use today | this spec; "none in use" is the operator's |
| D22 | No migration. A task that holds `spawn` today — including every task holding the old `all` — cannot resume after the change until it is granted `resume`. The operator re-grants as needed | operator |

## The powers (D2, D19, D20)

| Request | Bit check (`hasAnyCap`) | Target allowed when |
| --- | --- | --- |
| fresh submit / open (`resume_task_id` zero) | `spawn` | — no task is named (but see § The seat for a fresh attached open) |
| resume, `resume_exact = 0` | `resume` | holds `resume` ∧ T ∈ scope(`resume`) |
| resume, `resume_exact = 1` | `resume \| resume_exact` | (holds `resume` ∧ T ∈ scope(`resume`)) ∨ (holds `resume_exact` ∧ T ∈ scope(`resume_exact`)) |

Within the rank, a holder of `resume` can always do an exact resume, so
requiring `resume_exact` as well would be bookkeeping, not confinement — the
reasoning `attachModeCap` records. The masks above come from one helper,
`resumeRequestCap(resumeID protocol.TaskID, exact bool) protocol.Capability`,
beside `attachModeCap` in `server/capabilities.go`. The target rule comes from
`resumeTargetAllowed(cid string, exact bool, targetHex string) bool` beside
it, which loops over `resumeRequestBits(exact)` (`resume`, plus `resume_exact`
when exact) and calls `h.inScope(cid, b, targetHex)` for each bit the caller
holds.

A missing bit is a `PermissionDenied` carrying the mask. `CapabilityDeniedError`
already renders a multi-bit mask as "requires any of …"
(`cli/capability_error.go:36`). An out-of-scope target stays `resume_not_found`,
as now.

**Why split (D20, replacing D1).** The first draft ranked `spawn` above
`resume`, so `spawn` kept authorizing resumes. That left P4 in place, and the
question of whose scope binds a spawn holder's resume had no clean answer:
binding it by `resume`'s scope (D12) silently dropped every `--scope-for
spawn`, and binding it by `spawn`'s scope (D19's first form) kept a scope whose
name says "creation" bounding resumes. Splitting makes each bit name one
operation, and each scope bound that operation's targets. The operator chose it
while no `--scope-for spawn` is in use, so there is nothing to carry over.

**Under D19,** narrowing the resumes of a task holding BOTH bits means narrowing
both scopes, since either one admits a target. Holding both is redundant
(`resume` implies `resume_exact`), so the case should be rare. This differs
from attach on purpose. Attach binds by the MODE's own bit because three modes
of one verb share one target. Here the two bits are grants the operator writes
separately, and each grant's scope is what the operator wrote for it.

**`spawn`'s scope (D21).** After D20 nothing reads it. `validateScope` refuses
an override whose mask contains `spawn`, with "spawn names no task; narrow
resume / resume_exact instead". Without that check, `--scope-for spawn=…` would
be accepted and do nothing, which checklist item 33 forbids. The task's BASE
scope is unaffected: it is not `spawn`'s, and every bit without an override
reads it.

`capTargetClasses` moves `spawn` to `capNoTargetResolution` ("creating a task
names no existing one") and classifies `resume` and `resume_exact` as
`capResolvedVia: "resumeRequestBits"`. Classifying them by literal would be
unsound anyway: the source scan matches substrings, and `Capability_Resume` is
a prefix of `Capability_ResumeExact`.

**What `resume` grants (D8).** `resume` can change the prompt and the agent
profile, so it can run arbitrary commands — through the bash profile, for
example — exactly as `spawn` can. What it withholds is a new task id. It also
inherits today's property that a resume without `resume_caps_override` keeps
the task's recorded caps, which may exceed the resumer's. Within the default
subtree scope a target was created with caps no wider than its creator's, so
this exceeds the resumer only when the operator put a stronger task in scope or
widened a child with `caps set`. Clamping on resume was considered and
rejected: it breaks P1. An operator-created bash task revived by an agent that
holds `exec_cowrite,resume_exact` would come back with the agent's caps, and
the `harness-cli` inside that shell would lose its powers.

## Exact resume (D10, D11)

The server fills every setting from the `TaskEntry`:

| Setting | Under `resume_exact` | A plain resume (today's behaviour, unchanged) |
| --- | --- | --- |
| prompt | the recorded `Prompt` (submit path); `""` (open path) | the request's |
| `extra_args` | the recorded `ExtraArgs` | the request's — **replaces** the record, so a resume without args clears them (`server/taskstore.go:448`) |
| agent profile | the recorded `AgentProfile`, for pinned and unpinned selectors alike (no picker re-fire) | request's, else recorded when pinned, else the picker |
| kind | must equal the recorded `Kind`: submit ⇔ `Oneshot`, open with `event_stream = 0` ⇔ `Interactive`, open with `event_stream = 1` ⇔ `Stream` | set by the path |
| caps / scope | recorded (override bits must be 0) | re-granted when the bit is set |
| runner (selector) | a pinned selector is the caller's choice. An Any selector is tried first as `by_runner_id` = the recorded `AssignedTo`, then as Any if that runner is not connected (D18). Candidates are filtered by the recorded profile either way | caller's choice |
| `resume_conversation`, `x11`, `detached`, initial size | caller's choice | caller's choice |

**Why the last runner by default (D18).** On the fleet every slot advertises
every preset, `bash` included (`scripts/agent_presets.py:297`), and slots on
one host share roots. So an Any selector filtered by the recorded profile
answers `ambiguous_runner`, and the documented recovery command would stop
there. A runner on another host also lacks the task's worktree, which is not
"exactly as it was". The fallback still reaches `ambiguous_runner` after the
last runner restarted, since a restart issues a new `RunnerID`
(`tui/interactive.go:140`). The caller then pins with `--runner` from the
candidate list, as an operator does today. The handler reads `AssignedTo`
during candidate resolution, before `Tasks.Resume`, which zeroes it
(`server/taskstore.go:441`).

`Tasks.Resume` already takes every one of these as an argument, so exact resume
is a handler change: the handler reads the entry and passes the recorded values.
The `task_resumed` WAL event then records the same values again, so replay
needs nothing new.

Validation runs before runner resolution, in the order the table lists, and the
first violation answers `resume_exact_mismatch`. On the submit path the
`error_msg` names the field (`resume_exact: --agent-arg is a setting; an exact
resume takes the task's recorded one`). The open path has no `error_msg`, so
the status is the whole answer and the client names the conflict from the flags
it sent, as it does for `scope_not_permitted`. `resume_exact = 1` with a zero
`resume_task_id` is also `resume_exact_mismatch`.

## The seat (D7, D14)

`OpenInteractiveRequest` gains `detached :u1`. With `detached = 1` the server
creates the task and the session mux exactly as now, but creates no client
stream and attaches nobody, and the response carries no `stream_id` (its
`attached` bit is 0 — see § Wire for why this is not a zero `stream_id`). With
`detached = 0` (an attached open), the opener must hold the control power over
the target, checked the way an `attach --control` is:

1. `hasAnyCap(callerCaps, attachModeCap(AttachMode_Control))`, else
   `PermissionDenied` naming `exec_control`. Checked after the spawn / resume
   bit, so a caller missing both hears about the bit that names the operation.
2. The target is in the control scope:
   - resume: `inScope(cid, attachModeScopeCap(AttachMode_Control), idHex)`.
   - fresh open: the new task will be the caller's direct child, so it is in
     the control scope iff that scope's base reaches direct children —
     `subtree`, `descendants` or `global`. `none` and pure `ids:` do not, because
     an id list cannot name a task that does not exist yet. This is decided
     before `Tasks.Create`, so no dead task is left behind, by one helper,
     `scopeCoversNewChild(scope Scope, c protocol.Capability) bool`, pinned by a
     test that creates a real child under each base and compares the helper
     with `actionSet`.

   Out of scope answers a new `OpenInteractiveStatus.detach_required`: the
   caller may open this task but may not hold its seat. Not `resume_not_found`,
   because the resume scope has already shown the task to the caller.

Operators hold `all` with a global scope, so nothing changes for them. An agent
holding `spawn` without `exec_control` can no longer open a session attached;
`-d` keeps working.

**Initial size (D14).** A detached start sends its PTY size today as a window
size frame on the stream it is about to drop (`cli/initial_winsize.go`); a
session that never gets one sits at 0x0, and a full-screen program inside it
draws nothing (`tui/interactive.go:330-334`). With no stream, the size goes in
the request. When `detached = 1` and both values are non-zero, the server builds
a `TerminalWindowSize` frame and applies it through
`SessionMux.applyWinSizeFrame` (`server/session_mux.go:891`) before returning.
`x11_enabled = 1` with `detached = 1` is refused with `internal_error`, the
answer `x11` with `event_stream` already gets (`server/task_handler.go:1566`):
the X tunnel needs a live client.

## Wire

All of it, in one place (`feedback_no_split_schemas`). `runner/protocol/message.bgn`:

```
enum Capability:
    :u32
    none           = 0x000, "none"
    # spawn authorizes CREATING a task (submit / open_interactive with a zero
    # resume_task_id). It does not authorize a resume: that is resume /
    # resume_exact, which spawn does not imply. Creation names no existing
    # task, so no scope bounds this bit and an override naming it is refused.
    spawn          = 0x001, "spawn"
    …
    board_peer     = 0x80000, "board_peer"
    # resume authorizes re-queuing an existing terminal task under its own id,
    # with the request free to change its prompt, args, agent, kind, caps and
    # scope. It is NOT weaker than spawn at running commands (a new prompt or
    # the bash profile is arbitrary execution); what it withholds is a new
    # task. A task resumed without resume_caps_override keeps its recorded
    # caps, which may exceed the resumer's. Default-off, appended.
    resume         = 0x100000, "resume"
    # resume_exact authorizes the same re-queue with every setting taken from
    # the task's record (SubmitRequest / OpenInteractiveRequest.resume_exact):
    # the caller picks only the runner, resume_conversation, x11, detached and
    # the initial size. Implied by resume. Default-off, appended.
    resume_exact   = 0x200000, "resume_exact"
    all            = 0x3fffff, "all"

format SubmitRequest:
    …
    scope_present :u1
    # resume only: 1 = take every setting from the task's record. The request
    # must carry none (empty prompt / extra_args / agent_profile, both override
    # bits 0), else resume_exact_mismatch. Satisfied by resume_exact or resume.
    resume_exact :u1
    reserved :u4
    …

enum SubmitStatus:
    …
    # resume_exact was set and the request also carried a setting, or the
    # recorded kind is not the one this path opens, or there was no
    # resume_task_id. error_msg names which.
    resume_exact_mismatch = "resume_exact_mismatch"

format OpenInteractiveRequest:
    …
    event_stream :u1
    # Same as SubmitRequest.resume_exact; event_stream must match the recorded
    # kind (0 = interactive, 1 = stream).
    resume_exact :u1
    # 1 = open without attaching the requester: no client stream is created and
    # the response carries no stream_id. 0 = the requester takes the control
    # seat, which requires exec_control over the target.
    detached :u1
    reserved :u1
    resume_task_id :TaskID
    …
    if x11_enabled == 1:
        x11 :X11Forward
    # Present ONLY when detached == 1 (no invisible bytes otherwise). 0 in
    # either = no initial size.
    if detached == 1:
        initial_rows :u16
        initial_cols :u16

enum OpenInteractiveStatus:
    …
    # See SubmitStatus.resume_exact_mismatch. No error_msg on this response;
    # the client names the conflict from the flags it sent.
    resume_exact_mismatch = "resume_exact_mismatch"
    # The caller may open this task but may not hold its control seat: the
    # target is outside its exec_control scope. Open with detached = 1.
    detach_required       = "detach_required"

format OpenInteractiveResponse:
    status  :OpenInteractiveStatus
    task_id :TaskID      # populated when status == ok
    # 1 = the server attached the requester and stream_id names its stream.
    # 0 on every non-ok status and on a detached open.
    attached :u1
    reserved :u7
    if attached == 1:
        stream_id :u64   # (the existing comment on the splice moves here)
    if status == OpenInteractiveStatus.ambiguous_runner:
        candidates_stream_id :u64
```

**Why a bit and not `stream_id = 0`.** The server's trsf is created with
`isServer = true` (`server/server.go:1240`), and on that side
bidirectional stream ids start at `ServerBidirectionalStart = 0` and step by 4
(objtrsf `trsf/api.go:14-22`). The first bidirectional stream the server opens
toward a client connection IS stream 0, so 0 cannot mean "no stream" for this
response. The existing "0 = no stream" fields are safe only because they name
server-created SEND streams (`CreateSendStream`), whose ids start at 2.

The comment on `requiredCap` and on `SubmitRequest.requested_caps`
(`Capability.all (0xfff)` there is already stale) are updated in the same
change.

**Rollout (D16).** A skewed pair does NOT reliably fail at decode. The two
request flags take bits from `reserved` in an existing flags byte, so the
layout before them is unchanged:

| Pair | What happens |
| --- | --- |
| old client → new server | decodes. An old `session new -d` is an attached open, so it is refused for a caller without `exec_control`; an old `--resume` is refused for a caller without `resume` |
| new client → old server, `submit --resume X --exact` | decodes and runs as a PLAIN resume with an empty prompt and empty `extra_args`, which clears the recorded args — the opposite of exact |
| new client → old server, `-d` | the conditional size block adds 4 bytes, `DecodeExact` fails, and the old server sends no response: a hang |
| new server → old client, any open | `OpenInteractiveResponse` gained `attached` before `stream_id`, so the old client misreads the response |

A guard is not added. The project carries no compat shims by design
(Pitfall 10), and an old server cannot be taught to refuse anything. So the
order is the guard: **restart the server first, then every client** (every
`harness-cli` binary in use — the one agents run included — the TUI, and the
WebUI wasm). The runner wire is unchanged, but
`scripts/wire-skew-check.sh` is run anyway.

**Disk and existing grants (D17, D22).** The WAL persists `Capabilities` as a
JSON number (`server/wal.go:61`) and no request bytes, so old records replay
unchanged — and that is the point of D22: nothing is rewritten. A task holding
`spawn` keeps it and can still create tasks, but it cannot resume until the
operator grants `resume` (`caps set`). That includes every task holding the old
`all` (`0xfffff`), which lacks both new bits. `CapsLabel` prints `all` only for
the exact literal (`cli/verb/caps.go:235`), so such a task now renders as a
list of twenty names (D17). That is true — the task does not hold the new
bits — and it is what every earlier widening of `all` did.

## Server changes

- `requiredCap` loses `Submit` and `OpenInteractive`. Both are gated in their
  dispatch cases, as `AttachSession` is: `resumeRequestCap` with `hasAnyCap`,
  then (open only) the seat check. `kindCapClass` reclassifies both as
  `capInHandler` with the reason "the bit depends on resume_task_id,
  resume_exact and detached".
- `handleSubmitResume` and the resume branch of `handleOpenInteractive` take
  the exact flag, run the D10 validation, and pass the recorded values to
  `Tasks.Resume`. Their target gate becomes `resumeTargetAllowed(cid, exact,
  idHex)` (D19).
  On the open path the recorded `ExtraArgs` must ALSO go into the runner's
  `OpenExecRunnerRequest.ExtraArgs`, which today is fed from `req.ExtraArgs`
  (`server/task_handler.go:1554`), not only into `Tasks.Resume`.
- `handleOpenInteractive` skips `tuiConn.CreateBidirectionalStream` and the
  initial `Attach` when `detached = 1`, and applies the initial size (D14).
  Nobody attaches, so the `OnDetach` hook that turns today's `-d` into
  `Detached` never fires. The handler calls `SetDetached` itself, so a detached
  open ends in the same status a `-d` reaches today.
- `validateScope` refuses an override mask containing `spawn` (D21).

## Surfaces

| Surface | What |
| --- | --- |
| CLI verb | `--exact` in `spawnFlags` (`cli/verb/spawn.go`), on `submit`, `interactive` and `session new`. A `Requirement` makes it need `--resume`. `Rule`s make it exclusive with `--task` / the prompt text, `--agent`, `--agent-arg`, `--caps`, `--scope` and `--scope-for`. The server's D10 check is the authority; the rules make the common mistake fail before a round trip |
| CLI `-d` | `session new -d` sets `detached` and the initial size in the request instead of opening and dropping a stream |
| Shared funnel | `cli.SessionOpts` gains `ResumeExact` and `Detached`; `buildSubmitRequest` / `buildOpenInteractiveRequest` set them. Every hand-written `SessionOpts` literal in `tui/` goes through `Authority.opts` (checklist item 28a) |
| CLI errors | `resume_exact_mismatch` and `detach_required` rendered with the conflicting flag / "use -d"; a `--scope-for spawn=…` refusal names `resume` / `resume_exact` |
| caps catalog | `resume`, `resume_exact` inserted right after `spawn` in `GrantableCaps`, weakest-first within their rank (`resume_exact`, then `resume`). `CapDescription`: `spawn` says "create tasks (submit, open sessions); does NOT resume — see resume"; `resume` says it implies `resume_exact` and keeps recorded caps; `resume_exact` names `--exact`. `spawn`'s and `exec_control`'s descriptions gain "opening a session without -d needs exec_control" |
| TUI cmdline | `--exact` arrives through the verb table; its request builds carry it (28a) |
| TUI detached start | `DoStartDetachedSession` sets `Detached` instead of `stream.Close()` |
| TUI keybindings `r`/`R`/`u`/`U` | `--exact` **omitted**: these are operator surfaces and the operator holds `all`. Exact resume is for confined agents |
| TUI / WebUI caps pickers and chips | built from `GrantableCaps`, so the two bits appear with no surface-specific code; verified, not assumed |
| WebUI command input | `--exact` and `-d` via the wasm bridge's parse; the wasm `OpenInteractive` sets `Detached` for `-d` |
| WebUI resume buttons | `--exact` **omitted**, same reason as the TUI keys |
| README | the capability list; the session section's `-d` paragraph; every example that grants `spawn` to a supervisor expected to resume (`README.md:889` is one) also grants `resume` |
| Agent skill | `runner/agentskills/supervising-workers/SKILL.md` (and both mirrors): resuming needs `resume`, not `spawn` (its advice to resume rather than spawn, `:173`, and its `--caps` examples, `:343`); how a confined supervisor revives a session (`session new --resume <id> --exact -d`, then `session send`); an attached open needs `exec_control` |

The `surface-parity-checklist` is walked item by item while writing the plan,
and this table is checked back against the code at the end (item 39).

## Testing

Unit, server:

- the gate matrix: {fresh, resume, exact} × caller caps {none, `spawn`,
  `resume_exact`, `resume`} → allowed or `PermissionDenied` with the expected
  mask. In particular a `spawn`-only caller is refused a resume ("requires
  capability resume") and a `resume`-only caller is refused a fresh spawn
- scope (D19): a `resume`-only holder with `--scope-for resume=ids:A` resumes A
  and no other task in its subtree; a `resume_exact`-only holder with
  `--scope-for resume_exact=ids:X` exact-resumes X and nothing else; a holder of
  `resume` (scope `ids:A`) and `resume_exact` (scope `ids:B`) exact-resumes A and
  B but plain-resumes only A
- D21: `--scope-for spawn=…` refused on submit, open and `caps set`
- D10: each field set alone under `resume_exact` → `resume_exact_mismatch`;
  kind mismatch on both paths; exact with no `resume_task_id`
- D18: an exact resume with Any binds to the recorded runner while two
  candidates advertise the profile; with that runner gone it answers as Any
  does (one candidate → that one, two → `ambiguous_runner`)
- an exact resume preserves the recorded `ExtraArgs`, `Prompt` and
  `AgentProfile`, where a plain resume without args clears `ExtraArgs` (today's
  behaviour, pinned so the contrast is visible)
- seat: an attached open without `exec_control` → `PermissionDenied`; with
  `exec_control` but the target out of its scope → `detach_required`; a fresh
  attached open under each scope base matches `scopeCoversNewChild`, and that
  helper agrees with `actionSet` over a real child
- `detached = 1`: no client stream, `attached = 0` with no `stream_id` on the
  wire, the initial size reaches the runner stream, `x11` refused; an attached
  open whose stream is the connection's first server-created bidirectional
  stream (id 0) still reads `attached = 1`
- the completeness guards (`TestCapClassAgreesWithRequiredCap`,
  `TestCapabilityTargetClassesMatchTheSource`,
  `TestEveryCapabilityDeclaresHowItsTargetIsResolved`) are seen failing before
  the reclassification lands

Unit, cli: the `--exact` rules on all three verbs; `CapabilityDeniedError` for
the resume masks.

Live, against `scripts/dummy-harness.sh` (Pitfall 13), in the spelling the help
prints. The use-case run uses TWO runner slots on the dummy server serving the
same root, with the same presets, because a single runner cannot show D18:

1. The operator opens `session new -d --agent bash` → task X.
2. The operator submits an agent with `--caps exec_cowrite,resume_exact --scope
   ids:X`. From inside it: `session send X 'exit'`. X goes terminal.
3. The agent runs `session new --resume X --exact -d` → ok; `ls` shows X
   running again with its recorded args; `session send X 'echo ok'` works.
4. The same agent runs `session new --resume X -d --agent bash` → "requires
   capability resume"; `session new -d --agent bash` → "requires capability
   spawn"; `session new --resume X --exact` (no `-d`) → requires
   `exec_control`.
5. TUI `session new -d` and WebUI `session new -d` still start a sized session
   (a full-screen program inside draws).

Before landing: `make` targets, `scripts/wire-skew-check.sh`.

## Non-goals

- **No new verb.** The resume powers ride the existing `submit` /
  `open_interactive` kinds and `--resume`.
- **No clamp of a resumed task's caps** (D8).
- **No migration of existing grants** (D22).
- **The implication is not applied when caps are handed to a child** (D13).
  `intersectCaps` is a plain bitwise AND of the creator's bits and the requested
  bits; the creator's caps are NOT first expanded to include the weaker bits its
  stronger ones imply. So a creator holding `resume` alone cannot hand
  `resume_exact` to a child, exactly as a creator holding `exec_control` alone
  cannot hand `exec_view` today. If a supervisor needs to, it is granted the
  weaker bit itself.
- **Attach scope is unchanged.** Attach still binds by the mode's own bit
  (`attachModeScopeCap`), so a task holding only `exec_control` narrowed by
  `--scope-for exec_control=ids:A` can view and cowrite anything in its base
  scope. Whether to move attach to the D19 rule is a separate question; the
  seat check here reads only `exec_control`'s scope and is unaffected either way.
