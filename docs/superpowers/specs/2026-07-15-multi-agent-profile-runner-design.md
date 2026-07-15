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
- No change to the resume/`--continue` semantics beyond routing them through
  the selected profile's `resumeOneshotArgv` / `resumeInteractiveArgv`.

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
- **TaskInfo** — the resolved profile the task ran under, for display on task
  detail:
  ```
  agent_profile_len :u8
  agent_profile :[agent_profile_len]u8
  ```

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

### 4a. Relationship to the AmbiguousRunner candidate picker

`docs/superpowers/specs/2026-07-02-ambiguous-runner-candidate-picker-design.md`
lets the client pick **which runner** (by cid / hostname / matched-root) when
≥2 runners tie on longest-prefix roots score. That picker has **no agent
axis** — it never selected an agent, so nothing in it is literally replaced.

The two designs **compose as an ordered pipeline**, they do not overlap:

```
submit / open  (--agent codex?)
  [1] profile filter   — keep only runners advertising the requested profile   (this spec)
  [2] roots / selector — longest-prefix candidate set                          (existing)
  [3] if ≥2 candidates — candidate picker (OpenInteractive only)               (picker spec)
```

The profile filter is the **front stage**: the picker enumerates the
**already-profile-filtered** candidate set. Practical effects:

- The picker's *primary motivating scenario* (a second `--hostname`-only runner
  on the same roots, spawned solely to run a different agent) **stops
  occurring**, because that reason to run two same-roots runners is gone. The
  agent choice that was previously smuggled into "pick runner A (claude) vs
  runner B (codex)" becomes the explicit `--agent` selector.
- The picker is **not** obsoleted: genuine ties remain (different hosts serving
  the same repo, load-split slots, failover replicas). Two multi-profile
  runners on the same roots that both advertise `codex` still tie under
  `--agent codex` → the picker fires as before, just over a profile-filtered
  set.

Scope boundary preserved: the picker stays **OpenInteractive-only**. The
profile filter applies to **both** submit and open, but a oneshot profile miss
is the flat `SubmitStatus.profile_unavailable` (no picker), exactly as the
picker spec keeps the oneshot path status-string-only. No picker code is
reworked to be "profile-aware" — it simply receives fewer candidates.

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

- **CLI** (`cmd/harness-cli`): `session new --agent <name>` and the submit path
  gain `--agent <name>`. `list` / task detail show the resolved profile
  (`cli/list.go` already renders `agent`; extend to show the profile set for
  runners and the resolved profile for tasks).
- **TUI** (`tui/*`): the compose / new-session flow gains an agent picker
  populated from the target runner's advertised `agent_profiles`; runner and
  task rows reuse the existing agent-descriptor rendering, extended to the set.
- **WebUI** (`webui/`, wasm): the new-session form gains an agent dropdown fed
  by `agent_profiles`; the WebUI already surfaces `agentBin`
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
- **Verify (real drive)** — per the repo `verify` skill: stand up a single
  runner with two profiles, create one session per profile from the CLI, and
  observe each launches the correct binary (not just a passing unit test).

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
