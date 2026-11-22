# Request → capability: one source for the bits a request needs — Design

Date: 2026-11-22

The server decides which capability bits a `TaskControlRequest` needs in two
kinds of place: the `requiredCap` map in `server/capabilities.go` (kind → one
bit, checked centrally before dispatch) and about a dozen checks written inline
in the dispatch cases and handlers, because the map is keyed on the kind alone
and cannot see the request's fields. This design moves the DECISION of which
bits — everything that is a pure function of the request, or of a value the
handler has just looked up — into package `authority`, and has every server
site read it from there. Where and in what order the server checks stays
exactly as it is.

## 1. Problem

- **Anything outside the server that needs to know "what does this call
  need" has to copy it.** The trigger was the MCP surface
  (`2026-11-22-harness-cli-mcp-surface-design.md` §8): its `--caps` filter and
  its caller-caps filter of `tools/list` were specified as a `VerbSpec.Cap`
  declared per verb, which is the product of `requiredCap` and the inline checks
  written down a second time. Such a copy drifts, and the harmful direction of
  the drift — a tool the caller could use is hidden — fails silently: a model
  does not call a tool it was not shown. There is nothing a copy can be kept
  honest against, because the server's knowledge is not in one place. (That
  §8 belongs to the MCP spec's second-pass revision, which has not landed; on
  `main` at `0b5a069d` the caps filter is still an open question, §12 there.
  The MCP work was paused so this lands first.)
- **The server's own knowledge is scattered by an artefact of a type, not by
  the features.** The inline checks say so themselves: "requiredCap cannot see
  AttachMode" (`task_handler.go:960`), "told apart by list_only, which
  requiredCap cannot see" (`restore_handler.go:80`), direction-dependent
  (`capabilities.go:13-21`). The same rule is written at several sites with
  different answer shapes (§3.3).

## 2. Goals, non-goals, anti-goals

Goals:

- One function in `authority` answers, for any request, which bits it needs
  insofar as the request determines them, and names the parts that a lookup
  decides.
- Every server site that today computes a bit takes it from `authority`
  instead — the central gate included, and the bit passed to `inScope` /
  `scopeSet` / `scopeSetWith` / `authorize` to decide which bit's SCOPE binds
  a target as well as the bit that is checked. A check bit and a scope bit
  from two sources can drift apart, which is the cap-for-the-wrong-direction
  shape `task_handler.go:878-880` was rewritten to prevent. (Decided: operator,
  2026-11-22.)
- A new `TaskControlKind` cannot be added without a decision in that function:
  a test enumerates every kind.

Non-goals:

- **Moving checks.** Every check stays where it is today, in the same order,
  with the same answer (PermissionDenied with the same `RequiredCap`, or the
  kind's own NotFound / status). The ordering is load-bearing in several places
  (§3.2) and is not this change's subject. (Decided: operator, 2026-11-22 —
  approach A, §4.)
- **Fixing the denial labels.** Several denials report a bit that is not what
  was checked (§3.4). They are recorded, not changed.
- **Board reach.** Whether a task may read or publish to a topic is decided by
  `agentboard/reach.go` (`readableFor`, `publishableFor`), which is already one
  implementation. `Need` names reach as a later requirement and does not
  restate it.
- **The MCP surface.** It will consume this; it is not built here.
- **`cli/verb/caps.go`'s description prose**, which names verbs per capability
  in English — a separate copy, out of scope.

Anti-goals — this fails even if it compiles and the suite is green when:

- any request is accepted or refused differently from before, or answered with
  a different status or `RequiredCap`;
- a check moves ahead of a visibility or reach check it currently follows (an
  existence oracle reopened);
- the function becomes a second place the bits are written, i.e. some server
  site still computes a bit by itself alongside it.

## 3. How it works today

The full per-kind survey is the appendix (§9). Summary of its 89 kinds (counted
from the appendix rows, one per kind):

