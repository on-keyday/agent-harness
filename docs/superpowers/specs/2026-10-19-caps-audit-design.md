# Capability audit — who holds which power — design

## Problem

- **P1.** A task keeps a strong capability after the reason for it is gone.
  The case that prompted this: after re-arranging agents' parent links
  (`caps set-parent`), an agent still held `spawn`, and the operator learned it
  only when someone pointed it out. Nothing in the UIs answers "who holds
  `spawn` right now?" in one look.
- **P2.** The task views show one task's authority at a time — `ls` rows, the
  TUI `d` popup, the WebUI task sheet, the tree view. Finding every holder of a
  given bit means reading every row.
- **P3.** Since `resume` / `resume_exact` were added (spec
  `2026-10-19-resume-caps-design.md`), a task holding the old `all` (`0xfffff`)
  renders as a list of twenty names rather than `all`. That makes this kind of
  holder harder to spot in a row, not easier.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-19). **this spec** =
the author chose it while writing.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The view is organised **by capability**: one entry per bit, listing its holders. A task × capability matrix was prototyped and not chosen | operator |
| D2 | Bits are ordered strongest first, and each is labelled with a tier, 強 / 中 / 弱 (strong / mid / weak), from one table shared by every surface | operator |
| D3 | Tier table — 強: `spawn`, `resume`, `exec_control`, `purge`, `prune`, `board_send`, `runner_admin`, `exec_tap`, `forward_tap`, `forward_local`, `forward_remote`. 中: `exec_cowrite`, `exec_run`, `file_write`, `cancel`, `board_peer`, `board_observe`, `resume_exact`. 弱: `exec_view`, `exec_resize`, `file_read`, `notify` | operator |
| D4 | Each holder shows the scope **that bit** resolves to (`scope_by_cap`), coloured by the action rank's breadth: red = global, yellow = subtree / descendants, green = none / ids. A holder whose scope comes from a `--scope-for` override is marked `*` | operator |
| D5 | Surfaces: CLI `caps audit`, TUI modal on `i`, WebUI new tab 「権限」 | operator |
| D6 | Default shows live tasks only; a toggle (`--all` on the CLI) adds ended ones | operator (prototype) |
| D7 | Bits with no holder are folded away by default; a toggle shows them | operator (prototype) |
| D8 | Choosing a holder opens that task's existing re-grant UI: TUI authority picker (`OpenRegrant`, the `a` key's), WebUI `openRegrantDialog`. The CLI prints the `caps set` command line instead | this spec |
| D9 | No wire change: built from the task listing (`TaskInfo.capabilities`, `scope`, `overrides`, `creator_task_id`) the surfaces already fetch | this spec (checked: `cli/list.go:525`, `cmd/harness-webui-wasm/main.go:1261,1281`) |
| D10 | One Go builder produces the audit rows; the CLI and TUI call it, the WebUI reaches it through the wasm bridge, so no surface re-derives tiers, ordering or scope colour | this spec (`surface-parity-checklist` item 32) |
| D11 | Visibility is the listing's: an operator sees every task, an agent running `caps audit` sees the tasks its own `ls` shows | this spec |
| D12 | A bit whose reach no task scope bounds — `spawn`, `notify`, `runner_admin`, `board_observe`, `board_send` — shows a fixed 「scope 対象外」 mark instead of a coloured scope. Every other bit shows its own resolved scope. (`exec_resize` was a third case, bounded by the scope of the attach it rode; it is scoped by its own bit since `resizeAllowed`, landed before this feature, so it needs no special treatment.) | operator (after the builder check) |
| D13 | A row lists the tasks whose mask holds **that literal bit** — what a revoke would remove. Bits that a stronger one implies carry a note: `exec_view` (also `exec_cowrite`, `exec_control`), `exec_cowrite` (also `exec_control`), `resume_exact` (also `resume`) | operator (after the builder check) |
| D14 | A holder is labelled `<id8> <agent> <prompt head>`, as the TUI picker's rows are; tasks have no name field | this spec (builder check) |
| D15 | The wasm bridge fetches the task listing itself for each call; it keeps no Go-side snapshot | this spec (builder check) |

## The tier table (D2, D3)

Criteria, so a bit added later can be placed without re-deciding the scheme:

