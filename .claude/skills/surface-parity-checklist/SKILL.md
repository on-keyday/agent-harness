---
name: surface-parity-checklist
description: Use BEFORE adding, changing, or displaying any operator-visible task/runner field (caps, scope, status, agent, repo, …), adding an option to any verb family, or wiring a new operator action's result reporting — AND before adding/renaming an agent or changing its bin, argv templates, log format, credential mode, egress domains, or launch env. NUMBERED checklists that must be walked item by item with a verdict per number — never summarized: 1–37 for every input surface, display surface, per-path semantics axis and result/display convention across CLI/TUI/WebUI/wasm, plus S1–S6 for preset↔podman-sandbox agent-launch parity.
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
**every number from 1 to 37 in order** and record a verdict for each:

- `done` — implemented/verified, with the file touched
- `n/a` — genuinely not applicable, WITH the reason stated
- `omitted` — deliberately skipped, WITH the reason stated (this is a design
  decision that belongs in the diff or spec, not a silent default)

A walk that skips numbers or collapses into "surfaces covered" is invalid —
the summary sentence is exactly where discretionary omission hides. If an
item is expensive, say so and mark it; do not silently drop it.

Items S1–S6 (agent-launch parity) are a SEPARATE list with its own trigger,
kept out of 1–37 on purpose: they are `n/a` for almost every field change,
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
    **Reaching the funnel is not reaching the wire.** `cli.SessionOpts` is a
    struct its callers fill by hand, so "the field is in SessionOpts and both
    builders copy it" can be true while callers never set it — which is what
    happened to `--scope-for`: seven hand-written `cli.SessionOpts{…}` literals
    in `tui/`, six of them older than the field. This item passed. Item 28a is
    the half it does not cover.
28. Persistence — a new task field needs its `WALEvent` fields AND a
    decided replay meaning for records written before it existed (scope
    chose zero = subtree = old behaviour).

28a. **Follow the value to the request build, and count the builds.** Items
    1–10 check that a surface ACCEPTS the option; they stop at the parser.
    For each surface that accepts it, grep every construction of the request
    struct that surface reaches and confirm the field is set in each. One
    checklist cell routinely hides several: "TUI cmdline" reaches SIX request
    builds (`session new`, `session new -d`, `--x11`, `r`/`R` resume — pinned
    AND its Any-selector retry — and `interactive`), plus `submit` on another
    file. `--scope-for` parsed correctly, rode `SessionNewAction` and
    `spawnAuthority`, and died at those six; every 1–10 cell read `done` and
    the operator got a task with the bare scope. `--caps` arrived, which made
    the loss look like a scope bug rather than a dropped field.
    The fix that ends the class is a constructor, not a comment — the field's
    doc comment already said "a field set in one caller and missed in the
    others is the established failure mode on these routes". Give the struct
    one builder (`Authority.opts`) and pin it with a test that fails when a
    second literal appears (`tui/session_opts_test.go`), so the next authority
    field cannot be added in one place and missed in six.
    Cheap check when you cannot restructure: `grep -c 'TheStruct{'` per package
    and diff it against the count of sites that set your new field.

## Conventions (each learned from a user complaint)

29. Result messages name the target and the change — `caps set <id8>:
    caps=… scope=…`, never a bare count; counts only as a fan-out suffix
    (`+N descendant(s) clamped`) when a cascade actually reached something.
30. Results go to the result surface, not the status indicator — WebUI:
    `appendCmdOutput` (like the ✕ Cancel handler); `setStatus` is the
    CONNECTION badge. TUI: `a.cmdresult`.
31. Do not hide a value because of what it IS — not in detail views, and
    **not in rows either**. A hidden `scope=subtree` reads as "this task
    has no scope"; an elided `cowrite=0 viewer=0` reads as "this row does
    not report watchers". **The row-width exception this item used to
    grant is withdrawn** (2026-08-20, on operator complaint): `ls` rows
    and `whoami` now print `scope=subtree` like the WebUI row, the TUI
    detail popup and both JSON forms already did — only those two still
    carried it — and `IsDefaultScope`, whose doc comment existed to
    justify the eliding, is deleted.
    Gate rendering on whether the subject EXISTS, never on its value: a
    live session prints `cowrite=0 viewer=0`; a finished task prints
    nothing, because it has no session to describe. **Zero is a
    measurement, absence is not**, and collapsing them throws the
    measurement away.
    The trap: neighbouring fields (`act=`, `err=`, `exit=`, `by=`) DO
    elide — on ABSENCE, a different axis. Copying their form without
    checking their axis is how this recurs, and it did: the observer
    counts shipped eliding-at-zero in the same change that added them to
    fix exactly that ambiguity elsewhere.
32. One serializer per grammar, pinned by tests. Not one PER RUNTIME — that
    was the old wording and it licensed exactly what went wrong: `scopeSpecFor`
    (Go) and `scopeSpecJS` (JS) were both hand-written next to
    `cli.ScopeLabel`, and both knew three of the six scope bases and neither
    half of the visibility pair. A picker or dialog apply silently turned
    `descendants` into `subtree` and dropped `global/…`. Neither copy had the
    round-trip test this item claimed for them; the sentence was the evidence.
    Both are gone: the picker calls `cli.ScopeLabel`, the browser calls
    `cli.ScopeSpec` over the wasm bridge (`harness.scopeSpec`). When a grammar
    must be produced in the browser, EXPORT the Go serializer rather than
    mirroring it — a mirror has no way to fail loudly when the grammar grows.
    A "round-trips" / "can be pasted back" claim in a comment is the cue to
    write the test, never evidence that one exists. Fixed frames:
    popup/dialog width derives from the terminal/viewport, never content
    (TUI picker `fit()`, `.picker-modal.regrant-modal`).