| category | count | meaning |
|---|---|---|
| MAP | 35 | `requiredCap` alone decides the bit (plus, often, `inScope` on the same bit) |
| REQ | 7 | the bits follow from request fields: AttachSession (mode), OpenFileTransfer (direction), RegisterPortForward (direction), RecordSwitch (on/off), RestoreTasks (list_only), ScheduleCreate (task / record fields), OpenPortForward (constant, but written inline) |
| EITHER | 1 | ListFiles: file_read or file_write |
| LOOKUP | 16 | part of the decision needs server state: OpenInteractive, AwaitIdle, AwaitProgress, KillPortForward, ExecRunKill, RecordKeep, SessionMode, ScheduleEdit, ScheduleRun, BoardRetain, BoardFlush, BoardArchive, BoardMembership, AgentSend, AgentSubscribe, AgentWait |
| IDENTITY | 3 | SetCaps, TaskRename (operator, or a task that holds resume over a strict descendant: `regrantReaches`), SetParent (operator only) |
| NONE | 23 | no bit; visibility or ownership filters the answer |
| RETIRED | 2 | Submit, GetTaskLog |
| NOTREQUEST | 2 | ClientHello (handshake), PermissionDenied (response only) |

### 3.1 Request-determined parts hidden inside LOOKUP kinds

Several LOOKUP kinds are mostly request-determined, and only one clause needs
the lookup. OpenInteractive: spawn or resume (`resumeRequestCap`, from the
resume id and the exact flag), AND exec_control unless detached, AND
record_persist when the request asks to record — all request fields; only
"schedule_edit when a foreground resume unbinds a schedule" needs the task's
binding. AwaitIdle/AwaitProgress: notify when sink=notify is request-determined;
the board sink's reach is not.

### 3.2 Ordering that must not move

- KillPortForward: visibility first, then `forwardCapFor(pf.direction)`
  (`task_handler.go:911-914`).
- ExecRunKill: missing bit or out of scope both answer NotFound
  (`exec_run.go:415-422`).
- RecordKeep: reach before the retention lookup (`record_handlers.go:174-177`).
- OpenInteractive: the schedule_edit bit only after `resumeTargetAllowed`
  (`task_handler.go:638-642`).
- ScheduleEdit/Run: power after reach and self (`schedule_handler.go:688-689`).
- AwaitIdle/AwaitProgress: visibility before any bit.

### 3.3 One rule, several sites

- "Unbinding a schedule needs schedule_edit" (S-D15): `task_handler.go:643`
  (PermissionDenied), `task_handler.go:1786` (re-check, ScopeNotPermitted),
  `session_mode.go:54-56` (PermissionDenied).
- "Recording needs record_persist": `record_wiring.go:28` with
  `task_handler.go:1835` (RecordNotPermitted), `schedule_handler.go:584-587`
  (RecordNotPermitted), `record_handlers.go:216-220` (PermissionDenied).
- notify for a notify sink: `capabilities.go:68`, `await_idle_handler.go:92`,
  `await_progress_handler.go:55`.
- The spawn bit: `resumeRequestCap` (`capabilities.go:133`) and direct
  `hasCap(Spawn)` in `schedule_handler.go:705, 740`.
- schedulePower vs scheduleRunPower (`schedule_handler.go:705-718` vs
  `736-752`) look alike and DIFFER on purpose — edit needs `resume`, run
  accepts `resume|resume_exact` (comments at 684-688, 731-735). They stay two
  functions.

### 3.4 Reported bit ≠ checked bit (recorded, not changed)

- ListFiles checks file_read OR file_write, reports file_read.
- RecordKeep's pre-gate checks persist|delete, reports record_persist.
- AgentSend and the board sinks of AwaitIdle/AwaitProgress report board_peer
  for a `publishableFor` refusal, including a `tree.<X>` topic no bit opens.
- AgentSubscribe/AgentWait report board_observe for a `readableFor` refusal,
  including "not a member of tree.<X>".

## 4. Approaches

