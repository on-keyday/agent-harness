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

## Pitfall 7 — Specs that punt decisions to "implementer's choice"

**What went wrong**: While writing the chained-relay spec, I left four design points unresolved with phrases like "implementer's choice", "or kept as dead code temporarily", "could be deferred to a separate change if scope creeps", "Implementer must decide between (a) ... (b) ... (c) ...". User caught it: "implementer decision的なところが残るのは何でなん?".

Fixed in commit `3c99b39` — each punt converted to an explicit decision in the spec body. Open questions section renamed to "Decisions taken".

**Why it slipped**: When the spec author is unsure between two paths, deferring "to the implementer" feels collaborative. It isn't. The implementer doesn't know which to pick either, will either freeze on it or default to the easiest path without recording rationale (cf. Pitfall 1, scope contraction). A "could be deferred" line in a spec means: the spec is incomplete and pretending to be complete.

**Mitigation — when writing specs:**

If you find yourself writing any of these phrases, STOP and decide before committing:

- "implementer's choice"
- "could be deferred to a separate change"
- "remove or deprecate" (pick one)
- "or kept as dead code temporarily"
- "Implementer must decide between (a)... (b)... (c)..."
- "TBD", "TODO" inside the spec body (open questions section is fine, but they should be USER decisions, not implementer's)

For each, ask: "What would I do if I were implementing this right now?" Write that as the decision. If the spec genuinely needs the user's input on a tradeoff, mark it as an explicit open question for the USER, not the implementer.

**Difference from a legitimate open question**:

- Legitimate open question = user must make a tradeoff call (e.g. "do we want SSO support in v1 or v2?"). Spec waits for user.
- Implementer-punted decision = author doesn't want to pick (e.g. "remove or keep as dead code"). Both options are author-decidable; user shouldn't have to.

---

## Pitfall 8 — Harness worktree silently diverges from parent's branch

**What went wrong (2026-05-24)**: All 21 commits of chained-relay spec / plan / code work landed on the parent repo's `feature/chained-relay-spec` branch, while the harness task worktree's actual branch (`harness/<hash>`) stayed at its old HEAD. User caught it when `ls .claude/skills` from the worktree showed only `harness-cli` (missing `implementation-pitfalls`), while `ls` from the parent repo showed both. By then the worktree's branch was 148 commits behind the work the controller "thought" it had done. Controller had also incorrectly claimed "the 2 worktree-unique commits are already on origin/main" based on commit-message matching alone — content diff showed they were different code.

Root mechanism:
- Harness spawns the task with cwd = `/home/.../remote-agent-harness/.harness-worktrees/<hash>/` on branch `harness/<hash>`.
- Edit / Write tools called with absolute paths under `/home/.../remote-agent-harness/<rel>` resolve to the PARENT repo's main checkout — not the worktree (`feedback_worktree_path_routing`).
- `git -C /home/.../remote-agent-harness <op>` similarly operates on the parent's `.git` and the parent's currently-checked-out branch.
- Result: controller does `Edit ... && git add ... && git commit ...` from the worktree session, but everything actually lands on the parent. Worktree stays frozen. Controller's `git log` (which also routes to parent) reports the new commits, so the divergence is invisible until someone `ls` from the worktree side.

**Mitigation — when controlling a harness task on this repo**:

1. **Decide upfront where work lands**, and stick to it for the entire task:
   - **Parent repo** (default): explicitly tell every subagent to operate in `/home/kforfk/workspace/remote-agent-harness/` and verify branch. See "Subagent dispatch checklist" below.
   - **Fresh non-harness worktree**: user creates a worktree outside `.harness-worktrees/` (e.g. `/home/kforfk/workspace/remote-agent-harness-impl/`) and you operate there. Avoids routing entirely.
   - **The harness worktree itself**: only OK for read-only work (verifying state, running tests). Do NOT commit from inside — commits route to parent unpredictably.

2. **Verify before claiming "commits are on the right branch"**:
   - `git -C <path> log --oneline -5` to confirm
   - `git worktree list` to map directories ↔ branches
   - Never assume two same-name commits across branches are the same content — `git diff <sha1> <sha2>` or `git cherry` is the only way to know.

3. **When the user asks "is the branch up to date with origin/main?"**, run `git fetch && git log origin/main..HEAD && git log HEAD..origin/main` AND `git worktree list` AND `git -C <worktree> rev-parse --abbrev-ref HEAD`. The single-source-of-truth answer is the worktree HEAD's actual branch's relationship to origin/main, NOT the parent repo's checkout state.

---

## Pitfall 9 — Operator-surface skew: CLI-only / TUI-only / WebUI-only changes

**What went wrong (2026-07-03)**: Resume-conversation and runner-selection work was repeatedly checked through only one operator surface at a time. The CLI had explicit `--resume-conversation` flags in `submit`, `interactive`, and `session new`; the TUI task-list keybindings had equivalent behavior via `r`/`u` versus `R`/`U`; WebUI task-sheet actions later gained assigned-runner and any-runner resume variants. But the separate command-entry surfaces were not checked at the same time:

- TUI has a bottom command line (`tui/cmdline.go`) distinct from its task-list keybindings. It parses `--resume` for `submit`, `interactive`, and `session new`, but does not parse `--resume-conversation`.
- WebUI has a command input (`webui/static/main.js` `runCmd`) distinct from its task-sheet buttons. Its `submit` command uses the Resume task id field but does not set `resumeConversation`.

So "TUI supports resume conversation" was true for task-list keys but false for the TUI command-line route. "WebUI supports resume conversation" was true for task-sheet buttons and `resumeTaskById`, but false for the WebUI command input. These are different user-visible entry points and must not be collapsed into one mental bucket.

**Why it slipped review**: The controller asked "does TUI have it?" and answered from the most visible TUI path. It did not enumerate every operator entry point. The same happened for WebUI: task-sheet buttons were treated as the whole WebUI, while the command input was a separate parser with different request construction.

**Mitigation — before implementing or reviewing any operator-visible flag/option:**

Build an explicit surface matrix and fill every cell before claiming coverage:

| Surface | Check |
| --- | --- |
| CLI binary | `cmd/harness-cli/main.go`, `cmd/harness-cli/session.go`, help text, CLI tests |
| TUI keybindings | `tui/app.go`, action helpers, keybinding tests |
| TUI command line | `tui/cmdline.go`, `tui/cmdline_test.go`, `runAction` wiring |
| TUI popups/forms | `tui/popup.go` and submit/session popup state |
| WebUI forms/buttons | `webui/static/main.js` direct event handlers |
| WebUI command input | `webui/static/main.js` `runCmd`, help text, tokenizer/flag parser |
| WASM bridge | `cmd/harness-webui-wasm/main.go` option parsing and request construction |
| Shared client/protocol | `cli/*`, `server/task_handler.go`, `runner/session.go`, protocol round-trip tests |

If a feature intentionally omits a surface, document the omission in the diff or spec. "No parser there" is not an implicit excuse; the absence itself is a design decision.

**Mitigation — insert into IMPLEMENTER prompts for operator-facing features:**

> Before editing, enumerate CLI, TUI keybindings, TUI cmdline, TUI popups, WebUI buttons/forms, WebUI cmdline, WASM bridge, shared cli/server/runner handling. For each, say "implemented", "not applicable", or "intentionally omitted because ...". Do not report "TUI/WebUI done" unless both their direct controls and command-entry surfaces were checked.

**Mitigation — insert into REVIEWER prompts:**

> Audit operator-surface parity. For every new flag/option/request bit, verify CLI, TUI keybindings, TUI cmdline, WebUI buttons/forms, WebUI cmdline, and WASM bridge separately. Flag any surface where the parser accepts a related option like `--resume` but drops the companion option like `--resume-conversation`.

---

## Pitfall 10 — A `.bgn` wire change wipes the runner fleet (server-first, and prove the skew is recoverable)

**What went wrong (2026-07-16)**: landing appended fields on `RunnerHello`
(`agent_profiles`) while the server — which runs on a DIFFERENT host — still ran
the old binary. The old server could not decode the new hello, found no identity
union, and answered `PskAuthStatus.no_identity`. That was classified FATAL, so
`PersistLoop` exited instead of retrying: **all 12 runner slots died within ~1s
and none returned when the server was upgraded.** Each needed manual recovery
(flags recovered from `bin/.run/agent-runner-<tag>.restart.log`).

The asymmetry that makes this easy to trip: `make build` + `/restart-all` on the
runner host upgrades ONLY the runners. The server is a separate host and stays
old unless you restart it too — **restart the SERVER first.**

Fixed in `d4f7a5a` + follow-up: `no_identity` is now RETRYABLE
(`cli.PskRejectedError.Retryable`) — BadPsk/BadTicket stay fatal (credential
failures no retry can fix), a version skew now costs a reconnect, not a wipe.
That only helps once BOTH ends carry it, so the restart order still matters.

**Why it slipped**: the fatal/retryable line was drawn at "explicit rejection vs
transport drop", and `no_identity` was swept into the fatal set on an unexamined
assumption — `cli/psk.go` literally said *"should not happen: we always embed a
hello"*. True for our own bugs; false for a version-skewed server that cannot
DECODE a hello we DID send.

**Mitigation — before landing any `.bgn` change:**

```
scripts/wire-skew-check.sh          # OLD_REF defaults to merge-base with origin/main
```

It builds both sides, runs NEW runner × OLD server, and asserts the failure is
RECOVERABLE (rejected → retries → self-heals when the server is upgraded). It is
a no-op (exit 0) when no `.bgn` changed, so it is safe to run unconditionally.
Note what it deliberately does NOT assert: that skew *works*. This project has no
compat shims by design; asserting "works" would push us toward shims we do not
want.

**Two sub-lessons, both learned the hard way while building that check:**

1. **A guard that cannot fail is worse than none.** The check's first version
   passed on everything: a fresh `git worktree add` has no
   `webui/static/main.wasm` (gitignored build artifact) and `harness-server`
   refuses to start without it, so the runner only ever saw "connection refused"
   and "it retried" was trivially true. It now proves the skew was *exercised*
   (a real rejection reached the runner) before it may report PASS — and a
   non-starting old server is a setup error (exit 2), never a pass. **Always run
   the negative control: make the fix absent and confirm the check goes red.**
2. **Don't hand-build the same struct at N sites — give it a constructor.**
   `PskRejectedError` was built raw at three sites; the fix added a `Code` field
   and a `grep` scoped to `cli/` missed the third — the runner has its OWN
   handshake (`sendRunnerMergedHandshake` in `runner/connect.go`, because it
   sends a `RunnerHello` not a `ClientHello`). The zero `Code` read as
   `PskAuthStatus_Ok`, `Retryable()` returned false, and the fatal behaviour was
   silently restored. Unit tests could not catch it: they hand-built the struct
   too, so they never exercised a creation site. Now there is one field and one
   `cli.NewPskRejectedError(status)`. **When adding a field to a struct built in
   several places, add a constructor (or delete the derived field) rather than
   trusting a grep** — and grep the whole repo, not the package you expect.
   See also Pitfall 3 and `feedback_enumerate_all_callsites_when_intercepting`.

---

## Subagent dispatch checklist (controller-side)

When dispatching an implementer or reviewer subagent in this project, include in the prompt:

### Hard preconditions (every subagent prompt — non-optional)
- [ ] **Parent repo, not harness worktree**: `Work in /home/kforfk/workspace/remote-agent-harness/, NOT in any .harness-worktrees/<hash>/ directory. Verify branch with git rev-parse --abbrev-ref HEAD before writing code. Use absolute paths under the parent repo for all tool calls.` Past incident: 148 commits diverged silently because writes via absolute path routed to parent while the worktree HEAD stayed on an unrelated `harness/<hash>` branch.
- [ ] **Read this file first**: `Read .claude/skills/implementation-pitfalls/SKILL.md in full before writing code.` The catalog is short; each entry was triggered by a prior incident. Subagents that skip it tend to recreate the same failure mode.

### Implementer prompt augmentations
- [ ] Quote the spec's **Problem statement** verbatim. Report which bullets the diff addresses; justify omissions.
- [ ] **Sibling-code grep**: before adding code to TUI/WebUI/server handlers, grep how adjacent files in the same layer invoke the same helper category. Match the existing pattern.
- [ ] **Operator-surface matrix**: for operator-visible flags/options, check CLI, TUI keybindings, TUI cmdline, TUI popups, WebUI buttons/forms, WebUI cmdline, WASM bridge, and shared cli/server/runner handling separately. Do not collapse "TUI" or "WebUI" into a single surface.
- [ ] Beware `peer.Conn.Close()` vs `pc.Connection().Close()` when working with relays.
- [ ] **Build hygiene — keep the worktree clean.** Compile-check with `go build ./...` (builds everything, writes NO binary) or `go build -o /dev/null ./cmd/<x>` / `go vet ./cmd/<x>`. NEVER bare `go build ./cmd/<x>/` — that drops a `<x>` executable into the worktree root. `go test ./...` cleans up after itself; don't `go test -c` without `-o /dev/null`. Stray `harness-tui` / `*.test` binaries pollute `git status`, get caught by `git add -A`, and leave junk in the user's worktree. The cwd must be exactly as clean after your checks as before.

### Reviewer prompt augmentations
- [ ] Read BOTH the Problem statement AND the Implementation section. Flag uncovered problem-statement bullets.
- [ ] Check layer-internal consistency: does this diff's caller pattern match adjacent code in the same layer?
- [ ] Check operator-surface parity: CLI, TUI keybindings, TUI cmdline, TUI popups, WebUI buttons/forms, WebUI cmdline, WASM bridge, and shared cli/server/runner code must either all support the feature or have explicit documented omissions.
- [ ] Check for silent fallback paths: if a config / env is set, does any combination cause it to be ignored without log?

### Spec-writing checklist (controller-side, before commiting a spec)
- [ ] Grep the draft for "implementer's choice", "could be deferred", "remove or", "TBD", "TODO". Decide each before committing.
- [ ] Grep for "Wait —", "hmm", "let me think" — these are author-uncertainty markers that don't belong in a spec body. Verify the underlying logic and rewrite confidently or mark as an explicit open question.
- [ ] Problem statement and Implementation section must cover the same scope. If the Implementation section silently narrows scope, the rationale must be written in the spec body (not just left as a label change).

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
