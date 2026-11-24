# The list's history bound: asked for, counted, and applied to what the caller sees — design

Status: built with this spec (2026-11-24).

## Problem

`94fba0c3` (2026-11-16) narrowed the list's 100-row cap to finished tasks:
`TaskStore.ListLiveAndRecent(100)` on the server, the same cut again in the
TUI. Three defects remained, found while asking how an operator with several
hundred finished tasks fares (operator, 2026-11-24):

- **P1 — nothing reaches a finished task older than the newest 100.** The cap
  was a constant; `ListQuery` carried an opaque `query :[..]u8` no code read.
  `ls`, the TUI and the WebUI listed the same 100, and the WebUI's 「id で開く」
  resolved only against the listed rows (`resolveTaskIdInput(…, lastTasks)`),
  so an older task could not be opened or resumed from the WebUI.
- **P2 — the cut was applied before visibility.** `handleList` took the 100
  newest finished tasks of the whole store, then filtered to what the caller
  may see. Another tree's newer tasks pushed a confined caller's own history
  out of its `ls`.
- **P3 — the cut said nothing.** No surface counted what it left out.

Two further consequences of the same cut, found while reading the callers:

- **P4 — the caps audit, `caps reach` and `caps what-if` read the cut list.**
  They count ended tasks on purpose (a resume brings their recorded caps
  back), so a holder older than the 100 was invisible to them.
- **P5 — a tree drawn from the list lost parents.** A live task whose creator
  had fallen past the cut was drawn as an orphan root.

## Decisions

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Fix P1–P3 now | operator (2026-11-24) |
| D2 | `ListQuery` becomes `finished_limit :u32`; `ListResultBody` gains `finished_omitted :u32` | this spec |
| D3 | The bound counts tasks visible to the caller (P2) | this spec |
| D4 | Every visible creator, up to the root, of a listed task is listed too, outside the bound and outside the count (P5) | this spec |
| D5 | Audit / reach / what-if and `credential ls` ask for every finished task (P4) | this spec |
| D6 | The rule lives once, `protocol.ListKeep`; the server answers with it and the TUI cuts with it | this spec |
| D7 | Surfaces: CLI `ls --finished N\|all`; TUI `H`; WebUI a note under the task list with a button, and 「id で開く」 widens by itself | this spec |

**D2.** A client that wants everything sends `0xffffffff`; it is a limit like
any other, so the field has no sentinel. 0 lists no finished task (creators
excepted). The default, 100, is the client's (`verb.FinishedDefault`), not the
server's. `finished_omitted` lets a client say "N older" instead of guessing
from a full page; 0 means the list holds every visible finished task. The
body's `tasks_len` is a u16: past it the server drops the oldest finished rows
and counts them, rather than failing the encode.

**D5.** `credential ls` is not a history: a revoked credential is what
`credential rekey` brings back, and the TUI already listed every credential
"ended ones included" — of the rows it had been given.

**D6.** The TUI keeps a cut of its own because the snapshot can hold more than
the table shows: the whole history while the caps audit is open or has pinned
rows (so the audit sees it), and tasks added by events between polls. It cuts
with the same function the server uses.

## Wire and rollout

`ListQuery` and `ListResultBody` change length, and the body is decoded with
`DecodeExact`. Until server and client are both rebuilt, a client of either
age against a server of the other fails its list (`ls`, the TUI's table, the
WebUI's poll). No runner decodes either format, so the runner fleet is not
affected; agents' own `harness-cli ls` is, until the runner host's `bin/` and
the server are both current. No shim (individual dogfood): restart the server
and rebuild the clients together.

## Surfaces

| Surface | What |
| --- | --- |
| CLI `ls` (all four forms) | `--finished N\|all` (default 100); last TASKS line `finished: N listed, M older left out (ls --finished all)`, printed with 0 too; `--json` carries `finished_omitted` always |
| CLI `caps audit/reach/what-if`, `credential ls` | every finished task (D5) |
| MCP | `ls` gains the `finished` property (same verb row) |
| TUI task table | `H` toggles every finished task / the newest 100; a last header row names what is left out or how to go back |
| TUI caps audit | the snapshot widens while it is open or pinned (D6) |
| WebUI task list | note + button under the list; the typed `ls` follows it |
| WebUI 「id で開く」 | not listed and the list is cut → widen, then resolve again |
| WebUI caps audit / reach / what-if, credentials | every finished task (D5) |
| `ls --filtered`, `--order` (WebUI/CLI) | unchanged; they act on the rows listed |

`session ls`, `grid`, the overview and prune's local classification keep the
default: they are about live tasks, or (prune) treat a task the list does not
hold as safe to remove, which an old finished task is.