- **強** — creates a new acting principal (`spawn`, `resume`), takes a session
  away from whoever drives it (`exec_control`), destroys what cannot be brought
  back (`purge`; `prune`, which deletes a forgotten task's log file —
  `server/taskstore.go:1288-1294` — and with `--force` forgets live tasks),
  speaks as the operator (`board_send`), administers runners
  (`runner_admin`), opens a network path (`forward_local`, `forward_remote`), or
  reads secrets in transit (`exec_tap`, `forward_tap`).
- **中** — writes into or runs inside another task, or observes other tasks'
  traffic on the board: `exec_cowrite`, `exec_run`, `file_write`, `cancel`,
  `board_peer`, `board_observe`, `resume_exact`.
- **弱** — looks, or notifies: `exec_view`, `exec_resize`, `file_read`,
  `notify`.

Lives in `cli/verb/caps.go` beside `GrantableCaps`:

```go
type CapTier int
const (
	TierStrong CapTier = iota
	TierMid
	TierWeak
)
func CapTierOf(c protocol.Capability) CapTier
func (t CapTier) Label() string   // "強" / "中" / "弱"
func AuditOrder() []protocol.Capability // every granular bit, strongest tier first; within a tier, GrantableCaps order

// How a bit's reach is bounded (D12).
type CapScopeKind int
const (
	ScopeOwn  CapScopeKind = iota // its own resolved scope
	ScopeNone                     // no task scope bounds it
)
func CapScopeKindOf(c protocol.Capability) CapScopeKind
func CapImpliedBy(c protocol.Capability) []protocol.Capability // D13's notes
```

All four live in one table, keyed by bit. `TestEveryGrantableCapHasATier`
fails when a bit reaches `GrantableCaps` without an explicit entry, so a new
bit cannot silently default into a tier or a scope kind.

`CapScopeKindOf` restates a classification the server already makes
(`capTargetClasses`, `server/scope_percap_completeness_test.go`): a bit there
marked `capNoTargetResolution` is `ScopeNone` here. A server test asserts that
agreement (a test-only import of
`cli/verb`), so the two cannot drift silently. A second copy guarded by a test
is weaker than one source, and this is the accepted cost: the server's table
lives in a test file and is keyed by how the gate resolves, not by what a reader
should be shown.

`CapImpliedBy` restates `attachModeCap` and `resumeRequestCap`
(`server/capabilities.go`). It gets the same kind of agreement test, which
checks for each listed bit that `hasAnyCap(implier, <that bit's satisfying
mask>)` holds.

## The audit rows (D4, D9, D10)

```go
// cli/caps_audit.go
type ScopeBreadth int // BreadthGlobal, BreadthSubtree, BreadthNarrow
type AuditHolder struct {
	TaskID, Label, Parent string // Label = "<id8> <agent> <prompt head>" (D14)
	Live      bool
	ScopeKind verb.CapScopeKind // copied from the row's bit
	Scope     string            // ScopeOwn: the bit's resolved scope; ScopeNone: ""
	Breadth   ScopeBreadth      // from the ACTION half of Scope; unset for ScopeNone
	Override  bool              // Scope came from a --scope-for override
	TaskScope string            // the task's base scope, for the expanded view
}
type AuditRow struct {
	Cap       protocol.Capability
	Tier      verb.CapTier
	ImpliedBy []protocol.Capability // D13 note
	Holders   []AuditHolder         // global first, then subtree, then narrow (ScopeNone last); then task id
}
func CapsAudit(tasks []protocol.TaskInfo, includeEnded, includeEmpty bool) []AuditRow
```

- A task is "live" when its status is not terminal (`Succeeded`, `Failed`,
  `Cancelled`).
- `Breadth` reads the action half of a `<visibility>/<action>` scope.
  `global` → global; `subtree`, `descendants`, `subtree-self`, `subtree+ids:…`
  → subtree; `none`, `none-self`, `ids:…` → narrow.
- `Override` is true when an entry in `overrides` names the bit.
- `ScopeNone` renders as 「scope 対象外」 in a neutral grey on every surface.

## Surfaces (D5–D8)

