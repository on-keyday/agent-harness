# Task tree: id grants and global reach

Status: DECIDED (operator, 2026-10-14) — scope below, including `view` edges
and a node marker for `global`.

## Problem

The WebUI task tree (🌳 chip) draws creator → child edges only. Authority a
task holds beyond its own subtree is invisible there:

- `--scope ids:<id>` / `subtree+ids:<id>` — the base scope's ids
- `--scope-for CAPS=ids:<id>` — an override's ids
- `+vis-ids:<id>` — view-only ids
- a `global` base on the scope, an override, or the visibility rank

All of it is on the wire already (`TaskInfo.scope`, `TaskInfo.overrides`) and
printed per row as `scope=…`, but reading "who can reach whom" out of those
strings means cross-referencing 32-hex ids by hand.

## Design

**Go (`cli/tasktree.go`)** — `BuildTaskGrants(tasks) TaskGrants`, beside
`BuildTaskTree`, so the derivation is tested in Go and the browser only paints.

- Edge `From → To` per target named in any id clause, self excluded.
  `Kind` is the strongest clause reaching it:
  - `action` — the base scope's `ids`; carries every held bit no override claims
  - `override` — only an override's `ids`; carries that override's mask
  - `view` — `vis_ids`, or an id clause whose mask holds no bit (the id is
    still disclosed to the task)
  `Caps` is the union of bits reaching `To` through id clauses.
- `Global[id]` — a task with a `global` base: `action` (scope base global and
  it holds non-override bits), else `override` (an override base is global),
  else `view` (visibility rank global).
- An edge already covered by the same task's global reach is dropped, since
  the marker says it. A `view` edge survives unless visibility is global,
  because the id still adds visibility under a narrower visibility rank.
- A target not in the listing (pruned) is counted in `Offscreen[From]`, not
  dropped silently.

**wasm** — snapshot gains `grantEdges` (`from`, `to`, `kind`, `caps` label),
and each `taskTree` node gains `global` / `globalCaps` / `offscreen`.

**WebUI (`renderTaskTreeGraph`)** — grant edges as arrowed curves, distinct
from the elbow creator edges: `action` solid, `override` dashed, `view` dotted
and faint, override edges labelled with their caps. Edge tooltip names kind
and caps. A global node gets a ring around its dot in the kind's colour and
line style, and a tooltip line; `+N 一覧外` appears beside a node only when
N > 0. A legend shows above the graph only when any of these exist.

## Surfaces

| Surface | Verdict |
| --- | --- |
| WebUI tree graph | implemented |
| WebUI list row, TUI detail popup, CLI `ls` / `ls --tree` | omitted: they already print `scope=` with the ids and `global` (`cli/list.go:248`, `tui/detail.go:230`); a text gutter cannot draw a non-tree edge |
| input surfaces | not applicable — display only |

## Not done

- No separate toggle for grant edges. Add one when a dense fleet makes the
  edges unreadable.
- No reverse view ("who can reach me"). The arrowheads give it on the graph.
