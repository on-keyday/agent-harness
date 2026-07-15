# Multi-Agent-Profile Runner (per-session agent selection)

Date: 2026-07-15
Status: design

## Problem

The agent CLI a runner runs (`claude` / `codex` / `gemini` / custom) is baked
into the **runner process** at startup:

- `--agent-bin` (default `claude`) plus the `--agent-*-argv` templates are
  fixed `mainConfig` flags (`cmd/agent-runner/main.go`), threaded into
  `runner/connect.go` and `runner/session.go` unchanged for the process
  lifetime.
- `RunnerHello.agent_bin` carries a single basename; the server stores and
  *displays* it (`server/registry.go`, `cli/list.go`, `tui/*`) but **dispatch
  has no agent selector** — a task is matched to a runner by roots/host only.

Consequence: to run both Claude and Codex against the same repo you must stand
up **two runner processes** with different `--agent-bin`, doubling pid / log /
slot / hostname management. It also collides with the known `AmbiguousRunner`
condition (same host × identical roots → dispatch ambiguity), forcing distinct
`--hostname` and adding more operational surface.

The chosen pain to solve (confirmed with the operator): **process / management
doubling**. The root cause is that agent identity is bound to the runner
*process* instead of the *session/task*.

## Relationship to the 2026-07-02 Agent Runtime Adapter spec

`docs/superpowers/specs/2026-07-02-agent-runtime-adapter-design.md` defined the
**adapter boundary** — what is common (identity, tickets, agentboard, wake,
cross-tool instruction/skill injection via `AGENTS.md` / `GEMINI.md` /
`.agents/skills/`) versus runtime-specific (prompt/resume/hook conventions).
That spec:

- Left the *adapter profile* itself as **design guidance, not implemented**.
- Assumed **one runtime per runner** and, in **decision 6, explicitly deferred
  adding any new wire protocol field**.

This spec is the follow-up that **implements the adapter profile as a named,
per-runner set and makes it selectable per session**. Doing so genuinely
requires new wire fields (the selection cannot be carried by convention — a
missing field must extend the schema, not hide in a payload), so this work
**revisits decision 6 on purpose** and adds the fields below.

Because cross-tool instruction/skill injection is *already* a common-layer
behavior (that earlier spec), a Codex/Gemini session started by a multi-profile
runner already receives `AGENTS.md` / `GEMINI.md` / `.agents/skills/`. That is
why per-profile injection differentiation is out of scope here (see Non-Goals).

## Goals

- One runner process advertises **multiple named agent profiles** and can run
  any of them.
- A session/task **selects a profile by name**; the server dispatches to a
  runner that advertises it and the runner execs that profile's binary + argv.
- Reachable from **all three operator surfaces** (CLI, TUI, WebUI).
- The `AmbiguousRunner` pain for the claude-vs-codex case disappears, because
  the two runtimes now collapse into one runner.
- The Go/protocol layer stays **agent-agnostic**: it knows only names + argv
  templates. Concrete per-agent argv defaults live in the `scripts/runner.py`
  preset, not in Go.

## Non-Goals (v1)

- **Per-profile instruction/skill injection differentiation.** Injection stays
  the current global behavior (`.claude/{settings.json,skills}` plus the
  common-layer `AGENTS.md` / `GEMINI.md` / `.agents/skills/`). `skills_injected`
  keeps its current meaning. Differentiating injection *by selected profile* is
  a natural follow-on but is deferred.
- No per-profile capability model, no per-profile resource limits.
- Conversation continuation stays profile-relative (§4b); the harness does not
  attempt to migrate one agent's conversation into another's store.

**In scope (added during design):** resume becomes able to switch both the
**agent profile** (§4b) and the **mode / Kind** (§4c) per open — `resume_task_id`
identity is decoupled from the `(mode, profile, runner)` chosen at each open.
Both were found to be incidental (handler-guard-level), not essential,
constraints.

## Design

### 1. Profile model

An **agent profile** is a named record fully describing how to launch one
runtime:

```
AgentProfile {
  name                 string   // selection key, e.g. "claude", "codex"
  bin                  string   // basename or path of the agent binary
  agentArgs            []string // baseline extra args (was --agent-args)
  oneshotArgv          []string // {args} {prompt} template
  resumeOneshotArgv    []string // {args} {prompt} template (resume)
  resumeInteractiveArgv[]string // {args} template (resume interactive)
}
```