| Surface | What |
| --- | --- |
| CLI | `harness-cli caps audit [--all] [--empty] [--cap NAME] [--json]`, declared in `cli/verb/table.go` beside `caps set` (CLI \| TUI \| WebUI). Text form: one line per bit — `spawn  強  3  3f2a9c supervisor[subtree] 8bd0e4 worker-a[subtree] 70fbad landing[global/global*]`. `--cap NAME` narrows to that bit and prints each holder on its own line followed by the command that would revoke it, `harness-cli caps set <id> --caps <its caps minus NAME>` (`caps set` with `--caps` alone keeps the task's scope). `--json` emits the rows |
| TUI | `i` opens a full-screen modal with the same rows; `enter` on a bit expands its holders; `enter` on a holder opens the authority picker in re-grant mode for that task; `a` toggles ended tasks, `e` empty bits, `esc` closes. The TUI cmdline `caps audit` opens the same modal. Scope breadth uses the TUI's existing warn/ok colours |
| WebUI | new tab 「権限」 with the prototype's layout: per-bit rows, tier badge, count, holder chips coloured by breadth with `*` for overrides, click a bit to expand, click a holder for `openRegrantDialog`; toggles for ended tasks and empty bits; dark theme, usable at 390px. Rows come from a wasm bridge function `harness.capsAudit(includeEnded, includeEmpty)`, which fetches the task listing on each call (D15) |
| WebUI command input | `caps audit` prints the CLI's text form into the command output |
| README | the Capabilities section and the TUI key list |
| Agent skill | `supervising-workers`: one line pointing at `caps audit` for "what did I hand out" |

The prototype the layout came from: `.superpowers/brainstorm/…/audit-proto-b-v3.html`
(scratch, not committed).

## Testing

Unit, `cli/verb`: tier table complete; `AuditOrder` strongest-first and covers
every granular bit.

Unit, `server` (agreement): every `capNoTargetResolution` bit is `ScopeNone`
in `CapScopeKindOf`, and every other bit is `ScopeOwn`. For each `CapImpliedBy` pair, the server's satisfying mask
(`attachModeCap` / `resumeRequestCap`) accepts the implier.

Unit, `cli`: `CapsAudit` over a fixture — holder per bit, ended tasks excluded
by default, empty bits excluded by default, override marked with the
overridden scope, breadth from the action half (`global/subtree` → subtree),
ordering of holders. A `--scope none` holder of `board_send` shows
`ScopeNone`, never a green breadth. An `exec_control`-only task is absent from
the `exec_view` row, and that row's `ImpliedBy` names `exec_control`.

Unit, TUI: `i` opens the modal; `enter` on a holder opens the picker in
re-grant mode for that task; the key table test still passes with `i` bound.

JS: `caps audit` in the command input reaches the bridge.

Live: dummy harness with a few tasks granted different caps and one
`--scope-for` override; CLI output read; TUI modal and WebUI tab (Playwright,
desktop and 390px) checked against the CLI's rows; a holder clicked through to
the re-grant dialog and a bit revoked from there, then gone from the audit.

## Non-goals

- **No automatic warnings.** Rule-based flags ("spawn but no children") were
  prototyped (option C) and not chosen.
- **No history.** The audit shows current grants, not who granted what when.
- **No new authority.** The view reads the listing; revoking goes through the
  existing re-grant paths and their gates.

## Amendment — what shipped (2026-10-19)

Where the build differs from the text above. All of these are **this spec**
calls made during implementation; none was put to the operator.

- **`--json` is CLI and WebUI only.** The TUI answers `caps audit` with its
  modal, which has no JSON form, so the TUI cmdline refuses `--json` rather
  than ignoring it (surface-parity item 33). The § Surfaces CLI row is
  otherwise as written.
- **`--cap NAME` includes unheld bits**, so a bit nobody holds still gets its
  row, with `0 holder(s)`, on all three surfaces. It also narrows the JSON form
  to that bit. One writer, `cli.WriteCapsAuditReport`, does this for the CLI
  verb and the bridge; the TUI opens its modal with unheld bits on.
- **The bridge takes an object and returns the `--json` shape.**
  `harness.capsAudit({includeEnded, includeEmpty})` resolves to rows keyed as
  `caps audit --json` writes them (`implied_by`, `task_id`, `scope_kind`, …),
  built by `cli.AuditRowsJSON`. The command input calls
  `harness.capsAuditText({all, empty, cap, json})`.
- **The TUI audits the whole listing**, not the task table's rows (the 100
  newest). A long-lived task still holding `spawn` is usually the oldest one,
  which is the P1 case.
