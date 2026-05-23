---
name: implementation-pitfalls
description: Use BEFORE dispatching implementer/reviewer subagents (superpowers:subagent-driven-development) or BEFORE extending a layer in this repo. Catalog of concrete past failure modes specific to this project — subagent dispatch checklists + sibling-code grep obligations + spec problem-statement enforcement.
---

# Implementation pitfalls (project-local failure catalog)

Concrete past misses on this project. Each entry has:
- **What went wrong** — the specific incident with commit refs
- **Why it slipped review** — the mechanism that let it through
- **Mitigation** — what to insert into implementer / reviewer prompts to catch it next time

These are NOT generic best practices — they are this-project trap-pattern documentation derived from incidents on `main`. Read this BEFORE you start dispatching subagents or writing a fresh implementation. They are the most cost-effective additions to your subagent prompts.

---

## Pitfall 1 — Spec scope contraction (problem statement ≠ implementation section)

**What went wrong**: The Phase B spec (`docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md`) said in the **Problem** section that ALL `harness-cli` subcommands on a runner host suffer from ACL outbound block. But the **Implementation** section was titled "Phase B: Agent leg = objproto negotiated proxy" — and the implementation shipped covering only the `agent` subcommand. User caught it months later: `harness-cli ls` from inside a runner-spawned process silently failed because cli.Dial didn't check the proxy env.

Fixed in commit `2365260` (moved proxy detection into `cli.DialPeerConn`, shared by every subcommand).

**Why it slipped review**: The reviewer subagent compared the diff to the Implementation section only. Spec compliance was technically PASS. The Problem section was never re-read during review.

**Mitigation — insert into IMPLEMENTER prompts:**

> Step 0: read the spec's **Problem statement** verbatim (not the Implementation section alone). In your final report, enumerate the problem-statement bullets and state which ones the diff addresses. If any bullet is left unaddressed, justify the omission explicitly.

**Mitigation — insert into REVIEWER prompts:**

> Read BOTH the spec's Problem statement AND the Implementation section. Flag any problem-statement bullet not covered by the diff. "Spec compliance" review against only the Implementation section misses scope contractions that happen in the spec itself.

---

## Pitfall 2 — Invented labels harden into unwritten constraints

**What went wrong**: The Phase B spec used the label "Agent leg" (which I invented, undefined). The label semantically narrowed the scope from "all harness-cli on runner host" to "agent subcommand", but the narrowing was never written down — the label just felt narrower than the actual problem. User: "agent leg という言葉がそもそも定義がよくわからない".

Same incident as Pitfall 1, same fix (`2365260`).

**Why it slipped review**: A vague label feels rigorous. Reviewers and implementers treat it as authoritative without checking what it actually denotes.

**Mitigation — when writing specs:**

- Describe the concrete mechanism in plain terms, NOT a label
- If a label is unavoidable, state IN/OUT scope explicitly immediately after introducing it
- Beware labels like "X leg", "Y side", "Z axis", "design intent", "state precondition" — these are tells

**Tells of the same pattern** (`feedback_jargon_masks_confusion` in memory): if you find yourself constructing terminology to describe what code SHOULD do, you probably don't fully understand the mechanism yet. Read the code first.

---

## Pitfall 3 — Sibling-code grep skipped, wrong pattern copied

**What went wrong**: When adding the `--via` flag for `server dial-runner` across CLI/TUI/WebUI, the implementer copied the high-level `cli.ServerDialRunner(ctx, server, target, via)` pattern (which does Dial + defer Close — appropriate for the short-lived `harness-cli` binary) into TUI and WebUI. But TUI and WebUI already hold a long-lived `*cli.Client` from snapshot polling; they should have used `cli.ServerDialRunnerWith(ctx, c, ...)` instead. Every `server dial-runner` from TUI/WebUI was opening a fresh WS conn + ECDH + PSK + Hello, throwing away the existing client.

Fixed in `c55ed45` (TUI/WebUI use the `*With` variant on `a.client` / `currentClient()`).

**Why it slipped review**: Implementer's view was confined to the cli helper. The reviewer compared against the spec which said "extend cli.ServerDialRunner" — also literally satisfied. Neither asked "how does DoSubmit / DoCancel call into cli? Should this new caller follow the same pattern?".

**Mitigation — insert into IMPLEMENTER prompts when adding code to TUI/WebUI/server handlers:**

> Step 0: grep how sibling code in this layer calls into the same helper category. Specifically for TUI: every existing Do* function (DoSubmit, DoCancel, DoInteractive, DoSessionList, ...) threads `a.client` through. Your new DoX MUST follow the same pattern, NOT the harness-cli binary's Dial+Close pattern.

**Mitigation — insert into REVIEWER prompts:**

> Does this diff use the same helper-invocation pattern as adjacent code in the same layer? Spec compliance alone is not enough — check layer-internal consistency.