A runner holds an **ordered set** of profiles. The **first** profile is the
**default** (selected when a session names none). `name` values are unique
within a runner; empty selection means "default".

### 2. Runner configuration (Go generic; runner.py preset for ergonomics)

- The existing single-agent flags (`--agent-bin` / `--agent-args` /
  `--agent-oneshot-argv` / `--agent-resume-oneshot-argv` /
  `--agent-resume-interactive-argv`) continue to define **the default (first)
  profile**. Its `name` defaults to the basename of `--agent-bin` (e.g.
  `claude`). This keeps every existing `runner.sh up` invocation working
  unchanged, advertising exactly one profile.
- **One new flag** `--agent-profiles <json>` (value may be inline JSON or
  `@path`). It is a JSON array of `AgentProfile` records (the `name`/`bin`
  fields required; argv fields default to the same built-in templates used for
  the default profile when omitted). Profiles from this flag are **appended
  after** the default profile. Duplicate `name` is a startup error.
- Concrete per-agent argv defaults are **not hardcoded in Go**. Instead
  `scripts/runner.py` gains an ergonomic preset:
  `runner.sh up --agents claude,codex[,gemini]` expands the known agents into
  the `--agent-profiles` JSON. This matches the repo convention "bake arg
  defaults into the runner-up preset; keep wrappers pure pass-through".
- Validation: `ValidateOneshotArgvTemplate` /
  `ValidateResumeInteractiveArgvTemplate` (in `runner/agent_command.go`) run
  per profile.

### 3. Wire changes (`runner/protocol/message.bgn`)

All additions are appended to existing formats. Both server and runner are
built and deployed together (solo operation, established precedent of wire
bumps), so no straddling-version compatibility shim is required. A new
`AgentProfileName` format carries one length-prefixed name.

```
format AgentProfileName:
    name_len :u8
    name :[name_len]u8
```

- **RunnerHello** — advertise the profile-name set (names suffice for dispatch
  filtering and display; the bin/argv stay runner-private). `agent_bin` is
  retained as the **default profile's** basename for existing display paths.
  ```
  agent_profiles_len :u8
  agent_profiles :[agent_profiles_len]AgentProfileName
  ```
- **RunnerInfo** — mirror the same field so the server echoes it to operator
  surfaces (list/detail).
  ```
  agent_profiles_len :u8
  agent_profiles :[agent_profiles_len]AgentProfileName
  ```
- **SubmitRequest** and **OpenInteractiveRequest** — the client's requested
  profile (empty = runner default):
  ```
  agent_profile_len :u8
  agent_profile :[agent_profile_len]u8
  ```
- **AssignTaskBody** and **OpenExecRunnerRequest** — the server-resolved
  profile the runner must exec with (empty = runner default):
  ```
  agent_profile_len :u8
  agent_profile :[agent_profile_len]u8
  ```
- **TaskInfo** — the **latest** resolved profile (updated on each Create/Resume,
  a per-open mutable field like `assigned_to`/`resumed_by_kind`; see §4c), for
  display and as the next resume's default:
  ```
  agent_profile_len :u8
  agent_profile :[agent_profile_len]u8
  ```
- **RunnerCandidate** (from the AmbiguousRunner candidate-picker design, if that
  lands first — otherwise introduced here) — gains a `profile` field so each
  picker row carries which `(runner, profile)` combo it represents (§4a):
  ```
  profile_len :u16
  profile :[profile_len]u8
  ```
  The `OpenInteractiveResponse` candidates block then enumerates one entry per
  combo instead of one per runner.

### 4. Server dispatch (`server/task_handler.go`)

- When the request's `agent_profile` is **empty**: behavior is unchanged —
  match by roots/selector, runner uses its default profile.
- When **non-empty**: restrict the candidate runner set to runners whose
  advertised `agent_profiles` contains that name, *before* applying the
  existing roots/selector/ambiguity logic. Then:
  - No candidate → new `SubmitStatus.profile_unavailable`.
  - Exactly one → assign, and set `AssignTaskBody.agent_profile` /
    `OpenExecRunnerRequest.agent_profile` to the requested name; record it in
    `TaskInfo.agent_profile`.
  - Multiple → existing `ambiguous_runner` path (now far less likely, since one
    runner serves both runtimes).