- **The TUI cursor stays on its line across a refresh.** When a holder above it
  ends, the cursor stays on the same holder. When its own holder or its whole
  bit goes, the cursor moves to a capability line. So enter never opens the
  re-grant picker on a task the cursor was not on.
- **WebUI layout.** Holder chips sit on their own wrapping line under each
  bit's header at every width, not inline beside it as in the prototype. The
  two toggles are checkboxes, like the page's other option rows.
- **Verified live** on a dummy harness against the § Testing setup, on all
  three UIs, including a revoke from the TUI picker and from the WebUI dialog
  that removed the holder from the next audit, and the WebUI at 390px with no
  horizontal overflow.

## Amendment 2 — the audit leads into the tree (2026-10-19)

### Why

The audit ran on the live fleet the day it landed, and it raised two problems.

- **A holder could not be recognised.** Most live tasks are interactive
  sessions with an empty prompt. Their label was `70fbad4a claude` and nothing
  more, so the operator could not tell which task that was or whether it was
  one in use. The operator recognises tasks by where they sit in the creator
  tree and by their repo and status, and the tree views already show those.
- **Choosing a holder jumped straight to re-grant.** The operator wants to see
  first where the task sits, then act on it from there. A separate 「権限」 tab
  also split the audit from the task tree it is about.

### Decisions