---

## Pitfall 4 — Build-output / runtime-state collision in `make clean`

**What went wrong**: `make clean` was `rm -rf bin`. `bin/.run/` is the runtime state directory for `scripts/runner.sh` (slot pid / log / shutdown sentinel files). Live agent-runner processes kept their `--shutdown-file` path open across the delete; subsequent restart attempts via `scripts/build_and_restart_all.py` reported "no alive agent-runner slots under bin/.run/" because the orchestrator could no longer discover them.

Fixed in commit `fe62101` — `make clean` now removes only the four specific binary targets, preserving `bin/.run/`.

**Why it slipped**: `bin/.run/` was added to the project after `make clean` was written. No one re-audited the clean target.

**Mitigation — when adding state under an existing build-output directory:**

Audit `make clean` / `git clean` / build scripts for the new path. Better: don't co-locate runtime state with build output (this project should eventually move `bin/.run/` to `.run/` at repo root, but kept as-is to avoid a coordinated restart of all live runners).

---

## Pitfall 5 — `peer.Conn.Close()` sends wire-level Close, breaks relay setup

**What went wrong**: `runner/relay_handler.go`'s `completeRelaySetup` initially called `pc.Close()` after `SetProxy`. `peer.Conn.Close()` sends a `trsf.Close` wire message before tearing down — but that wire Close reached the SERVER's end-to-end relay conn (which shares the same CID through SetProxy) and caused `handleConnection` on the server to exit EOF prematurely, before the target runner's `RunnerHello` arrived.

Fixed in commit `101128d` — use `pc.Connection().Close()` (raw objproto-level close, no trsf Close message). The proxySettings entry survives, the activeConn entry is removed, packets keep being forwarded.

**Why it slipped**: Discovered only by the E2E integration test (`TestRelayE2E`). Unit tests didn't catch it.

**Mitigation — when working with `peer.Conn` and `objproto.Connection`:**

`peer.Conn.Close()` ≠ `pc.Connection().Close()`. The former sends a trsf.Close wire message (semantically "I'm leaving"); the latter tears down only the local activeConn state. For SetProxy-style relay setups, you almost always want the latter — sending wire Close would propagate through the proxy to peers you didn't mean to notify.

`peer.Conn.Close()` also has a load-bearing 50ms sleep for Windows scheduling (`project_peer_close_send_drain` in memory) — that's the other half of the same close-behavior surface.

---

## Pitfall 6 — Listen bind addr ≠ dial target addr

**What went wrong**: `HARNESS_PROXY_VIA_RUNNER` was set to literal `cfg.WSListen` (e.g. `"ws:0.0.0.0:8540-*"`). Agents on the same host tried to dial `0.0.0.0:8540` — accepted by Linux kernel as localhost, rejected by Windows / macOS. With `--listen :8540` (host empty) the agent got `ws::8540-*` which fails DNS lookup on every OS.

Fixed in commit `d26aa8d` — `rewriteProxyViaForLocalDial` normalizes the host portion to loopback when it's an unspecified bind address.

**Why it slipped**: Tested only on Linux. The cross-OS issue was theoretical until somebody actually ran it on Windows / macOS — at which point it's a confusing late failure.

**Mitigation — when generating addr strings for cross-host / cross-OS use:**

Distinguish bind addr (server-side, "listen on every interface" = 0.0.0.0) from dial addr (client-side, "where to reach this peer" = 127.0.0.1 / hostname / specific IP). Never use the bind addr as a dial target. If you're auto-deriving one from the other, write a rewrite step explicitly.

---

## Subagent dispatch checklist (controller-side)

When dispatching an implementer or reviewer subagent in this project, include in the prompt:

### Implementer prompt augmentations
- [ ] Quote the spec's **Problem statement** verbatim. Report which bullets the diff addresses; justify omissions.
- [ ] **Sibling-code grep**: before adding code to TUI/WebUI/server handlers, grep how adjacent files in the same layer invoke the same helper category. Match the existing pattern.
- [ ] Beware `peer.Conn.Close()` vs `pc.Connection().Close()` when working with relays.

### Reviewer prompt augmentations
- [ ] Read BOTH the Problem statement AND the Implementation section. Flag uncovered problem-statement bullets.
- [ ] Check layer-internal consistency: does this diff's caller pattern match adjacent code in the same layer?
- [ ] Check for silent fallback paths: if a config / env is set, does any combination cause it to be ignored without log?

---

## When to invoke this skill

- BEFORE running `superpowers:subagent-driven-development` for any task on this project
- BEFORE dispatching a fresh implementer or reviewer subagent
- BEFORE writing new code that touches: `cli.Dial*`, `peer.Conn.Close`, `transport.WebSocketEndpoint` mode handling, env injection for spawned processes, `HARNESS_PROXY_VIA_RUNNER`, `make clean`, or any helper that has `*With(client)` long-lived and `X(serverCID)` short-lived variants

