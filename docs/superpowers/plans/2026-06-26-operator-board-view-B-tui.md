# Operator Board View — Plan B (TUI) Implementation Plan

> Single cohesive deliverable (one modal + Do* cmds + events + app wiring + tests), implemented as one reviewed task. Spec: `docs/superpowers/specs/2026-06-26-operator-board-inbox-view-design.md`. Builds on Plan A's `cli.Client` board methods (landed).

**Goal:** Add a TUI "board" modal: list agentboard topics → drill into a selected topic's messages (with content) → keys to purge whole-topic or a single message.

**Architecture:** A modal overlay mirroring `tui/conns.go`'s `ConnsModal` (opened by a hotkey, two internal modes: topic-list / message-drilldown), driven by on-demand `Do*` `tea.Cmd`s like `DoConnSnapshot`, calling the long-lived `*cli.Client` board methods. No periodic poll (the TUI has none); refresh on open + a manual `r` key.

## Global Constraints
- Reuse the long-lived `a.client *cli.Client` (tui/app.go:71) via `Do*` cmds — same shape as `DoConnSnapshot` (tui/conns.go:24). NEVER dial fresh.
- Board methods: `(*Client).BoardTopics(ctx) ([]cli.BoardTopicRow, error)`, `BoardRead(ctx, topic) ([]cli.BoardMessage, bool, error)`, `BoardPurge(ctx, topic, seq) (purged int, found bool, err error)`. Types: `cli.BoardTopicRow{Name,LastSeq,LastPublishedAtMs,MsgCount}`, `cli.BoardMessage{Seq,FromTaskHex,FromHostname,ReceivedAtMs,Payload}`. `seq==0` purges whole topic.
- Work in THIS worktree (…/.harness-worktrees/0f0d4dd6…), branch `harness/0f0d4dd6b7d3b64354cf4ff249b87403`. Verify with `go build ./...`, `make vet`, `go test ./tui/`. No bare `go build ./cmd/x`.
- Mirror existing patterns exactly (don't invent): `ConnsModal` (tui/conns.go) for the table-based list modal; `LogsModel` (tui/logs.go) for the scrollable `viewport.Model` used to show message content; `DoConnSnapshot`/`DoCancel` (tui/client.go) for the cmd shape; `conns_test.go` for model-level tests.

## Task: TUI board modal (single reviewed task)

**Files:**
- Create `tui/board.go`: `BoardModal` struct with two modes (`boardTopics` / `boardMessages`). Fields: `open bool`, `mode`, `topicsTable table.Model` (cols Topic|Msgs|LastSeq|LastAt), `rowTopics []cli.BoardTopicRow`, `curTopic string`, `msgs []cli.BoardMessage`, `msgCursor int`, `content viewport.Model` (payload of the selected message, pretty-printed if valid JSON), `status string`. Methods mirroring `ConnsModal`: `NewBoardModal()`, `IsOpen()/Open()/Close()`, `SetSize(w,h)`, `ApplyTopics([]cli.BoardTopicRow)`, `ApplyMessages(topic, []cli.BoardMessage, found bool)`, `Update(msg) (BoardModal, tea.Cmd)`, `View() string`. Key handling in `Update`:
  - topic mode: ↑/↓ → `topicsTable.Update`; `Enter` → emit a cmd to drill (caller dispatches `DoBoardRead`); `r` → refresh cmd (`DoBoardTopics`); `x` → purge whole selected topic (`DoBoardPurge(topic,0)`); `Esc` → close.
  - message mode: ↑/↓ → move `msgCursor`, update `content` viewport with that message's payload; PgUp/PgDn → `content.Update`; `X` → purge selected message (`DoBoardPurge(topic, msgs[cursor].Seq)`); `r` → re-read (`DoBoardRead(topic)`); `Esc` → back to topic mode.
  Because `Update` needs to return cmds that reference `a.client`, follow the ConnsModal convention: the modal's `Update` returns a small intent (or the App's key handler dispatches the `Do*` cmd directly — match whichever ConnsModal uses; ConnsModal forwards keys and the App kicks `DoConnSnapshot`. Prefer: App-level key handling kicks the Do* cmds, modal holds state + table/viewport).
- Add to `tui/client.go`: `DoBoardTopics(c *cli.Client) tea.Cmd`, `DoBoardRead(c *cli.Client, topic string) tea.Cmd`, `DoBoardPurge(c *cli.Client, topic string, seq uint64) tea.Cmd` — each a `context.WithTimeout(…,10–15s)` closure returning a typed msg. Mirror `DoConnSnapshot` exactly.
- Add to `tui/events.go` (or wherever ConnSnapshotMsg lives): `BoardTopicsMsg{Rows []cli.BoardTopicRow; Err error}`, `BoardReadMsg{Topic string; Msgs []cli.BoardMessage; Found bool; Err error}`, `BoardPurgeMsg{Topic string; Seq uint64; Purged int; Found bool; Err error}`.
- Modify `tui/app.go`: (1) add `boardModal BoardModal` field + init in the App constructor; (2) in `Update`, add `case BoardTopicsMsg/BoardReadMsg/BoardPurgeMsg` (apply to modal; on purge success, re-kick `DoBoardTopics` or `DoBoardRead` to refresh; set status on error); (3) add an `if a.boardModal.IsOpen()` guard block at the same level as the `connsModal` guard (route keys to the modal + dispatch the Do* cmds for Enter/r/x/X); (4) add a global hotkey to open it (pick a free key — `B` per the map; verify it's unused) that opens the modal and kicks `DoBoardTopics(a.client)`; (5) render it in `View()` via `lipgloss.Place(...)` like the other modals; (6) add a footer hint.
- Create `tui/board_test.go` (mirror `conns_test.go`): `TestBoardModalApplyTopics` (ApplyTopics → rowTopics len + table rows); `TestBoardModalDrillAndPop` (enter message mode via ApplyMessages, Esc pops to topics, Esc again closes); `TestBoardModalContentFormatsJSON` (a message with a JSON payload renders pretty-printed in the content viewport).

**Steps (TDD):**
- [ ] Read the mirror targets: `tui/conns.go`, `tui/logs.go` (viewport usage), `tui/client.go` (Do* + ConnSnapshotMsg), `tui/app.go` (the `connsModal` guard + `'C'` open key + View Place + footer), `tui/conns_test.go`.
- [ ] Write `tui/board_test.go` with the three tests above (they will not compile until BoardModal exists — that's the codegen-like ordering; write them, then implement to satisfy).
- [ ] Implement `tui/board.go` + the `tui/client.go` cmds + `tui/events.go` msgs + `tui/app.go` wiring.
- [ ] `go build ./...` && `go test ./tui/` until green; `make vet`.
- [ ] Confirm the open key (`B`) doesn't collide with an existing binding (grep the app.go key switch); if taken, pick another free key and note it.
- [ ] Commit: `feat(tui): agentboard board view (topics → messages w/ content → purge)`.

## Verification (controller, post-implement)
Resume a bash-runner or drive harness-tui to confirm the modal opens, lists topics, drills into a topic showing content, and purge works. (TUI input verification per memory: feed real keys, not just render.) Land via Mode A FF + `make build`.
