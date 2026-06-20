# Session-default capabilities for TUI/WebUI spawns

Date: 2026-06-20
Status: design — awaiting review

## Problem

The capability system (`docs/superpowers/specs/2026-06-20-harness-cli-capabilities-design.md`,
landed) lets a spawner attenuate a child task's authority via `--caps` on
`harness-cli submit` / `session new`. But the operator's interactive surfaces —
the TUI and the WebUI — always spawn with `Capability_All` (the builders default
to all, Task 7). To confine a child from the TUI/WebUI today, there is no knob;
the operator would have to drop to the CLI and type `--caps` per spawn.

Operators want to **pre-set a capability set once per session** and have
subsequent spawns from that TUI/WebUI session follow it, without re-specifying it
each time.

## Decision

Each interactive client holds an **in-memory, session-scoped default capability
set** (`sessionCaps`, initial `Capability_All`). Every spawn from that client
sends `sessionCaps` as the request's `RequestedCaps`. The server enforcement is
unchanged: it intersects with the caller's authenticated caps (the operator holds
all), so `sessionCaps` effectively becomes the spawned child's grant.

This is a **client-side ergonomic convenience**, not a new trust boundary: the
operator is already the trusted root with full authority; `sessionCaps` is a
self-imposed guard ("don't spawn over-privileged children in this session").
Enforcement remains entirely server-side via the existing capability system.

**Semantics = default (not ceiling).** `sessionCaps` is the value spawns use.
There is no per-spawn override UI in this iteration (YAGNI); if one is added
later, it would take precedence over `sessionCaps`.

**Lifetime = session only.** Held in memory (the TUI process / the WebUI browser
tab). Not persisted across restarts. Resetting/persisting is out of scope.

## Scope

- A shared capability-name parser/list usable by CLI, TUI, and WebUI.
- TUI: a `caps` command to show/set `sessionCaps`; all TUI spawns use it.
- WebUI: a flat toggle-chip selector to show/set `sessionCaps`; all WebUI spawns use it.

### Non-goals

- Per-spawn override UI (a spawn dialog that overrides `sessionCaps` for one task).
- Persistence of `sessionCaps` across restarts (config file / localStorage).
- Any change to server-side enforcement, the wire schema, or the capability set
  itself (all landed and unchanged).
- A `caps` knob for the X11 spawn path beyond what already flows (X11 spawns will
  pass `sessionCaps` like other spawns — no special X11 handling).

## Shared capability parsing (DRY)

`parseCaps` and the granular cap list currently live in `cmd/harness-cli/caps.go`
(names sourced from `Capability.String()`). Extract them into the `cli` package
(imported by `cmd/harness-cli`, `tui`, and `cmd/harness-webui-wasm` alike) so all
three surfaces share one source:

- `cli.ParseCaps(s string) (protocol.Capability, error)` — empty → `Capability_All`;
  comma-separated snake_case names → OR; unknown → error. (moved verbatim)
- `cli.GrantableCaps() []protocol.Capability` — the granular bit list (the
  individual caps a UI offers as chips / a flag may name), names via `.String()`.

`cmd/harness-cli` then calls `cli.ParseCaps` (its local copy is deleted — no
duplicate). The cap **names** still have exactly one source: `Capability.String()`.

## TUI

- **State:** add `sessionCaps protocol.Capability` (init `Capability_All`) to the
  TUI app model.
- **Command:** a `caps` cmdline command (alongside the existing `submit` etc. in
  `tui/cmdline.go`):
  - `caps` (no args) → print the current set (via `.String()` of each enabled bit).
  - `caps <names>` → `cli.ParseCaps(names)` → set `sessionCaps`; on parse error,
    show the error in the TUI status line (no state change).
  - `caps all` / `caps none` work via `ParseCaps` (`all`→All, `none`→None).