| # | Decision | Decided by |
| --- | --- | --- |
| D16 | Choosing a holder no longer opens re-grant. It shows that task in the creator tree: the tree is turned on, the task is brought into view and marked. Re-grant is then reached from the task the usual way: WebUI node → task sheet → 「🔑 caps/scope 再付与」; TUI row → `a`. Supersedes D8 | operator |
| D17 | WebUI: the 「権限」 tab is removed. The audit becomes a collapsible section of the タスク tab, beside the tree. Supersedes D5's WebUI half | operator |
| D18 | The tree marks capabilities two ways: (1) **a chosen bit** — the holders of one selected bit are highlighted; (2) **always** — every task shows its strongest tier and how many bits it holds at that tier, e.g. `強3` | operator |
| D19 | A holder's label also carries its repo basename and status (`70fbad4a claude remote-agent-harness Detached`). The prompt head stays when there is one, shortened to 16 runes | operator (repo + status); this spec (16 runes) |
| D20 | TUI: `enter` on a holder closes the audit, switches the task list to tree order and puts the cursor on that task's row | operator |
| D21 | A bit no task scope bounds (D12's five) renders as **`global（固定）`**, coloured as global (red), and sorts with the global holders, not last. Its reach is the whole server's shared resources — runners, the board, the operator's notifications, any repo a task can be spawned on — which is what global means. 「scope 対象外」 read as "nothing to worry about", the opposite of the truth. 「（固定）」 says no `--scope` / `--scope-for` narrows it, so the label is not mistaken for the scope grammar's `global` value or for a misconfigured `--scope none` task. Supersedes D12's grey 「scope 対象外」 on every surface; `ScopeKind` / `scope_kind: "none"` in the data are unchanged | operator |
| D22 | The tree's marks (D18 1 and 2) cover ended tasks too, from each task's own recorded mask. A resume brings the recorded caps back, so a bit left on an ended task is a live risk. The count on an audit row (live only by default, D6) can therefore be smaller than the number of nodes marked for that bit; the two answer different questions | operator |

### How each surface does it (this spec's calls unless marked)

**WebUI**

- `<details id="caps-audit">` 「権限の棚卸し」 sits in the タスク tab directly
  under the tree graph. Opening it loads the rows. While it is open, the rows
  reload on each snapshot. The rows, chips and toggles are the ones the tab
  had.
- A bit selector, 「権限で強調」, sits beside 🌳 ツリー. Its first entry is
  「なし」, then every bit in `AuditOrder`. While a bit is chosen, holder nodes
  get a ring and the other nodes are dimmed (D18-1). It acts only while the
  tree is shown.
- Every node draws its tier mark, `強3` / `中2` / `弱1`, in the tier's colour,
  beside the id label. A task holding no bit draws nothing (D18-2). The mark
  arrives rendered on each snapshot task row (`tierMark`, `tierClass`, from
  one Go function the TUI also calls), and the selector's bit list comes from
  `harness.capsAuditOrder()`, so the page holds no copy of the tier table.
- Clicking a holder chip does four things (D16):
  1. turns the tree on if it is off;
  2. sets 「権限で強調」 to that chip's bit;
  3. scrolls the node into view;
  4. pulses it for about two seconds.

  Clicking the node then opens its task sheet, as it already does.

**TUI**

- `enter` on a holder does the D20 jump.
- `t` on a bit line closes the audit, switches to tree order, and marks that
  bit's holders with `◆` in the ID cell; the header reads `ID (◆ spawn)`.
  Pressing `t` on another bit replaces the mark, and `T` (the tree toggle)
  clears it (D18-1).
- **Tasks past the table's 100 newest rows.** The table normally keeps only
  the 100 newest tasks, but the oldest live task is the P1 case. So whatever
  `enter` or `t` points at is shown even when it is older:
  - the jump target, or every holder of the marked bit;
  - together with each one's creator chain, up to a root.

  Without the chain such a task would sit as an orphan `†` row, which shows no
  position, and position is what the jump is for. These pinned rows are
  dropped on the next jump, the next `t`, or `T`.
- In tree order the ID cell ends with the tier mark: `└─ 70fbad4a 強3`. It is
  plain text, because the table's cells cannot carry colour (D18-2).
- The audit modal's footer names `t`.

**CLI**

- Labels gain repo and status (D19). That is the whole CLI change.
- D18 is `omitted` here. Every `ls` row already prints its full `caps=`, and
  `caps audit --cap NAME` is the "holders of one bit" view. A tier mark on
  `ls --tree` would be a third rendering of the same mask on one line.

### Not changed

- The row builder (`cli.CapsAudit`), the tier table, and the `caps audit` verb
  and its flags.
- The TUI audit stays a modal on `i`.

### Testing

- **Unit, `cli`:** the label carries the repo basename (Windows and POSIX
  paths) and status; the prompt head is cut to 16 runes; an empty part is
  dropped.
- **Unit, TUI:**
  - `enter` on a holder → tree order, cursor on that id.
  - A holder older than 100 rows is still reachable, and is drawn under its
    real creator rather than as an orphan.
  - `t` marks a holder older than 100 rows too.
  - `t` marks exactly the holders.
  - `T` clears the mark.
  - The ID cell carries the tier mark in tree order only.
- **JS:** the `caps audit` command input is unchanged.
- **Live, WebUI:**
  - A chip click turns the tree on, selects the bit and brings the node into
    view.
  - A node click opens the sheet.
  - Tier marks are drawn.
  - At 390px there is no overflow.

### What shipped (Amendment 2)

- **Holder scope text.** The holder JSON gained `scope_text`, the scope cell
  exactly as every surface prints it. The WebUI chip shows it, so the page
  never builds `global（固定）` or the `*` itself. `caps audit --json` carries
  the field too.
- **WebUI tier mark.** It sits right of the node, outside the highlight ring,
  vertically centred. Placed above-right, it overlapped the id label; the live
  check measured zero overlaps after the move.
- **TUI tree columns.** The ID column is 28 wide and Repo 18, so the gutter,
  `◆` and the tier mark fit.
- **Verified live** on a dummy harness with these tasks:
  - an ended `spawn` holder;
  - an old-`all`-like task;
  - a supervisor and the child it spawned;
  - a `caps=none` task.

  Results:
  - **CLI:** labels carry repo, status and prompt head, with
    `[global（固定）]`.
  - **TUI:** `enter` landed the cursor on the holder in tree order with the
    tasks pane focused. `t` marked exactly the three `spawn` holders, ended one
    included, and `T` cleared the mark. Tier marks read `強10` / `強1` / `弱1`,
    and none on the `caps=none` task.
  - **WebUI:**
    - with the tree off and the list on Active, a chip turned the tree on,
      selected `spawn` and ringed the three holders, the rest dimmed;
    - the node opened its sheet with 🔑;
    - at 390px no overflow.
