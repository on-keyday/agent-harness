# Board membership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator, or a task holding the new bit `board_member` within bounds, subscribe another task to a topic or make it a member (subscribed + read/write on that one topic, replies staying on the topic).

**Architecture:** `agentboard.Board` gains a task-id-keyed membership set consulted by the reach checks, `RegisterTask`, `Unsubscribe` and `Send` (reply-route default), persisted for retained topics through the existing `Store`. One wire kind `board_membership` (op = subscribe/unsubscribe/join/leave) gated on `board_member`, with the task-caller bounds in its handler. CLI verbs, TUI keys and WebUI controls sit on the subscribers views.

**Tech Stack:** Go; `runner/protocol/message.bgn` → `make protoregen ARGS='runner/protocol/message.bgn'`; verb table → `go generate ./cli/verb`; WebUI `webui/static/main.js` + wasm bridge `cmd/harness-webui-wasm/main.go` (`make webui-build`).

**Spec:** `docs/superpowers/specs/2026-10-31-board-membership-design.md`

## Global Constraints

- `board_member = 0x2000000`, appended, default-off; `all` becomes `0x3ffffff`; a task the WAL recorded with `0x1ffffff` must not gain it on replay.
- `join` refuses `chat.*` and empty names; on `tree.<X>.*` only a target already in X's lineage (`Board.member`); otherwise `not_joinable`.
- `subscribe` grants nothing: the target must pass `readableFor` itself; `chat.operator` refused as `agent subscribe` refuses it.
- Task caller (has a principal): target must be in its `board_member` scope (`h.inScope(cid, Capability_BoardMember, targetHex)`), else `not_found`; `join` also needs `PublishableBy(caller, topic)`, else `not_publishable`. Operator caller: namespace rules only.
- Membership: per (task id, topic) at board level; `join` = membership + subscription; `leave` = both; a member's own `agent unsubscribe` also leaves.
- A member's publish to that topic with no `--reply-to` gets `reply_to_topic = topic`; an explicit `--reply-to` wins; non-members unchanged.
- Retained topic: membership persisted (`member` records) and restored before tasks register; operator-made subscriptions are never persisted.
- Status names: `ok`, `not_found`, `not_readable`, `not_joinable`, `not_publishable` (snake_case labels in the `.bgn` enum).
- Tests that dial a server: `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.
- Skill text: edit `runner/agentskills/…/SKILL.md`, copy byte-identically to `.claude/skills/` and `.agents/skills/`.
- Every surface is driven live once before done (memory: a live check of one surface does not cover another).

## Review Focus

1. **A target that is not running** (finished, or never assigned) — subscribe answers `not_found` rather than recording a subscription nobody holds; join still records membership, applied when the task registers. Test in Task 2.
2. **Resume on another runner** — two `taskState`s under one task id: join/subscribe reach both. Test in Task 2.
3. **Membership on a retained topic that is then purged or cleared** — `PurgeTopic` keeps it (retention stays), `ClearRetain`/expiry drop the persisted record but keep the in-memory one until restart. Test in Task 2.
4. **An explicit `--reply-to` from a member** (broadcast request, unicast response) — kept, not overridden by the default. Test in Task 2.
5. **Old client against the new server** — `board subscribers` rows grow (`joined`); old clients fail to decode (no shim, as the retention change); note in the landing message to update the server first. No test; recorded.

---

### Task 1: `board_member` bit

**Files:** `runner/protocol/message.bgn` (enum entry with reasoning comment after `board_persist`; `all = 0x3ffffff`), regen; `cli/verb/caps.go` (ordering after `BoardPersist`, `CapDescription`, `capAuditTable` `{tier: TierMid}` — task-scoped, so NOT `ScopeNone`); completeness tests (`runner/protocol/capability_test.go`, `server/scope_percap_completeness_test.go` — classify as resolved through the target task, like `BoardContactsClear`'s use of `BoardPurge`; `cli/verb/caps_audit_table_test.go` 26 bits); `README.md` capability list; `server/wal_test.go`.

- [ ] Test `TestWALReplay_OldAllMaskLacksBoardMember`: replay `capabilities: 0x1ffffff` → no `Capability_BoardMember`, still `Capability_BoardPersist`. Run → FAIL (undefined).
- [ ] Enum + regen + tables until `env -u … go test ./runner/protocol/ ./cli/... ./server/` → PASS.
- [ ] Surface-parity walk logged in `firing-log.md`; commit `protocol: board_member capability bit`.

### Task 2: Board membership (agentboard)

**Files:** Create `agentboard/membership.go`; modify `agentboard/reach.go` (`readableFor`, `publishableFor`), `agentboard/board.go` (`RegisterTask`, `Unsubscribe`, `Send`, `ListSubscribers`), `agentboard/store.go` + `filestore.go` (member records), `agentboard/retention.go` (`AttachStore` restore); test `agentboard/membership_test.go`.

**Produces:**
```go
var ErrNotJoinable = errors.New("agentboard: topic cannot be joined")   // empty, chat.*, tree outside lineage
var ErrNotRunning  = errors.New("agentboard: task holds no board state") // subscribe on a task with no taskState
func (b *Board) SubscribeTask(tid protocol.TaskID, topic string) error   // readableFor(tid) else ErrNotPermitted; chat.operator → ErrReservedTopic
func (b *Board) UnsubscribeTask(tid protocol.TaskID, topic string)
func (b *Board) Join(tid protocol.TaskID, topic string) error            // ErrNotJoinable; records + subscribes every taskState of tid
func (b *Board) Leave(tid protocol.TaskID, topic string)                 // removes membership + subscription
func (b *Board) IsMember(tid protocol.TaskID, topic string) bool
// Store: SetMember(topic string, task protocol.TaskID, on bool) error; StoredTopic.Members []protocol.TaskID
// SubscriberPattern gains Joined bool
```
Membership map is guarded by `b.mu`; the reach checks read it through a helper that takes `b.mu` (they are called without it — see `readableTargets`' lock note). `Unsubscribe(c, p)` calls `Leave` when `IsMember`. `Send`: if `cfg.replyToTopic == ""` and `IsMember(fromTid, topicName)` → `cfg.replyToTopic = topicName`.

- [ ] Tests (each RED first): `TestMembership_JoinGrantsReadWriteOnOneTopic` (no-bit task, free-form `grp`: `Publishable`/`Readable` true for `grp`, false for `other`), `TestMembership_RefusesChatAndForeignTree` (`chat.x`, `chat.operator`, `""`, `tree.<other>.d` → `ErrNotJoinable`; own-lineage tree → ok), `TestMembership_SubscribeTaskGrantsNothing` (readable free-form → subscribed, `Publishable` still false; another task's `chat.<id>` → `ErrNotPermitted`), `TestMembership_NotRunning` (Review Focus 1: no taskState → `SubscribeTask` = `ErrNotRunning`; `Join` ok and applied by a later `RegisterTask`), `TestMembership_BothRunnerStates` (Review Focus 2), `TestMembership_SelfUnsubscribeLeaves`, `TestMembership_ReplyRouteDefault` (member send → `ReplyToTopic == "grp"`; explicit `WithReplyTo(SelfTopic(tid))` kept — Review Focus 4; non-member → empty), `TestMembership_RetainedSurvivesRestart` (FileStore; join, close, new board `AttachStore` + `RegisterTask` → member & subscribed; a non-retained topic's membership gone), `TestMembership_PurgeKeepsClearDrops` (Review Focus 3), `TestListSubscribers_MarksJoined`.
- [ ] Implement; `go test -race ./agentboard/` → PASS; commit `agentboard: task membership of a topic`.

### Task 3: Wire + server handler

**Files:** `runner/protocol/message.bgn` (`TaskControlKind.board_membership` appended + union arms; `enum BoardMembershipOp: subscribe, unsubscribe, join, leave`; `format BoardMembershipRequest {task :TaskID, topic_len, topic, op}`; `enum BoardMembershipStatus` with labels; `format BoardMembershipResponse {status}`; `SubscriptionPattern` gains `joined :u1, reserved :u7` — read its current trailing fields first and append), regen; `server/capabilities.go` (`requiredCap[BoardMembership] = Capability_BoardMember`), `server/task_handler.go` (dispatch, passing `cid`), `server/board_handler.go` (`handleBoardMembership`, subscribers row `Joined`), completeness tests (new last kind); test `server/board_membership_test.go`.

`handleBoardMembership(conn, requestID, cid, r)`: target must exist in `h.Tasks` (else `not_found`); if the caller has a principal, `h.inScope(cid, Capability_BoardMember, targetHex)` else `not_found`, and for `join` `h.Board.PublishableBy(principal, topic)` else `not_publishable`; then map `ErrNotJoinable`→`not_joinable`, `ErrNotPermitted`/`ErrReservedTopic`→`not_readable`, `ErrNotRunning`→`not_found`.

- [ ] Tests RED→GREEN: operator subscribe/join/leave ok; task without bit → `PermissionDenied` naming `board_member`; task with bit, target outside scope → `not_found`; join free-form without global `board_peer` → `not_publishable`; join own `tree.<self>.sns` for its child → ok; subscribe child to an unreadable topic → `not_readable`; `board subscribers` body marks `joined`.
- [ ] `env -u … go test ./server/ ./runner/protocol/`; `scripts/wire-skew-check.sh` after commit; commit `server: board_membership`.

### Task 4: CLI

**Files:** `cli/verb/table.go` (four rows under `board`: `Args` task (`ArgTaskID`) + topic (`ArgTopic`); notes name the bit and the bounds; `ModalSurfaces` TUI/WebUI added in Tasks 5/6), regen; `cli/board.go` (`func (c *Client) BoardMembership(ctx, taskHex, topic string, op protocol.BoardMembershipOp) error` returning the status name as error when not ok, plus the fresh-dial wrapper); `cli/cmd_board.go` (output `{"status":"ok","task":"<id8>","topic":…,"op":"join"}`; subscribers text marks `joined:` before a member's topic, `--json` `joined:true`); `cmd/harness-cli/dispatch.go`; test `cli/board_membership_e2e_test.go` via `RunBoardSubcmd`.

- [ ] Test through argv: join → subscribers line shows `joined:grp`; leave → gone; `subscribe` of a chat topic of another task → exit 1 `not_readable`; flags after positionals parsed (permuted). RED → implement → GREEN; commit `cli: board subscribe/unsubscribe/join/leave`.

### Task 5: TUI

**Files:** `tui/keys.go` (`BoardSubscribeTask "S"`, `BoardUnsubscribeTask "U"`, `BoardJoinTask "J"`, `BoardLeaveTask "L"` — none a scroll key), `tui/board.go` / new `tui/board_membership.go` (task-id editor resolving a unique prefix against the App's task snapshot — ambiguous or none → status error; `U`/`L` act on the highlighted task line of the subscribers view; member lines show `joined`; compose's initial reply-to is the open topic when its subscriber rows contain a member, else `chat.operator`), `tui/overlays.go`, `tui/app.go` (result → status + `DoBoardSubscribers` refresh); tests in `tui/board_membership_test.go` through `App.updateKey`.

- [ ] Tests RED→GREEN: `J` + prefix + Enter → request cmd; ambiguous prefix → status, no cmd; `L` on a member line → cmd; `L` on a non-member line → status "not a member"; compose on a topic with a member starts reply-to at the topic.
- [ ] **Live**: harness-tui in a dummy PTY (dummy-harness skill): join a bash task to `grp`, see `joined`, leave it. Commit `tui: board membership keys`.

### Task 6: WebUI

**Files:** `cmd/harness-webui-wasm/main.go` (`harness.boardMembership(taskId, topic, op)`), `webui/static/main.js` (subscribers panel: "購読させる" / "参加させる" opening the task checklist used by the spawn form (`buildTaskChecklist`) in a `<dialog class="picker-modal …">`; per task row "購読を外す" or, for a member, "外す" (leave); `joined` marker; composer reply-to starts at the topic when a member exists), `webui/index.html`, `webui/static/style.css` (dark palette; 390px), `webui/static/board_membership_test.mjs` (+ `Makefile` js-test list).

- [ ] Node tests RED→GREEN: bridge exposes `boardMembership` and rejects without a client; pure helpers for the row's button label and the composer default (`boardComposeReplyDefault(topic, subscriberRows)`).
- [ ] **Live**: Playwright at 1280 and 390 against a dummy: join a task from the dialog, marker appears, leave, composer default. Commit `webui: board membership`.

### Task 7: Skill text and end-to-end

- [ ] `harness-cli` skill: "Joined topics" — the operator or a `board_member` holder may have subscribed/joined you; on a joined topic your posts' replies stay on it (send `--reply-to chat.<your id>` for a private answer); on any group topic you both send to and read, send with `--exclude-self`; `agent unsubscribe` leaves. `supervising-workers` skill: `board join` for gathering participants, the bounds. Mirrors identical (`go test ./runner/agentskills/`).
- [ ] Dummy harness: a bash task with `board_member` joins its child to `tree.<self>.sns`; the child (no bits) posts there and a reply defaults back to the topic; CLI `board subscribers` shows `joined`.
- [ ] `env -u … make test`, `make vet`, `make build`; commit `skill: joined topics`.
