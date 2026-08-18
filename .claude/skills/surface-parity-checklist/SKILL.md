---
name: surface-parity-checklist
description: Use BEFORE adding, changing, or displaying any operator-visible task/runner field (caps, scope, status, agent, repo, …), adding an option to any verb family, or wiring a new operator action's result reporting — AND before adding/renaming an agent or changing its bin, argv templates, log format, credential mode, egress domains, or launch env. NUMBERED checklists that must be walked item by item with a verdict per number — never summarized: 1–36 for every input surface, display surface, per-path semantics axis and result/display convention across CLI/TUI/WebUI/wasm, plus S1–S6 for preset↔podman-sandbox agent-launch parity.
---

# Surface-parity checklist

Every operator-visible field lives on ~15 surfaces across three UIs, and
every option's MEANING exists once per path it is reachable from. History
shows these get found one at a time, by the user, after landing. A second,
narrower axis lives at the bottom (S1–S6): an agent the operator launches
is described in a preset table AND re-described in the podman sandbox
wrapper, and no UI grep reaches that pair. Sibling:
`implementation-pitfalls` Pitfall 9 is the incident record; this file is the
checklist.

## How to use — non-negotiable

This is a CHECKLIST, not reference prose. When this skill applies, walk
**every number from 1 to 36 in order** and record a verdict for each:

- `done` — implemented/verified, with the file touched
- `n/a` — genuinely not applicable, WITH the reason stated
- `omitted` — deliberately skipped, WITH the reason stated (this is a design
  decision that belongs in the diff or spec, not a silent default)

A walk that skips numbers or collapses into "surfaces covered" is invalid —
the summary sentence is exactly where discretionary omission hides. If an
item is expensive, say so and mark it; do not silently drop it.

Items S1–S6 (agent-launch parity) are a SEPARATE list with its own trigger,
kept out of 1–36 on purpose: they are `n/a` for almost every field change,
and a list that trains you to type `n/a` is how the walk decays. Walk them
when their trigger fires, with the same three verdicts.

## Input surfaces (new flag / option / request field)

1. CLI flags — `cmd/harness-cli/main.go` (submit / interactive / caps set),
   `cmd/harness-cli/session.go` (session new), and their help text.
2. CLI parse helpers — `cli/caps.go` (`ParseCaps`, `CapsCatalog`),
   `cli/scope.go` (`ParseScope`, `ScopesCatalog`) — one grammar, one parser;
   never a per-surface copy.
3. TUI cmdline verbs — `tui/cmdline.go` (`parseSetCaps`, per-command flag
   loops).
4. TUI keybindings — `tui/keys.go` (mainKeyMap + mainKeyBindings row —
   keys_test enforces the pair), dispatcher in `tui/app.go`.
5. TUI pickers/popups — `tui/authoritypicker.go`, `tui/popup.go` — remember
   batched KeyRunes (a fast `jjj` is ONE msg).
6. WebUI controls — `webui/index.html` + `webui/static/main.js` (chips via
   `buildCapChips`, checklists via `buildTaskChecklist`, dialogs via
   `<dialog class="picker-modal …">`).
7. WebUI command input — `runCmd` in `main.js` — a separate parser from the
   buttons.
8. wasm bridge — `cmd/harness-webui-wasm/main.go` `opts.Get("…")` names must
   match the JS request keys.
9. Session-default state — TUI `App.sessionCaps`/`sessionScope`; WebUI
   `spawnCaps`/`spawnScope` — every spawn call site must read them.
10. Every OTHER verb family — `file` / `git` / `forward` / `prune` /
    `notify` / `board` / `session` sub-verbs / `caps set`: each exists as a
    CLI flag set, a TUI cmdline parser, and (subset) a WebUI control or
    `runCmd` case. A new option on one family member (`file pull -n`) has
    the same per-UI parity obligations as a spawn flag.

## Display surfaces (field must be VISIBLE, not just accepted)

11. `ls` human rows — `cli/list.go` (row renderer).
12. `ls --json` — `cli/list.go` (JSON struct — always carries everything,
    no elision).
13. `whoami` — `cli/whoami.go` (both the operator and task branches).
14. `session ls` — JSON rows in `cli/` session listing.
15. `caps` catalog — `cli/caps.go` `WriteCaps` + SCOPE section.
16. TUI task table — `tui/tasks.go` `SetRows` columns.
17. TUI task detail popup (`d`) — `tui/detail.go` `formatTaskDetail` —
    **this one had no scope line for a full release**.