- Add `profile_unavailable = "profile_unavailable"` to `SubmitStatus`. The
  interactive open path returns the analogous error through its existing status
  channel.
- Resume profile resolution (§4b) happens in the same handler: when
  `resume_task_id` is set and no `--agent` was given, the requested profile
  defaults to the resumed task's `TaskInfo.agent_profile`; an explicit `--agent`
  overrides it freely. There is **no** cross-agent conflict to reject
  (`resume_conversation` is profile-relative — see §4b).

### 4a. The candidate picker is generalized to (runner × profile)

`docs/superpowers/specs/2026-07-02-ambiguous-runner-candidate-picker-design.md`
lets the client pick **which runner** when ≥2 runners tie on longest-prefix
roots score. Today, the picker is *also the de-facto agent selector*: the
canonical reason to have ≥2 same-roots runners is running different agents on
`--hostname`-differentiated slots (a `claude` runner and a `codex` runner), so
"pick candidate A vs B" **is** "pick Claude vs Codex". Collapsing those two
runners into one multi-profile runner would make the resume non-ambiguous
(one candidate → auto-select), **removing that agent choice** — a regression in
the current `u`/`U` experience.

To preserve it, the picker's **candidate unit becomes a `(runner, profile)`
pair**, not a bare runner. The trigger changes from "≥2 runners" to "**≥2
`(runner, profile)` combos**".

```
open (interactive)  — no --agent given
  [1] roots / selector — candidate runners (longest-prefix)         (existing)
  [2] expand           — each candidate runner × its advertised profiles = combos   (this spec)
  [3] if ≥2 combos     — picker: user picks (runner, profile)        (generalized picker)
```

- **One multi-profile runner advertising `claude,codex`** → combos =
  `{(r,claude), (r,codex)}` → **2 combos → picker fires** → picking a row picks
  the agent. This reproduces today's `u`/`U` experience even though it is now a
  single runner. **No regression.**
- **`--agent <name>` given** → the combo set is pre-filtered to that profile, so
  the picker only disambiguates residual runner ties (or auto-selects one).
- Genuine multi-runner ties (different hosts, load-split slots) still surface,
  now with the profile shown per row.

Wire/UI impact on the picker design: the `RunnerCandidate` record gains a
`profile` field (which profile this candidate row represents), and selection
re-issues pinned to **both** the chosen `cid` **and** the chosen `agent_profile`.
The picker rows render `host · agent · matched_root · cid`.

Scope boundary preserved: the picker stays **OpenInteractive-only**. On the
oneshot (`Submit`) path a profile that no runner serves is the flat
`SubmitStatus.profile_unavailable` (no picker), and an ambiguous oneshot still
returns `ambiguous_runner` as a status string — the oneshot path is not given a
picker, exactly as the picker spec scopes it.

### 4b. Resume, worktree reuse, and profile switching