33. A typed option either takes effect or errors — never silently ignored,
    never silently overwriting. Both failure shapes shipped at once: a lone
    `--scope` on resume was dropped, and `--caps` without `--scope` reset
    the task's scope. When the wire cannot express "keep", the fix is a
    presence bit, not documentation.
34. A table whose column SET varies (by width, mode, capability) must
    rebuild its rows through the same decision, and swap the two together.
    bubbles' `SetRows` and `SetColumns` each re-render immediately and
    `renderRow` indexes `row[i]` per COLUMN, so a moment holding N columns
    against N-1-cell rows is an index-out-of-range panic — hit within
    minutes of making the tasks table's Obs column width-conditional. The
    fix shape is `applyColumns`: empty the rows, set the columns, rebuild,
    restore the cursor (a resize must not move the selection).
    Do NOT "just always emit the widest row": that only works if the
    optional column is LAST. `Obs` sits before `Repo`, so a stale extra
    cell would render every following value under the wrong header —
    silently, which is worse than the panic.
    Corollary: `tui/tasks.go` `SetRows` may no longer be the only place
    cells are built. Grep for every caller of the rebuild path before
    adding a cell.

34a. **A form field must be the same KIND of control as its neighbours.**
    The WebUI spawn form expresses caps as chips, a scope base as radios and
    scope ids as a task checklist. `--scope-for` shipped there as a
    `<textarea>` you typed `CAPS=SCOPE` into — an override IS a capability
    mask plus a scope, so both halves already had controls and neither was
    used. Caught on operator complaint (2026-08-20): *"UI として文字列打たせる
    のどうなの"*.
    The command input is where typing belongs; a form is not. If a new option
    decomposes into things the surface already has controls for, build it from
    those — `buildCapChips`, the base radio row, `buildTaskChecklist` — and
    serialize to the wire grammar at send time, so the PARSER still lives in
    Go and the browser cannot drift from what the CLI accepts.
    Corollary, and the reason this is worth a control rather than a box: a
    constraint the server enforces should be **unbuildable** in the UI, not
    merely rejected. Overlapping override masks are refused server-side; the
    row UI disables a chip another row already claims, so the operator cannot
    construct the rejected state at all.
    Two traps met while doing it, both costing a rebuild-and-reload cycle:
    the dummy server serves the EMBEDDED assets, so `make webui-build` alone
    changes nothing it hands out (`make build` + restart it, or run it with
    `--webui-dir`); and re-navigating to the SAME url differing only in its
    `#fragment` does not reload the page, so every check after an edit can
    silently read the old DOM — add a throwaway query param.
    Do not make a control depend on the first snapshot arriving: build it on
    `toggle` of its `<details>` as well, or an operator who opens the section
    on an empty server finds it blank.
    **And carry the control to its siblings in the same walk.** The
    exclude-self checkbox was added to the override rows and not to the
    task's own base row directly above them, so the WebUI could express
    `descendants` for one capability but not for the task — three of the six
    bases unreachable, in the dialog the fix had just been applied to. The
    same edit left a CSS hole: the mobile rule that keeps a control inline
    with its label enumerated `input[type="radio"]` in the base row and
    `input[type="checkbox"]` in the checklist, so a CHECKBOX in the BASE ROW
    fell between the selectors and rendered as a block above its text at
    390px. Select by container, not by control type.

## Documentation surfaces

35. `README.md` — the feature's section (e.g. "Capabilities and scope"),
    the per-binary summaries near the top, AND the TUI cmdline verb list.
    Incident: the README still recommended the REMOVED `caps --on-resume`
    command and described the caps-only era a full feature later.
36. Agent-facing skill texts — `runner/agentskills/*/SKILL.md` is the
    go:embed source of truth; mirror to `.claude/skills/` and
    `.agents/skills/` in the same commit. Repo-dev skills
    (`implementation-pitfalls`, `dummy-harness`, this file) live in
    `.claude/` only. **Now test-enforced** — `runner/agentskills`'
    `TestMirrorsMatchEmbeddedSkills` fails on a byte difference and
    `TestAgentsMirrorHasNoExtraSkills` fails on a `.agents/` skill this
    package does not embed, so this item is a reminder rather than the only
    guard. It became one because the manual form drifted three times at
    once: `.agents/` shipped an older `harness-cli`, `landing-to-main` and
    `supervising-workers` than the embed FS, which meant agents in OTHER
    repositories were reading instructions this repo had already replaced.
    Editing only the copy you happen to be reading is the way in — the
    embedded file is the one to edit, then copy it over both mirrors.
37. The feature's spec under `docs/superpowers/specs/` — semantic changes
    land as an Amendment section there, so the spec never contradicts the
    shipped behaviour a later reader verifies against.

## Agent-launch parity — SEPARATE trigger, walk S1–S6

Items 1–37 are one field across ~15 UI surfaces. This section is a
different axis: an agent is launched by the operator through a preset, and
the podman sandbox (`scripts/sandbox/`) re-runs that same agent through a
wrapper that must be kept in lockstep by hand. Nothing in `cli/`, `tui/`
or `webui/` mentions it, so a 1–37 walk cannot reach it.

**Trigger (walk S1–S6 instead of, or in addition to, 1–37):** adding,
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

**Then record it in `firing-log.md`, beside this file.** One entry per walk,
listing only the items that came back `done`/`omitted` and — filled in later,
when a defect surfaces — `missed`. Everything unlisted was `n/a`; writing
that out would bury the signal. The log answers the two things this list
cannot answer about itself: which items keep getting missed (their wording is
wrong, not the walker), and which never fire at all (dead weight, and the
`n/a` reflex is how the walk decays). It also records **walks that were
skipped**, which is a fact about when the trigger fails to fire in practice
rather than on paper — that has already happened twice.