| | what | trade-off |
|---|---|---|
| **A (chosen)** | Keep every check where it is; replace the expression that computes the bit with a call into `authority`. | Drift is closed for everything a request or a looked-up value determines. The server's shape — central gate plus inline checks — stays visible as it is. |
| B | Make the central gate check every request-determined clause for every kind; inline sites keep only LOOKUP/IDENTITY parts. | Reads as one layer for MAP/REQ/EITHER kinds, but re-derives the check-vs-scope order per kind, and the LOOKUP/IDENTITY kinds keep inline checks anyway — it is never truly one layer. Deferred: worth judging after A, on what remains inline. |
| C | Declare the bits per CLI verb (`VerbSpec.Cap`) and pin them with an agreement test. | The MCP spec's original §8. A copy of the server's knowledge; the test can only see single-kind verbs in the map, and the verb → kind table it needs is itself hand-written. Rejected: it is what this design exists to replace. |

Decided-by: A was chosen by the operator on 2026-11-22. B is not rejected, only
deferred.

## 5. Design

### 5.1 The function

In `authority` (a new file, e.g. `authority/need.go`):

```go
// Clause is one requirement: any ONE of AnyOf satisfies it. A Need's clauses
// all have to hold (an AND of ORs), because a request can need two unrelated
// powers at once -- an attached open needs spawn AND exec_control.
type Clause struct {
	AnyOf  protocol.Capability
	Report protocol.Capability // what PermissionDenied names when this clause fails
}

// Later is a requirement the request alone cannot decide; the handler decides
// it after a lookup. Possible is a FIXED per-kind value: every bit this
// requirement can come to need, never computed from the request (for board
// reach that would mean restating agentboard's topic grammar). Report is what
// the server names when it refuses on this requirement -- including the
// labels of §3.4, which are recorded here rather than allowlisted.
type Later struct {
	What     string             // "forward direction", "schedule binding", "board reach", ...
	Possible protocol.Capability
	Report   protocol.Capability
}

type NeedClass uint8 // Bits, None, Identity, Retired, NotRequest

type Need struct {
	Class   NeedClass
	Clauses []Clause // request-determined, in the order the server checks them
	Later   []Later
}

// RequestNeed is the single answer to "what does this request need".
func RequestNeed(req *protocol.TaskControlRequest) Need

// ScopeBit is which bit's scope binds a target, given what the caller holds,
// for every kind that checks a target. Separate from the clauses because the
// two differ: ListFiles is satisfied by either bit and scoped through the one
// held (file_read first); AttachSession is satisfied by AttachModeCap(mode)
// and scoped by AttachModeScopeCap(mode); ScheduleCreate is gated on
// schedule_edit and scopes its bind target by resume. Where the server walks
// several bits' scopes (a resume: each resume bit held; RecordKeep: persist and
// delete), ScopeBit returns the SET to walk and the walk stays in the handler.
// Scopes decided after a lookup (the bound schedule's record_persist) come from
// the §5.2 helpers.
func ScopeBit(req *protocol.TaskControlRequest, held protocol.Capability) protocol.Capability

// Possible is, per KIND, every bit some request of that kind could need: the
// union over the request-determined branches and the Later.Possible values.
// It is what a consumer with no request in hand (a tool list) reads. A zero-
// bodied request is NOT a substitute: it selects one branch (a zero
// FileTransferDirection is Push, so it would answer file_write and hide
// `file pull` from a file_read holder -- the silent direction §1 is about).
// A consumer that hides on Possible hides only when the caller holds none of
// its bits, so an imprecise union can only show too much, never too little.
func Possible(kind protocol.TaskControlKind) protocol.Capability
```

`Possible` is derived, not written beside the branches: a test enumerates, for
each REQ kind, every value of the deciding field (direction, mode, on/off,
list_only, sink, detached, resume id zero/non-zero, exact) and asserts each
`RequestNeed` clause's bits ⊆ `Possible(kind)`. How the union is computed so it
cannot fall behind a new branch is the builder's (§7). (Author's call: the
consumer side needs an argument-free answer, and building it from the same
branches is the only way that answer is not a second copy.)

