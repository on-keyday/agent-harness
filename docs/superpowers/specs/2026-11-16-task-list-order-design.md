# Task list ordering modes — short design

Status: approved by the operator in conversation, 2026-11-16. Step 1 of three
(this; then the capability u64 widening; then task labels, which get their own
spec).

## Intent

With many tasks, the operator cannot find the ones that matter on opening a
list. They want to switch how the list is ordered and grouped: live ones on
top, grouped by state, grouped by repository (and later by their own labels).

## Modes

| mode | order | groups (header shown) |
|---|---|---|
| `created` | created, newest first | none |
| `activity` | most recent activity first (last output for a live session, else the latest of ended / started / created) | none |
| `live` | live tasks (Queued, Running, Detached, Held) by start (else create) time, newest first; then finished by end time, newest first | `live`, `finished` |
| `state` | groups in this order, each by activity, newest first | `needs you` (lifecycle waiting / error, or with no lifecycle report a program blocked / error), `ready` (lifecycle idle, or program idle / done), `working` (lifecycle working, or program working), `no report` (live, neither reported), `queued`, `finished` |
| `repo` | groups by repository, named by its last path element (the full path when two repos share it), sorted by name; inside, live ones by activity, then finished by end time | one per repository |

Empty groups are not shown.

## Decisions

- **Defaults stay as they are** (the operator does not want looks changed
  unasked): the TUI opens in `created`, the WebUI in `activity`, `ls` keeps the
  server's order unless `--order` is given.
- **The chosen mode is remembered**: the TUI for the run, the WebUI in the
  browser (per-viewer convenience, localStorage).
- **Tree view and ordering are exclusive**: while the TUI's tree (`T`) is on,
  `o` reports that and does nothing; `ls --tree` and `--order` are exclusive.
- **TUI**: `o` cycles created → activity → live → state → repo. Grouped modes
  insert header rows (`── ready (2) ──`) that the cursor skips. The mode is
  reported in the result pane on every switch. (Approved text said "on the
  strip above the table"; it does not fit the strip's 80 cells with the fleet
  summary, so the result line and the headers carry it instead.)
- **WebUI**: a select beside the Active / Finished chips; grouped modes draw a
  header between groups.
- **CLI**: `ls --order MODE`; grouped modes print a header line per group;
  with `--json` the rows come in that order and each carries `group`.
- **One implementation**: the ordering and grouping are written once in Go
  (`cli.OrderTasks`). The TUI and CLI call it; the WebUI calls it through the
  wasm bridge on the snapshot it last fetched, passing the ids its filter
  admits. No surface restates the rules.

## Non-goals

Labels (next spec). Server-side persistence of the chosen mode. Changing any
default order.