18. TUI runner detail — `tui/detail.go` `formatRunnerDetail`.
19. TUI picker rows — `tui/authoritypicker.go` `buildRows` (id, status,
    agent, repo, prompt head).
20. WebUI task row meta — `renderTaskList` metaText in `main.js`.
21. WebUI task detail sheet — the `addItem` action list per task.
22. WebUI dialogs' prefill — needs RAW values in the wasm snapshot
    (`capsBits`/`scopeBase`/`scopeIds` pattern) — labels like `all,-spawn`
    cannot be re-parsed in JS.
23. wasm snapshot JSON — the `ls` conversion map in
    `cmd/harness-webui-wasm/main.go` — label string AND raw value.

## Semantics axes — ask these PER OPTION, not per surface

Items 1–23 find missing knobs and missing pixels; these find wrong
MEANINGS. They apply to EVERY verb family (item 10), not just spawns: any
option reachable from more than one verb, mode, or path needs its meaning
written down per path (`prune` with ids vs the bare age sweep, `file push
-f` vs `pull -f`, a forward flag on register vs kill — same discipline).

24. Same option, other path — for every option on a multi-path verb: what
    happens on EACH path? "Applied", "kept", "ignored-with-error" — written
    down, not implied. The incident: spawn options on the resume path — a
    zero value that means "default" on create means "overwrite with
    default" on resume unless a presence bit says otherwise.
25. Presence — can the wire tell "not given" from "given the zero value"?
    If not and the difference matters (resume, set-style RPCs), add a
    presence bit — reserved bits in an existing byte first (`scope_present`
    cost zero layout change).
26. Session defaults — defaults (TUI `sessionCaps`/`sessionScope`, WebUI
    Compose state) feed FRESH spawns only. A resume must never inherit
    whatever the default picker happens to hold — gate it behind an
    explicit control.
27. Shared funnel — new request fields land in the shared builders
    (`cli.buildSubmitRequest` / `buildOpenInteractiveRequest` via
    `cli.SessionOpts`), never in one caller — native, wasm, and x11 all
    funnel through them, so a per-path field silently misses the other two.
28. Persistence — a new task field needs its `WALEvent` fields AND a
    decided replay meaning for records written before it existed (scope
    chose zero = subtree = old behaviour).

## Conventions (each learned from a user complaint)

29. Result messages name the target and the change — `caps set <id8>:
    caps=… scope=…`, never a bare count; counts only as a fan-out suffix
    (`+N descendant(s) clamped`) when a cascade actually reached something.
30. Results go to the result surface, not the status indicator — WebUI:
    `appendCmdOutput` (like the ✕ Cancel handler); `setStatus` is the
    CONNECTION badge. TUI: `a.cmdresult`.
31. Do not hide default values in detail-ish views — a hidden
    `scope=subtree` reads as "this task has no scope". Documented
    exception: CLI human `ls` rows and the TUI table elide the subtree
    default for row width — the JSON forms always carry it. If that
    exception draws complaints, drop it everywhere at once.
32. One serializer per grammar per runtime, pinned by tests —
    `scopeSpecFor` (Go) / `scopeSpecJS` (JS), both feeding
    `cli.ParseScope`; round-trip tests keep them honest. Fixed frames:
    popup/dialog width derives from the terminal/viewport, never content
    (TUI picker `fit()`, `.picker-modal.regrant-modal`).
33. A typed option either takes effect or errors — never silently ignored,
    never silently overwriting. Both failure shapes shipped at once: a lone
    `--scope` on resume was dropped, and `--caps` without `--scope` reset
    the task's scope. When the wire cannot express "keep", the fix is a
    presence bit, not documentation.

## Documentation surfaces

34. `README.md` — the feature's section (e.g. "Capabilities and scope"),
    the per-binary summaries near the top, AND the TUI cmdline verb list.
    Incident: the README still recommended the REMOVED `caps --on-resume`
    command and described the caps-only era a full feature later.
35. Agent-facing skill texts — `runner/agentskills/*/SKILL.md` is the
    go:embed source of truth; mirror to `.claude/skills/` and
    `.agents/skills/` in the same commit. Repo-dev skills
    (`implementation-pitfalls`, this file) live in `.claude/` only.