`Need` does not encode the order between its clauses and its `Later`
requirements — inside OpenInteractive the schedule_edit lookup sits between
spawn/resume and exec_control. Order is the server's, unchanged (§2), and no
consumer needs it.

Which kinds the central gate checks today is also part of `authority`'s answer
(e.g. a `Central` marker on the clause, or a `CentralCap(kind)` beside
`RequestNeed`), so the gate's kind set is not a second list.

Names and exact field set are the builder's (§7); the semantic requirements
are fixed:

- an AND of ORs, in server check order;
- a per-clause reported bit equal to what the server reports today, quirks
  included (§3.4);
- LOOKUP parts named with their possible bits, never guessed into a clause;
- every `TaskControlKind` classified — an unknown kind is a programming error
  the exhaustiveness test catches, not a silent "needs nothing".

### 5.2 The lookup-decided helpers

Where the bit depends on a value the handler has just looked up, the mapping
from that value to the bit is ALSO a pure function and moves to `authority`, so
the handler looks up and `authority` decides:

| helper (name illustrative) | input | today at |
|---|---|---|
| `ForwardCap(dir)` | the forward's direction | `server/port_forward.go:335` (`forwardCapFor`) — moved |
| `RecordKeepCap(current, requested)` | the record's retention and the requested one | `record_handlers.go:188-200` |
| `UnbindCap` | — (the bit for S-D15) | `task_handler.go:643`, `:1786`, `session_mode.go:55` |
| `RecordOnCap` | — (the bit for recording) | `record_wiring.go:28`, `schedule_handler.go:584`, `record_handlers.go:216` |
| `SchedulePower(state, bound, specTask)` and `ScheduleRunPower(state)` | the schedule's state and binding | `schedule_handler.go:700-728`, `736-752` — two functions, as today |