- **Apply:** every TUI spawn path passes `sessionCaps` through the `...AndCaps`
  builder variants (added in capabilities Task 7):
  - `DoSubmit` (`tui/client.go`) → `SubmitWithSelectorArgsAndCaps(..., sessionCaps)`.
  - interactive new-session paths (`tui/interactive.go`, both the plain and X11
    branches) → `OpenInteractiveWithSelectorArgsAndCaps(..., sessionCaps)` /
    `OpenInteractiveX11(..., sessionCaps)`.
  - Threading: `sessionCaps` lives on the model; the `tea.Cmd`-producing helpers
    take it as a parameter (do not read global state inside the helper).

## WebUI

- **State:** a JS-side `spawnCaps` (a `Capability` uint32, default = all bits)
  held in the page (module-level / app state), per browser tab.
- **Selector:** a flat **toggle-chip row** near the spawn/new-session form:
  - `[all] [none]` quick-set buttons, then one toggle chip per cap from
    `cli.GrantableCaps()` (rendered via a value the wasm side exposes to JS —
    e.g. a `harnessCapList()` returning `[{name, bit}]`, and the chip click
    toggles the bit in `spawnCaps`).
  - A chip is "on" (✓, accent color) when its bit is set in `spawnCaps`, "off"
    (muted) otherwise. Reflects the current `spawnCaps`.
  - **Effective-set readout:** alongside the chips, a read-only line shows the
    OR-combined result as the comma-joined snake_case names of the enabled caps,
    collapsing to `all` when every bit is set and `none` when zero (e.g.
    `caps: spawn,file_read`). It updates live as chips toggle. This is the same
    string `cli.ParseCaps` accepts, so it doubles as a copy-paste source for a
    CLI `--caps` invocation. Names come from the shared source (`Capability.String()`),
    not a separate literal list.
  - Styling matches the existing WebUI dark palette (#1e1e1e / #d4d4d4, accent
    consistent with existing controls) and the <=600px mobile layout (chips wrap;
    usable at 390px). Verify desktop + 390px in Playwright per WebUI conventions.
- **Apply:** `harnessSubmit` and the WebUI interactive-new call
  (`cmd/harness-webui-wasm/main.go`) pass `spawnCaps` as `RequestedCaps` through
  the same `...AndCaps` builder variants. Default (all chips on) = `Capability_All`
  = current behavior, so existing flows are unchanged until the operator toggles.
- The WebUI builds the mask directly from toggles (OR of enabled bits) — it does
  NOT parse a comma string; `cli.GrantableCaps()` supplies the chip set + labels.

## Data flow

```
operator toggles chips / runs `caps X`
        │
        ▼
client holds sessionCaps (uint32 bitmask)
        │  (passed as a param, not global)
        ▼
spawn → *WithSelectorArgsAndCaps(..., sessionCaps)  → RequestedCaps on the wire
        │
        ▼
server: caps_child = caller_caps(=All for operator) ∩ RequestedCaps  (unchanged)
```

## Error handling

- TUI `caps <bad>`: `cli.ParseCaps` returns an error → shown in the status line;
  `sessionCaps` unchanged.
- WebUI: chips can only produce valid bits (no free text), so no parse error path;
  `[none]` yields `Capability_None` (a deliberate, valid "data-plane only" choice).
- A spawn always sends an explicit `RequestedCaps` (the builders never send an
  unset/zero by accident — guaranteed by the capabilities Task 7 default-All
  baseline; here the client always supplies `sessionCaps`).

## Testing

- **Shared parser:** `cli.ParseCaps` / `cli.GrantableCaps` unit tests (moved from
  `cmd/harness-cli`); `cmd/harness-cli` still passes its existing flag test
  against the relocated function.
- **TUI:** unit-test the `caps` command parse (sets the model field; bad input is
  rejected with state unchanged); a test that a spawn helper threads `sessionCaps`
  into the request it builds.
- **WebUI:** Playwright round-trip — toggle the chips to a confined set, spawn,
  and assert the created task is confined (e.g. a control-plane op from it is
  denied), OR at minimum assert the spawn request carried the reduced
  `RequestedCaps`. Verify the chip row renders and is usable at desktop width and
  390px, dark palette, and that the effective-set readout updates to the correct
  comma-joined string (incl. the `all`/`none` collapse) as chips toggle.
