# WebUI desktop: tabs, and a scroll that is not captured

## Problem

On desktop the WebUI stacks every section in one column and the operator scrolls
past all of them to reach any one of them. Two effects compound:

**It is long.** Measured at 1440x900 against a running server with *nothing
populated* — no runners, no tasks, no notification feed, no forwards, no board
topics:

| section | height |
|---|---|
| `connections` (topology panel is fixed-height even when empty) | 778px |
| `interactive` (terminal at 60vh + touch-key bar) | 691px |
| `compose` | 252px |
| `files` | 209px |
| `notifications` | 189px |
| `tasks` / `cmdline` / `board` / `runners` | 133 / 102 / 72 / 58px |

Total 2817px = 3.13 screens, empty. Real data lengthens the task list, the
notification feed (400px cap), the forward list and the board list on top of
that. Meanwhile `#app` is 1408px wide and the controls inside it occupy roughly
400-600px: the horizontal axis is idle while the vertical one grows.

**It captures the wheel on the way down.** Two kinds:

- *Hard capture.* `attachTopoZoom` (`webui/static/main.js:4061`) binds `wheel`
  on the topology host with `{passive: false}` and calls `preventDefault()`
  with no modifier condition. While connected, every wheel event over that
  778px panel becomes a zoom and none of it reaches the page. The panel sits at
  y=955 of a 2817px document, so any top-to-bottom traversal must cross it.
- *Soft capture.* `.task-list` (240px cap), `pre` (200px), `#notify-feed`
  (400px), `#file-entries` (300px), `.raw-output` (320px) each scroll their own
  content first. Scroll chaining does eventually carry the page, but the wheel
  cost of getting past a long task list is paid every time.

So the complaint is length multiplied by tollgates. Fixing one leaves the other.

## What already exists

- `webui/index.html:29-36` — a `#tabbar` with six buttons (`terminal`, `tasks`,
  `files`, `notify`, `conns`, `board`); every `<section>` already carries
  `data-tabgroup`. The markup is complete and surface-agnostic.
- `webui/static/style.css:313` — `.tabbar { display: none; }`, and
  `style.css:326-346` — a `@media (max-width: 600px)` block that turns the bar
  on (`display:flex`, `position:sticky`) and hides every section not in the
  active group. Tabs are therefore a mobile-only behaviour by CSS alone.
- `webui/static/main.js:1948-1984` — `setActiveTab` already sets
  `body.dataset.activeTab` on desktop too; it is the CSS that no-ops there. It
  additionally carries a desktop-only branch that `scrollIntoView`es the
  terminal or files section, which exists purely because those sections sit
  below the fold.
- `webui/index.html:12` — `#toast-host` is a direct child of `<body>`, outside
  `<main>`, so notifications surface regardless of the active tab.
- Multi-session watching already has its own surfaces: the session grid modal
  and the session preview modal, both body-level `<dialog>`s.

Two measurements that bound the change, both taken rather than assumed:

- `renderConnTopology` builds against a fixed `const W = 640, H = 560` viewBox
  (`main.js:4221`) and never measures its host. Rendering into a `display:none`
  section produces a correct SVG, so the ~5s poll needs no visibility guard.
- Hiding an ancestor fires `ResizeObserver` once with a 0x0 `contentRect`
  (verified in-browser), which reaches the observer at `main.js:2153`. But
  `addon-fit.js`'s `proposeDimensions()` reads `getComputedStyle(hidden).height`
  as `"auto"`, `parseInt`s it to `NaN`, and `fit()` returns early on its
  `isNaN` guard. `term.cols/rows` survive intact. The only consequence is one
  redundant — and correctly-valued — resize frame per tab switch away.

## Decisions

1. **Tabs stop being mobile-only.** Delete `.tabbar { display: none; }`
   (`style.css:313`) and hoist out of the `max-width: 600px` block both the
   `.tabbar` rule (`display:flex`, `sticky`, `top:0`, `z-index`, background)
   and the six-selector `body[data-active-tab=…] [data-tabgroup]:not(…)
   { display: none }` group. What stays behind in the media query is
   mobile-specific sizing only: gap, padding, font-size, and `.tab-btn
   { flex: 1 }`.

2. **Desktop tabs are left-aligned and content-sized.** `.tabbar
   { justify-content: flex-start }` and `.tab-btn { flex: 0 0 auto; padding:
   .45rem 1.1rem }`. Keeping the mobile `flex: 1` would stretch six buttons
   across 1408px at >230px each.

3. **`setActiveTab` resets scroll unconditionally** (`main.js:1957`) — a tab
   switch starts at the top on every width.

4. **The desktop `scrollIntoView` branch goes away** (`main.js:1966-1971`). Its
   entire reason was that the terminal and files sections sat below the fold;
   with tabs they are the fold. The `const mobile` local it depends on
   (`main.js:1949`) becomes unused and goes with it.

5. **Two inner caps grow into the freed vertical space.** `.task-list`
   (`style.css:273`) 240px -> `min(45vh, 520px)`; `#notify-feed`
   (`style.css:350`) 400px -> `60vh`. Both are desktop-side values; the mobile
   `.task-list { max-height: none }` (`style.css:308`) is untouched. This
   attacks the soft capture at its cause — a taller box needs less wheel to
   traverse — without removing the caps, which are what keep a long list from
   re-lengthening the page.

6. **Topology zoom requires a modifier.** In the `wheel` handler
   (`main.js:4061`), `if (!e.ctrlKey && !e.metaKey) return;` goes *before*
   `preventDefault()`. Plain wheel then reaches the page and the hard capture
   is gone. Two consequences worth naming: trackpad pinch arrives as a wheel
   event with `ctrlKey: true`, so pinch-to-zoom starts working as a side
   effect; and browser Ctrl+wheel zoom is taken over while the cursor is on
   that panel, which is the usual bargain for map-like widgets.

7. **The modifier is discoverable.** A static hint next to the existing
   `⤢ reset` button, since a gesture that silently stopped working is worse
   than one that never existed.

8. **Drag-pan is untouched.** It starts on `mousedown` and never competes with
   a wheel.

9. **The terminal `ResizeObserver` gets the guard its sibling already has.**
   `main.js:2153-2160` forwards `resizeInteractive` on every observation;
   `fitTerminalToViewport` (`main.js:1931`) forwards only when `cols`/`rows`
   actually changed. Give the observer the same check. The measured impact is
   one redundant frame per tab switch, not corruption — this is consistency
   between two pieces of code doing the same job, not a bug fix.

## Non-goals

Two-column layout *within* a tab; persisting the active tab across reloads;
making the terminal height dynamic on desktop (`interactive` is 691px and fits
a 900px viewport); hiding the touch-key bar on desktop. Splitting Raw connect
out of the Connections tab is deferred — that tab is the one most likely to
still exceed a screen, and the decision belongs after measuring it, not before.

## Verification

The assets are `//go:embed`ed (`webui/embed.go`), so a server serving the
embedded copy will not see CSS/JS edits. Run a local dummy harness with
`--webui-dir` pointed at this worktree's `webui/` (`cmd/harness-server/main.go`)
and drive it with Playwright:

- At 1440x900, per-tab `document.documentElement.scrollHeight` — the tasks,
  files, notify and board tabs should land at or near one screen. Record what
  the Connections tab actually measures; it is the input to the deferred split.
- Wheel over the topology panel while connected scrolls the page; Ctrl+wheel
  over it changes the SVG `viewBox` and does not scroll the page.
- At 390px, no mobile regression: tab bar, section switching, terminal fit, and
  the touch-key row all behave as before.
- `make check`.