---

# Project principles (from user-level memory)

These are user-driven feedback rules from past sessions, baked into project-local form so subagents on this repo see them. Each one was triggered by a specific past incident; the one-liner is enough to remember the rule, expand by reading the named memory file if needed.

## Scope / sizing
- **Individual dogfood scope** (`feedback_individual_dogfood`): No external users. Don't inflate breaking-change / migration concerns. Renames, subcommand splits, schema field additions are quality fixes — just do them. Don't add migration shims, deprecation periods, or backward-compat layers "for users" that don't exist.
- **Don't split schema/spec across plan tasks** (`feedback_no_split_schemas`): When a plan adds new wire types or config fields, put the FULL schema/interface in ONE plan task. No "also add this in Task N" follow-ups — the schema becomes harder to review and easier to diverge against. Same for spec docs: keep the authoritative byte layout in one place.

## Protocol / schema discipline
- **Schema describes every byte on the wire** (`feedback_no_schema_invisible_bytes`): The `.bgn` files are the single source of truth. Any byte sent on the wire MUST be described in the schema. "Convention puts this byte here" is worse than "schema doesn't mention it" — schema becomes a lie.
- **Protocol explicit over convention** (`feedback_protocol_explicit_over_convention`): If a field is needed, extend the schema. Don't write code that "by convention puts X in this position of the payload". LLMs lose convention context across sessions; explicit schema fields survive.

## Reading before writing
- **Read harness code before architectural speculation** (`feedback_read_code_before_arch_speculation`): Don't reason from 1-line memory summaries or vague impressions. When proposing a structural change, first check README + relevant `main.go` + handler implementation. Memories can drift; code is authoritative.
- **Jargon masks confusion** (`feedback_jargon_masks_confusion`): When unsure of how a layer behaves, the next step is reading the schema / handler. Not constructing "design intent" / "state precondition" / "[X]-axis" prose. Treat invented framing words as tells that you don't fully understand yet. (See also Pitfall 2 above for the spec-writing variant.)

## Verification discipline
- **Verify fix in symptom env BEFORE writing memory** (`feedback_verify_before_memory_writes`): Hypotheses codified as memory mislead future sessions when wrong. Push the code fix freely, but hold the explanatory memory until you've actually seen the original symptom go away with your change applied. This catalog file is bound by the same rule.

## Harness / runtime
- **Harness worktree paths route to parent repo** (`feedback_worktree_path_routing`): Inside a harness task, `/home/.../remote-agent-harness/<rel>` resolves to the parent's main checkout, not the per-task worktree. Anchor edits explicitly to the worktree when needed; bash commands `cd <abs path>` land on the parent.
- **Spawn runners via `scripts/runner.sh up --as TAG`** (`feedback_use_runner_scripts`): Never hand-roll `nohup setsid bin/agent-runner ...`. The script handles slot allocation, pid/log files, detach, restart. Bypassing it leaks state under `bin/.run/`.
- **Bundle related repos via `--roots`** (`feedback_runner_roots_bundle`): "Add repo B to runner A" = extend A's `--roots`, not a new `--as` slot per repo. One runner can serve multiple roots.
- **harness-cli never sync wait/dispatch from agent turn** (`feedback_no_sync_wait_dispatch`): `send` only and end the turn. Replies arrive via inbox hook on a later turn. Applies to the initial `harness.hello` handshake too. Synchronous wait stalls the entire conversation.

## Naming / labels
- **Invented labels in specs become implicit constraints** (`feedback_invented_terms_become_implicit_constraints` — see also Pitfall 1 & 2): Vague labels in specs (e.g. "Agent leg", "X-axis", "design intent") feel rigorous but harden into narrow implementations. Describe the mechanism in plain terms. If a label is unavoidable, state IN/OUT scope explicitly right after introducing it.

## Layer consistency
- **Reuse long-lived `*cli.Client` in TUI/WebUI** (`feedback_reuse_long_lived_client` — see also Pitfall 3): When adding a new helper, expose both `X(serverCID)` (short-lived: dial+close) and `XWith(client)` (reuse). TUI/WebUI MUST call the `*With` variant against their existing `a.client` / `currentClient()` — never the high-level fresh-dial form.
- **Check existing patterns before extending a layer** (`feedback_check_existing_patterns_before_extend` — see also Pitfall 3): Before adding a new TUI / WebUI / server-handler entry, grep how sibling code in the same layer invokes the same helper category. Subagents copy what they see first (often a test pattern or CLI pattern) which is usually wrong for production. Controller must show them the forest.

---

# Sources

- Project-local incidents (Pitfalls section) — commit refs on `main`
- User-level memory (`~/.claude/projects/.../memory/feedback_*.md`) — accumulated across sessions, mirrored above