ExecRunKill is not in this table: its bit is the constant exec_run, so it is a
Clause; only its TARGET (the exec's task) comes from the lookup.

Each of these is called by the existing site; the site keeps its lookup, its
order and its answer shape (the three `RecordOnCap` sites keep their two
different answers — unifying them is a behaviour change, §2).

Reach (`readableFor` / `publishableFor`) and the identity rules
(`regrantReaches`, operator-only SetParent) are already single implementations;
`RequestNeed` names them as `Later` / `Identity` and they do not move.

### 5.3 The server

- `requiredCap` moves into `authority` (it is `RequestNeed`'s MAP case). The
  central gate (`task_handler.go:525`) asks `authority` for the same kinds it
  gates today, with the same bit — still before dispatch, still only for those
  kinds.
- Each inline site replaces its bit expression with the matching clause (or
  helper). The `if !hasCap… { deny(...); return }` lines stay where they are.
- Each scope check (`inScope`, `scopeSet`, `scopeSetWith`, `authorize`) takes
  its bit from `ScopeBit` or a §5.2 helper. The builder's survey counted at
  least 25 such sites passing a constant today (e.g. `task_handler.go:564, 589,
  772, 798, 865`, `session_mode.go:33`, `progress.go:105`,
  `schedule_handler.go:277, 287, 432, 587, 722, 1276`, `board_handler.go:219,
  438`, `forward_tap_handler.go:43`, `exec_tap_handler.go:31`,
  `transcript.go:54`, `credential_handler.go:165`, `record_handlers.go:75, 179`,
  `restore_handler.go:132-134`, `capabilities.go:193`); the go/ast guard of
  §5.4 is what finds the rest.
- `resumeRequestCap` (`capabilities.go:131`) and `forwardCapFor` move into
  `authority`. `AttachModeCap` / `AttachModeScopeCap` are already there and are
  the precedent for this shape.
- The stale comment at `task_handler.go:1302-1303` ("Operator-identity gate,
  like SetCaps") is corrected: RestoreTasks gates on the prune bit
  (`restore_handler.go:89`).

### 5.4 Keeping it single

- **Exhaustiveness test** (in `authority`): for every `TaskControlKind` value
  the protocol defines, `RequestNeed` on a zero-bodied request of that kind
  returns a non-zero `Class`. Adding a kind without deciding it fails.
- **No second computation in the server**: a test (go/ast over `server/`
  non-test files, in the style of `cli/flagorder_test.go`) fails on ANY
  reference to a `protocol.Capability_*` constant outside an allowlist — not
  only one passed directly to `hasCap` / `hasAnyCap` / `authorize` /
  `denyTaskControl`. A direct-argument check would miss the shapes the server
  actually uses: `need := protocol.Capability_FileWrite` then `hasCap(…, need)`
  (`task_handler.go:715`), the `gate(...)` closure in `record_handlers.go`,
  `deny(...)` at `task_handler.go:645`, constants returned from
  `schedulePower`. Uses that are not a requirement (`Capability_All` for the
  operator in `callerCaps`, the `includeShared` widening in `transcript.go:61`,
  visibility-widening reads such as `scheduleReadReach`) are allowlisted with
  the reason, per site. The property is fixed — a new inline bit cannot be
  written without either going through `authority` or being named in the
  allowlist; the exact mechanical form is the builder's.
- **Behaviour is pinned by the existing suite**, which is not edited to make it
  pass. `server/capabilities_test.go` already exercises denial bits and
  direction gates; where a kind's denial `RequiredCap` is not yet pinned by any
  test, the builder adds a case BEFORE changing that site, so the test is red
  if the refactor changes it.

## 6. Verification

| # | question | how |
|---|---|---|
| V1 | Does every kind have a decision? | the exhaustiveness test |
| V2 | Is every request answered as before? | existing `server/` and `cli/` suites, unchanged, plus the pre-refactor pinning cases of §5.4 |
| V3 | Does `RequestNeed` agree with the server for the request-determined clauses? | a table test per REQ / EITHER / MAP kind: build the request, assert the clauses; and, for the REQ kinds, drive `TaskHandler.Handle` with a caller missing the clause's bits and assert TODAY's answer — PermissionDenied with the clause's `Report` where that is what the server sends, the kind's own status where it is not (record_persist on an open answers RecordNotPermitted; ExecRunKill answers NotFound) |
| V3b | Is `Possible(kind)` a superset of every branch? | the enumeration test of §5.1 |
| V3c | Does `ScopeBit` agree with the scope the server resolves? | per kind that checks a target: a caller holding the bit but whose scope for it excludes the target gets today's out-of-scope answer, and one whose scope includes it does not |
| V4 | Is no bit computed outside `authority`? | the go/ast test |
| V5 | Live | `make test`; one `scripts/dummy-harness.sh` run exercising a denial and an allowed call of an inline-gated kind (e.g. `file pull` without file_read, `session snapshot` with exec_view), since the change is in the server's gate path |

## 7. Left to the builder

- Type and function names, and whether `ScopeBit` is a method on `Need`.
- Whether `RequestNeed` takes the decoded request or (kind, body) pairs.
- The order in which sites are converted, and how the work is split into
  commits (every commit leaves the suite green).
- Whether the go/ast guard is a separate test file or joins an existing guard.

### Author's calls (open to the operator)

- `Possible(kind)` is part of this design, derived from the branches, so an
  argument-free consumer has an answer that is not a copy (§5.1).
- `Later.Possible` is a fixed per-kind value; the topic grammar of reach is not
  restated in `authority` (§5.1).
- The §3.4 labels are recorded as `Report` values in `authority` rather than
  allowlisted in the guard (§5.1).
- The guard refuses any `Capability_*` reference outside its allowlist, not
  only direct arguments of the check functions (§5.4).
- `forwardCapFor` and RecordKeep's direction move to `authority`; the
  schedule power functions move as two functions, as today (§5.2).

## 8. Open questions (operator's)

- Whether to fix the denial labels of §3.4 (a behaviour change; would its own
  small spec).
- Whether the record_persist answer shapes of §3.3 should be unified.
- Approach B, after A has landed.

## 9. Appendix — per-kind survey

Survey of `main` at `0b5a069d`. Paths under `server/` unless stated. `cap.go`
is `capabilities.go`, `th.go` is `task_handler.go`. "grep only" means the
absence of a further check was established by search, not a full read.

| kind | category | bits | where | notes |
|---|---|---|---|---|
| Submit | RETIRED | — | th.go:538-545 | answers "submit is retired" |
| List | NONE | — | th.go:547; cap.go:466-483 | visibility + one redacted parent hop |
| Cancel | MAP | cancel | cap.go:45; th.go:564 inScope | |
| PruneTasks | MAP | prune | cap.go:67; th.go:589 scope filter | |
| GetTaskLog | RETIRED | — | th.go:607-614 | |
| OpenInteractive | LOOKUP | spawn \| resume \| resume+exact (req); exec_control if attached (req); record_persist if record (req); schedule_edit if a foreground resume unbinds (lookup) | th.go:634-652, 1756, 1775-1791, 1834-1838, 1888; record_wiring.go:28 | schedule_edit checked only after `resumeTargetAllowed` |
| ClientHello | NOTREQUEST | — | th.go:978-992; psk.go:222-224 | handshake |
| AttachSession | REQ | `AttachModeCap(mode)`; scope `AttachModeScopeCap(mode)` | th.go:965-970; authority/attach.go | bit before scope; exec_resize only decides whether a resize is honoured |
| OpenFileTransfer | REQ | pull → file_read, else file_write | th.go:713-725 | |
| ListFiles | EITHER | file_read or file_write | th.go:742-756 | reports file_read; scope via the held bit |
| DialRunner | MAP | runner_admin | cap.go:73 | grep only |
| OpenPortForward | REQ (constant, inline) | forward_local | th.go:860-865 | |
| Notify | MAP | notify | cap.go:68 | |
| PermissionDenied | NOTREQUEST | — | response arm only | |
| ListConns | NONE | — | th.go:2480-2498 | visibility |
| BoardTopics / BoardRead / BoardSubscribers / BoardArchives / BoardArchiveRead | MAP | board_observe | cap.go:86-87, 99-100, 109 | |
| BoardPurge / BoardRetract / BoardArchivePurge | MAP | board_purge | cap.go:88, 101-102 | |
| BoardContactsClear | MAP | board_purge | cap.go:105; board_handler.go:219 inScope | |
| BoardSend / BoardWake | MAP | board_send | cap.go:110-111 | no reach check for a task caller |
| BoardRetain / BoardFlush / BoardArchive | LOOKUP | board_persist + `PublishableBy(topic)` | cap.go:91-95; board_handler.go:378; board_archive_handler.go:28, 61 | reach failure is a status, not PermissionDenied |
| BoardMembership | LOOKUP | board_member + inScope + `GrantableBy` (join) | cap.go:108; board_handler.go:430-451 | |
| Whoami | NONE | — | th.go:1110-1169 | |
| AwaitIdle / AwaitProgress | LOOKUP | reply → —; notify → notify (req); board → reach (lookup) | await_idle_handler.go:73-108; await_progress_handler.go:43-65 | visibility first |
| RegisterPortForward | REQ | `forwardCapFor(direction)` | th.go:882-889; port_forward.go:335 | |
| ListPortForwards | NONE | — | th.go:897-903 | visibility |
| KillPortForward | LOOKUP | `forwardCapFor(pf.direction)` via authorize | th.go:911-926 | visibility first |
| GitQuery | MAP | file_read | cap.go:112; th.go:772 | |
| SetCaps / TaskRename | IDENTITY | operator, or `regrantReaches` (resume over a strict descendant) | set_caps_handler.go:50-56, 167-175; task_rename.go:66-71 | all refusals NotFound |
| SetParent | IDENTITY | operator only | set_parent_handler.go:45-48 | |
| OpenExecRun | MAP | exec_run | cap.go:113; th.go:798 | |
| ExecRunList | NONE | — | th.go:809-817 | visibility |
| ExecRunKill | LOOKUP | exec_run on the exec's task via authorize | th.go:819-830; exec_run.go:415-422 | never PermissionDenied |
| OpenForwardTap | MAP | forward_tap | cap.go:119; forward_tap_handler.go:39-45 | |
| OpenExecTap | MAP | exec_tap | cap.go:123; exec_tap_handler.go:27-33 | |
| RestoreTasks | REQ | list_only → —; else prune | restore_handler.go:40-91 | stale comment th.go:1302 |
| TrsfState | NONE | — | trsf_state.go:97-110 | needs global visibility rank, not a bit |
| AgentSend | LOOKUP | reach of destination and reply_to (`publishableFor`) | agent_taskcontrol.go:166-189; agentboard/reach.go:125-196 | reported as board_peer |
| AgentSubscribe / AgentWait | LOOKUP | `readableFor` | agent_taskcontrol.go:85-88, 298-301; reach.go:110-121 | reported as board_observe |
| AgentUnsubscribe, AgentListSubscriptions, AgentInbox, AgentInboxAdvance, AgentListRetained, AgentReadSeq, AgentRetract, AgentListContacts, AgentReadPage, AgentArchives, AgentArchiveRead | NONE | — | agent_taskcontrol.go; board_archive_handler.go:187, 214 | own state, subscription or readability |
| AwaitList / AwaitKill | NONE | — | idle_watchers.go:127-142, 210-214 | |
| ProgressSet / LifecycleSet | NONE | — | progress.go:56-67; lifecycle.go:28-37 | self only |
| ProgressClear | MAP | cancel | cap.go:72; progress.go:105 | |
| RunnerEnroll / RunnerUnenroll / RunnerKeys / RunnerKick | MAP | runner_enroll | cap.go:76-78, 85 | grep only |
| RunnerDialAdd / RunnerDialList / RunnerDialRemove | MAP | runner_admin | cap.go:82-84 | grep only |
| SessionMode | LOOKUP | cancel (map) + schedule_edit when unbinding (lookup) | cap.go:46; session_mode.go:33-61 | |
| RecordList / RecordGet | MAP | exec_view | cap.go:50-51; record_handlers.go:41 (repeats) | |
| RecordDelete | MAP | record_delete | cap.go:52 | |
| RecordKeep | LOOKUP | pre-gate persist\|delete; then by current retention | record_handlers.go:169-200 | reach before the retention lookup |
| RecordSwitch | REQ | on → record_persist; off → record_delete | record_handlers.go:216-220 | |
| TranscriptList / TranscriptGet | MAP | transcript_read | cap.go:55-56; transcript.go:54 | exec_run only widens `includeShared` (transcript.go:61) |
| ScheduleCreate | REQ | schedule_edit AND (no task → spawn; task → resume + inScope) AND record_persist if record | cap.go:59; schedule_handler.go:700-728, 582-588 | |
| ScheduleEdit | LOOKUP | schedule_edit AND power over the schedule's state AND record_persist | cap.go:60; schedule_handler.go:881-892 | power after reach and self |
| ScheduleDelete | MAP | schedule_edit | cap.go:61 | |
| ScheduleRun | LOOKUP | schedule_run AND (pending → spawn; bound → resume\|resume_exact) | cap.go:62; schedule_handler.go:1011-1071, 736-752 | |
| ScheduleList / ScheduleShow | NONE | — | schedule_handler.go:1163-1180 | `scheduleReadReach` filter |
| CredentialIssue / CredentialRekey | MAP | credential_issue | cap.go:65-66; credential_handler.go:129-165 | |