36. The feature's spec under `docs/superpowers/specs/` — semantic changes
    land as an Amendment section there, so the spec never contradicts the
    shipped behaviour a later reader verifies against.

## Agent-launch parity — SEPARATE trigger, walk S1–S6

Items 1–36 are one field across ~15 UI surfaces. This section is a
different axis: an agent is launched by the operator through a preset, and
the podman sandbox (`scripts/sandbox/`) re-runs that same agent through a
wrapper that must be kept in lockstep by hand. Nothing in `cli/`, `tui/`
or `webui/` mentions it, so a 1–36 walk cannot reach it.

**Trigger (walk S1–S6 instead of, or in addition to, 1–36):** adding,
renaming or removing an agent; changing an agent's bin path, argv
templates, log format, config directory, credential mode, or endpoint
domains; changing what env the runner hands an agent; changing the
`HARNESS_SERVER_CID` format or the server's addressing.

S1. Preset derivation — `scripts/agent_presets.py`: `KNOWN_AGENT_PRESETS`
    plus the `for _base in (…)` tuple that derives each `sandbox-<base>`.
    A new agent added to the map alone gets NO sandbox twin. The twin must
    stay DERIVED (only `bin` changes) — the first hand-copied version
    drifted and one-shot progress arrived as a single final blob instead
    of streamed events.
S2. Wrapper dispatch — `scripts/sandbox/agent-in-podman.sh` selects the
    agent from `basename $0`, so each agent needs its
    `<base>-in-podman.sh` symlink next to it. The default arm is
    `AGENT=claude`: a missing symlink does not fail, it runs Claude Code
    while the task row still reads `agent=sandbox-<name>`. Same silent
    wrong-binary class as a fresh interactive launch carrying no argv
    template — the BIN is the only selector every launch path carries.
S3. Agent table — the `case "$AGENT" in` block in that wrapper is the ONE
    place per-agent differences live: host bin, container bin, image
    fallback, config mounts, token file + token env, always-env,
    firewall-only env, egress domains, HOME (mount auth) and ephemeral
    HOME (token auth). An agent with no revocable-token mode is
    mount-auth ONLY —
    state that, because it puts that provider's credentials in the
    container with no narrower option.
S4. Firewall-only fields — `A_DOMAINS` and `A_FW_ENV` take effect ONLY
    under `--firewall` / `--firewall-proxy`, and `SANDBOX_PROXY_ALLOW`
    only under the proxy mode. A run in the default (open-egress) mode
    proves nothing about them; a missing domain surfaces as a fail-closed
    abort on someone else's task.
S5. Env and addressing contract — the wrapper forwards env by PREFIX
    (`HARNESS_*`), so a new `HARNESS_…` var rides along automatically and
    a differently-named one silently does not. Separately it parses
    `HARNESS_SERVER_CID` into ip/proto/port to build the harness-server
    carve-out; if the format changes and the parse fails, the carve-out is
    skipped entirely (deliberately fail-closed) and the bridged
    `harness-cli` stops working inside the container.
S6. Sandbox docs + the verifier — `scripts/sandbox/README.md` (agent table
    AND the security-model section), `.claude/commands/runner-up.md`
    (preset table), and `scripts/sandbox/probe.sh`: probe measures the
    claims from inside the container, so a claim added to the README with
    no probe line reads as verified while being unmeasured. Also state the
    upgrade step for any bin/path rename — `build_and_restart_all.py`
    replays a running slot's recorded argv, so live slots do NOT migrate;
    they must be stopped and brought back up.

## When to invoke

- Adding a field to `TaskInfo` / `RunnerInfo` / any snapshot row
- Adding or renaming a capability, scope form, or status value
- Adding an option to ANY verb family (spawn or otherwise)
- Adding an operator action (new keybinding / button / dialog / verb)
- Reviewing a diff that claims "CLI, TUI and WebUI covered"
- (S1–S6) Adding or renaming an agent, or changing its bin / argv templates
  / log format / config dir / credential mode / egress domains
- (S1–S6) Changing the env or the server addressing an agent is launched
  with — the sandbox wrapper re-derives both

When a defect ships through this checklist anyway: fix the defect AND add
the number that would have caught it. The list grows by incident, and the
1–N walk is renumbered — a stale N in reports is harmless; a skipped
surface is not.