Reopening a worktree under a **different** agent (e.g. "the directory I had open
in Claude, reopen in Codex to work on it") is a **first-class, common use
case**, not an edge case. The design supports it by keeping two independent axes
separate (see `tui/app.go` r/R/u/U and
`SubmitRequest`/`OpenInteractiveRequest.resume_conversation`):

- **Worktree/task reuse** (`resume_task_id`): re-queues the same TaskID, reusing
  the `harness/<id>` branch, worktree dir, and working state. **Agent-agnostic.**
- **Conversation continuation** (`resume_conversation` / `--continue`):
  **profile-relative.** Each coding agent maintains its *own* conversation
  history, keyed by the working directory (Claude:
  `~/.claude/projects/<cwd-hash>/`; Codex/Gemini: their own stores). Reusing the
  worktree lines up the cwd, so `resume_conversation=true` maps to the
  **selected profile's** resume argv and continues **that agent's own** thread
  for this worktree. It never asks one agent to read another's session.

Because conversation state is per-`(agent, worktree)`, the two agents' threads
**coexist independently** on the same worktree. There is therefore **no
cross-agent conflict**: any profile may be selected on resume, with or without
conversation continuation.

**Profile resolution rules** (the profile axis is orthogonal to the
conversation and runner-pin axes of `r`/`R`/`u`/`U`):

| Case | Requested profile |
|------|-------------------|
| Fresh create (`resume_task_id` zero) | client `--agent`, else runner default |
| Resume, **pinned** (`r`/`R`) | client `--agent` if given, else the resumed task's `TaskInfo.agent_profile`. No picker — one keypress. |
| Resume, **unpinned** (`u`/`U`) | client `--agent` if given; **else unresolved → the `(runner, profile)` picker (§4a) supplies both**, so the agent choice happens here |

`resume_conversation` is resolved **against the selected profile** (its
`resumeOneshotArgv` / `resumeInteractiveArgv`), independently of which profile
the task last ran under.

- `r`/`R` (pinned): runner = `AssignedTo`, profile defaults to the task's own
  `agent_profile`. Same runner, same agent, one keypress — **non-regressive**.
- `u`/`U` (unpinned): the picker enumerates `(runner, profile)` combos (§4a).
  With one multi-profile runner this shows `{claude, codex}`, so **`u`/`U`
  remains the agent selector it is today** — the choice is not defaulted away.
  `U` (fresh) is the clean "reopen this worktree under a different agent" path;
  `u` (continue) reopens the *selected* agent's own thread for the worktree.

**First continuation under a not-yet-used profile:** if you continue
(`resume_conversation=true`) under a profile that has never run in this worktree,
that agent finds no prior conversation for the cwd and behaves per its own CLI
(starts fresh, or errors). The harness does not police this — it passes the
selected profile's resume argv and lets the agent handle "no prior
conversation." The common agent-switch flow (`U`, fresh) sidesteps it entirely.

### 4c. Task identity vs per-open attributes — mode (Kind) is per-open too

Once agent profile is a per-open property, the same logic exposes that **task
`Kind` (oneshot vs interactive) is also incidentally, not essentially,
creation-immutable.** A task is really a *worktree/branch/conversation
identity* (`resume_task_id` → `harness/<id>` branch, worktree dir, cwd-keyed
conversation store, caps). **Each open picks `(mode, profile, runner)` — all
three are per-open, latest-recorded, and the store is mode-agnostic.**

**Evidence the Kind lock is incidental (shallow):**

- `WorktreeManager.Create(taskID)` (`runner/worktree.go:56`) keys the worktree
  purely on `taskID` (`dir = <repo>/.harness-worktrees/<taskID>`, branch
  `harness/<taskID>`); **zero Kind dependence.** Cross-mode reuse reattaches the
  same branch.
- `TaskStore.Resume` (`server/taskstore.go:223`) never reads or changes `Kind`.
- The mode lock is only two handler guards: `handleSubmitResume`
  (`server/task_handler.go:554`, requires `Kind==Oneshot`) and the resume branch
  of `handleOpenInteractive` (`:639`, requires `Kind==Interactive`).
- The scheduler dispatches any `Queued` task irrespective of Kind
  (`server/scheduler.go:75`); the *mode a process runs in* is set by **which
  message the runner receives** — `AssignTask` (→ `claude -p`, headless) vs
  `OpenExec` (→ PTY claude) — which the invoked handler path chooses, not by the
  `Kind` field.

**What stays essential (unchanged, enforced structurally):**

- One process is headless **xor** PTY. Still decided per-open by the dispatch
  message; nothing here changes that.
- Interactive cannot queue (it needs a live attached client at open time):
  `handleOpenInteractive` keeps its synchronous bind + `RunnerBusy` fail-fast.
  A oneshot-created task reopened interactively goes through that path and gets
  the synchronous treatment; an interactive-created task reopened via `submit`
  goes `Queued` → scheduler → `claude -p`. Both correct.

**Change to allow mode switching on reopen:**

1. Relax the two guards (`:554`, `:639`) from `kind != X → ResumeNotFound` to an
   existence/terminal check only (delegated to `TaskStore.Resume`). The
   requested mode is the one implied by the path invoked (`submit` = oneshot,
   `open_interactive` = interactive).
2. `TaskStore.Resume` gains a `newKind` parameter, sets `e.Kind = newKind`, and
   populates the **already-present** `WALEvent.Kind` field (no WAL format
   migration).
3. `ReplayEvents` `task_resumed` case **unconditionally** applies
   `t.Kind = protocol.TaskKind(ev.Kind)` (safe with `omitempty`, since
   `TaskKind_Oneshot == 0` and the field is always assigned).
4. `TaskInfo.kind` (already carried and displayed) now reflects the **latest
   open mode**, exactly like `agent_profile` (latest profile) and `AssignedTo`
   (latest runner).

This makes "run a headless oneshot in a worktree, then open it interactively to
inspect/continue" (and the reverse) a first-class flow, mirroring the
"reopen under a different agent" flow of §4b — same `resume_task_id` identity,
different per-open `(mode, profile)`.

### 5. Runner exec (`runner/session.go`)

`SessionConfig` currently carries a single `ClaudeBin` + the three argv
templates. Replace those single fields with a **profile set** plus a
**resolution step**: given the incoming `agent_profile` (from `AssignTaskBody` /
`OpenExecRunnerRequest`), look up the matching profile (empty → default) and use
its `bin` + argv templates in the existing `buildOneshotArgs` /
`buildInteractiveArgs` / `agentexec.ExecuteCommandWithOption` path. An unknown
name (should not happen because the server filtered) fails the task with a
clear error rather than silently falling back.

### 6. Operator surfaces (all three)

- **CLI** (`cmd/harness-cli`): `session new --agent <name>` on both create and
  the submit path; `--agent` is also honored on the resume path (`--resume
  <id> --agent codex` reopens under a different agent — §4b). `list` / task
  detail show the resolved profile (`cli/list.go` already renders `agent`;
  extend to show the profile set for runners and the resolved profile for
  tasks).
- **TUI** (`tui/*`): the compose / new-session flow gains an agent picker
  populated from the target runner's advertised `agent_profiles`. **Resume
  reopen**: agent selection is carried by the **generalized `(runner, profile)`
  candidate picker** (§4a), which is exactly the existing `u`/`U` surface — no
  new keybinding. `r`/`R` (pinned) stay one-keypress on the task's own agent;
  `u`/`U` (unpinned) open the picker, whose rows now read
  `host · agent · matched_root · cid`, so choosing a row chooses the agent (as
  it effectively does today with two runners). This directly preserves the
  current "u/U to pick claude-or-codex" experience. Runner/task rows reuse the
  existing agent-descriptor rendering, extended to the profile set.
- **WebUI** (`webui/`, wasm): the new-session form gains an agent dropdown fed
  by `agent_profiles`; the ambiguous-resume modal (the WebUI form of the
  candidate picker) lists `(runner, profile)` combos so the agent is chosen
  there. The WebUI already surfaces `agentBin`
  (`cmd/harness-webui-wasm/main.go`), extended to the set.

Per the repo rule "features span all three UIs", the selector ships on CLI,
TUI, and WebUI together.

## Error handling

- Unknown profile at submit time → `SubmitStatus.profile_unavailable` with a
  human-readable `error_msg` listing the names the matched runners advertise.
- Duplicate profile `name` at runner startup → runner refuses to start with a
  config error (fail fast, do not silently drop).
- Malformed `--agent-profiles` JSON or an argv template failing
  `Validate*ArgvTemplate` → runner startup error naming the offending profile.
- Resolved-but-unknown profile at runner exec time (server/runner disagree) →
  task fails with an explicit error; never a silent default fallback.

## Testing

- **Unit** — profile parsing/validation (dup name, bad JSON, missing
  `{prompt}` token); resolution (empty → default, named → match, unknown →
  error); dispatch filter (candidate restriction, `profile_unavailable`,
  ambiguity still triggers with two matching runners).
- **Wire round-trip** — encode/decode of the extended `RunnerHello`,
  `RunnerInfo`, `SubmitRequest`, `OpenInteractiveRequest`, `AssignTaskBody`,
  `OpenExecRunnerRequest`, `TaskInfo` (mirrors existing `*_test.go` patterns).
- **Integration** — one runner advertising `claude,codex`; submit a task with
  `--agent codex` and assert the codex bin/argv were used (existing integration
  tag-gated tests are the template); submit with no `--agent` and assert the
  default profile ran.
- **Cross-mode / cross-profile resume (§4c)** — unit: `TaskStore.Resume` with a
  `newKind` updates `Kind` and writes it to the WAL; `ReplayEvents` restores the
  switched Kind after a resume (create Interactive → resume as Oneshot → replay
  → Kind==Oneshot). Integration: submit a oneshot under `claude`, then
  `open_interactive --resume <id> --agent codex` and assert the same
  `harness/<id>` worktree is reused under a PTY codex process; assert the
  guards no longer reject the cross-Kind resume.
- **Verify (real drive)** — per the repo `verify` skill: stand up a single
  runner with two profiles, create one session per profile from the CLI, and
  observe each launches the correct binary (not just a passing unit test);
  additionally reopen a finished oneshot worktree as an interactive session
  under the other agent and confirm the worktree state carried over.

## Wiring inventory (every callsite to thread `agent_profile` / mode)

Per "features span all three UIs" and "enumerate all callsites when
intercepting a shared operation" — the full route map for session
create/resume, so no surface is missed (line numbers drift; verify by symbol):

**Wire construction (2 kinds, native + wasm parity):**

- `SubmitRequest` — `cli/submit.go` (`Submit*` funnel).
- `OpenInteractiveRequest` — `cli/open_interactive_native.go` **and**
  `cli/open_interactive_wasm.go` (separate build-tag implementations — both
  must be edited or WebUI silently diverges).

**`cli.Client` funnels — add an `agentProfile` param, set the wire field:**

- `SubmitWithSelectorArgsAndCaps` (`cli/submit.go`).
- `OpenInteractiveWithSelectorArgsAndCaps` → `openInteractive` (native + wasm).
- `OpenInteractiveX11` / `RunInteractiveX11` (`cli/x11.go`).
- The narrower `Interactive*` / `SubmitWithSelector*` wrappers delegate down —
  extend signatures or add an `...AndAgent` funnel; keep native/wasm in lockstep.

**UI callers (pass the chosen profile; add the selection affordance):**

- **CLI** — `cmd/harness-cli/main.go` (`submit`, `interactive`) and
  `cmd/harness-cli/session.go` (open-detachable, x11, interactive). Add an
  `--agent <name>` flag to these commands.
- **TUI** — `tui/client.go` (`DoSubmit` / `DoSubmitWithOpts`) and
  `tui/interactive.go` (`DoOpenDetachableSession`, `DoResumeSession`, and their
  callsites). The Do\* helpers gain a profile param; the compose/new-session
  flow and the `(runner, profile)` resume picker (§4a) supply it.
- **WebUI** — wasm bridge `harnessSubmit` / `harnessStartInteractive` in
  `cmd/harness-webui-wasm/main.go`, exported as `harness.submit` /
  `harness.startInteractive`; JS `webui/static/main.js` (new-session form +
  resume modal). Runner `agentBin` is already surfaced to JS
  (`main.go` list mapping) — extend it to the `agent_profiles` set for the
  dropdown / picker.

Mode (§4c) rides the same callers: the CLI/TUI/WebUI action invoked (submit vs
open-interactive) already selects the mode; no extra field is needed on the
client — only the relaxed server-side guards.

## Tradeoffs

**Functional**

- One runner serves every configured runtime; process/management count drops to
  one per host. The `claude`-vs-`codex` `AmbiguousRunner` case is eliminated
  (single runner, no roots collision).
- Cost: a genuinely new wire surface (7 formats touched) and selector plumbing
  through three UIs.

**Security**

- Profiles are **runner-defined**; a client only names one. Clients cannot
  inject an arbitrary binary or argv (the rejected Approach B would have allowed
  that). The trust surface is unchanged from today — the runner still decides
  what it will exec.
- The server filters by advertised name, so a task cannot be assigned to a
  runner lacking the runtime; failures are explicit at submit, not at exec.

**Non-functional**

- Keeping the existing single-agent flags as "the default profile" preserves
  every current `runner.sh up` invocation and script.
- Keeping concrete per-agent argv in the `runner.py` preset (not Go) avoids
  coupling the protocol layer to specific third-party CLIs; adding a new agent
  is a preset edit, not a Go/protocol change.
- Internal names such as `ClaudeBin` remain in places for now; renaming to a
  profile-centric vocabulary is incremental and not required for this change.

## Migration / back-compat

- Existing runners: unchanged flags → one profile named after `--agent-bin`;
  existing clients that send no `agent_profile` → default profile. No operator
  action required.
- New capability is additive: `runner.sh up --agents claude,codex` to opt in;
  `session new --agent codex` to select.
- Wire: server and runner are rebuilt/redeployed together; no dual-version
  window is supported (consistent with prior wire bumps in this repo).
