# Grant conditions, task-bound enrollment, and server-defined actions — design

Date: 2026-11-26. Line references are to `main` at `3b1dc6e7`.

**Status: DESIGN.** Nothing here is implemented. It is a prerequisite of
pillar D (ephemeral runners, `2026-11-12-run-modes-and-records-grand-design.md`
§ Pillar D), not pillar D itself.

## Problem

- **P1 — `runner_enroll` is all-or-nothing.** It is a bit with no scope
  (`cli/verb/caps.go:482`, `scope: ScopeNone`; the four key verbs are `noTarget`
  in `server/scope_completeness_test.go:83-87`). An enrolled key records only
  the key, a label and who enrolled it (`server/runnerkeys.go:19-24`), and a
  runner declares its own roots. So a holder can enroll a key whose runner then
  takes any task whose repo it claims as a root, receiving that task's prompt
  and a ticket carrying that task's caps. A pillar-D controller that only
  provisions a runner for one task must still hold this power over every task.
- **P2 — a provisioned runner is not tied to the task it was made for.**
  `Registry.Candidates(repo, sel)` (`server/registry.go:430`) matches roots and
  selector only; `Scheduler.Tick` deliberately does not enforce
  `BoundRunnerID` (`server/scheduler.go:62-66`). A runner provisioned for task T
  can take another queued task for the same repo, and T can go to a
  pre-existing runner while the provisioned one is still booting.
- **P3 — authority stops at the task.** A grant is (capability bit) × (target
  task set) (`TaskScope`, `ScopeOverride`, `runner/protocol/message.bgn:5214-5255`).
  Nothing bounds which object inside a target a bit reaches: `exec_run` runs any
  argv in the target's worktree, `forward_local` / `forward_remote` reach any
  port. Running exactly one command on another host (the motivating case: a
  `deploy` command), or opening one port, needs the whole bit.
- **P4 — server responses are not size-checked.** Every control message is one
  datagram; over UDP an oversized one is dropped with no error to either end.
  Client task-control requests (`cli/client.go:385`) and server
  `RunnerRequest`s (`server/runner_send.go:27`) go through
  `protocol.CheckControlMessage`; task-control responses do not. They are sent
  from `respondAgent` (`server/agent_taskcontrol.go:61`, 31 callers) and from 50
  other sites that append `AppKind_TaskControl` and call `SendMessage` directly
  (`grep -rn 'AppKind_TaskControl)' server --include=*.go`, non-test, counted
  2026-11-26). `WhoAmIResponse` carries the caller's overrides inline
  (`message.bgn:2870`, `:2875-2876`). Anything this spec adds to a grant makes
  that worse unless the response path is guarded.

## Intent

- Pillar D's controller runs outside the harness and authenticates with a
  credential task (`2026-11-21-credential-tasks-design.md`). Its credential
  should reach only the tasks it provisions for (P1, P2).
- One mechanism for "this bit, but only this object", not one per verb (P3).
- Definitions of runnable commands are managed in one place, the server
  (operator, 2026-11-26).

## Decisions taken

**operator** = the human chose it in conversation (2026-11-26). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| C1 | **`runner_enroll` becomes a scoped bit.** Global scope = may enroll a key with no binding (today's behaviour) or bound to any task. Any narrower scope = may enroll only keys bound to tasks inside it. Not a new bit beside the old one: global already means "every task, including ones created later", which is exactly what an unbound runner accepts, so the scoped reading needs no special case | operator |
| C2 | **An enrolled key may be bound to a set of tasks.** A runner that authenticated with a bound key is a candidate only for tasks in that set. An unbound key behaves as today. Binding is fixed at enroll; changing it is unenroll + enroll | operator (the binding), this spec (fixed at enroll) |
| C3 | **No re-grant migration.** On 2026-11-26 `harness-cli ls --json --finished all` listed 39 tasks (`finished_omitted: 0`, caller scope `global/global`); exactly one held `runner_enroll`, with `runner_enroll: global` in `scope_by_cap`. A task later brought back by `restore` keeps the scope it was recorded with; if that is not global it can no longer enroll an unbound key, and is re-granted by hand | operator (confirmed the holder), this spec (restore note) |
| C4 | **A grant condition bounds the object a bit reaches inside a target.** It attaches to one `ScopeOverride` and applies to every bit in that override's mask. A condition kind is applicable only to the bits listed for it (C9); attaching it to any other bit is refused | operator (one general mechanism), this spec (per override) |
| C5 | **Attenuation is by containment (IAM-style), not by appended caveats.** A child's condition must be contained in the granter's for the same bit. Every condition kind must have a cheap, exact containment test (set inclusion, range inclusion, equality). **Regular expressions and globs are not condition kinds**: a mistake in one widens the grant silently (a missing anchor matches more), containment between them is expensive, and the result is unreadable on the three surfaces | operator |
| C6 | **Condition bodies never travel inline in a control message, in either direction.** Inline, an override carries only a one-bit marker (`conditioned`, from `ScopeOverride.reserved`). Grant-writing requests send the bodies on a client-created stream; reads receive them on a stream. The body is stored on the `TaskEntry` (persisted in the task WAL with the override). *Correction of the first draft of this spec*, which carried bodies inline in requests because requests pass `CheckControlMessage`: that narrowed the agreed property to "no body in an *unguarded* datagram", and put a worst case of 1128 bytes of bodies against a ~1170-byte floor | operator (bodies on streams, fixed-size marker inline; then approved the store on the `TaskEntry` and the one-bit marker over a separate store with a u32 reference) |
| C7 | **Fail-closed.** A bit whose override is marked conditioned reaches nothing through `inScope` / `authorize` / `scopeSet`; only an object-aware gate that evaluates the condition can pass it. A marker with no readable body (a corrupt WAL record, a client that did not fetch it) is shown and enforced as "reaches nothing", never as unconditioned | operator (fail-closed), this spec (where) |
| C8 | **Actions are defined on the server.** An action is a named command template; a grant names which actions it may run. The runner is not told about actions: the server resolves an invocation to an argv and sends the existing `RunnerExecRunRequest`, so no runner-decoded format changes | operator (server-side, central), this spec (runner unchanged) |
| C9 | **Two condition kinds in this round.** `ActionSet` (a set of action ids) is applicable to `exec_run`; `PortRange` (a list of inclusive port ranges) is applicable to `forward_local` and `forward_remote`. Enrollment is not a condition kind: C1/C2 express it with the ordinary task scope | operator (`PortRange` and actions first; enrollment through scope), this spec (which bits) |
| C10 | **An action runs against a task**, the existing `exec_run` target (`ExecRunRequest.task_id`): typically a long-lived task on the host where the command lives. No runner-targeted exec path is added | operator |
| C11 | **An action can be edited, but only by someone who could re-grant every holder of it.** Editing an action changes what every grant naming it runs, which is what `set_caps` does to one task. So an edit is gated as a `set_caps` on each holder would be (`server/set_caps_handler.go`): the operator always; a task only when, for every task holding a grant that names the action, it passes R1 (`regrantReaches`: a strict descendant inside its resume scope) and R2 (its own `exec_run`, with no `ActionSet` condition, reaches every target that holder's `exec_run` override reaches). A schedule whose stored authority names the action also needs what `schedule edit` of that schedule needs. One refusal refuses the whole edit. Deleting an action only narrows (grants naming it fail closed, C7), so it needs `action_admin` alone. *Reversal, twice*: the conversation first said an edit is recomputed under the editor's authority; the first draft then made actions immutable, on the ground that an edit changes what grantees run without any grant changing. That ground was wrong — `set_caps` does exactly that today; what differs is who may do it to whom, which this gate settles | operator (editable, gated like `set_caps`), this spec (R1/R2 per holder, schedules, delete) |
| C12 | **Action parameters are typed, never free text.** A template element is a literal or a parameter reference; a parameter value always becomes one whole argv element. Types: `choice` (one of a fixed list of literals in the definition) and `token` (1–128 bytes of `[0-9A-Za-z._-]`, not starting with `-`). The `token` check is fixed code, not a pattern supplied by anyone. `shell_line` is always 0 and stdin is always `/dev/null` for an action run | this spec |
| C13 | **Defining, editing and deleting actions needs a new grantable bit, `action_admin`** (scope none); an edit additionally passes C11's per-holder gate. Listing actions needs `action_admin` or `exec_run`. Not operator-only (operator-only is reserved for re-grant authority) | this spec |
| C14 | **`PortRange` bounds the port only, not the host.** For `forward_local` it bounds `target_port`; for `forward_remote` it bounds `bind_port` (`RegisterPortForwardRequest`, `message.bgn:5286-5294`). An `ip` forward (`forward_link`) is not a `PortRange` bit. Host bounds are left out: `localhost`, `127.0.0.1` and `::1` are one host under three spellings, so a host condition needs a canonicalisation design of its own | this spec |
| C16 | **An action id is 16 random bytes (`ActionID`), not a counter.** The action store is its own file, outside the WAL (as the runner-key store is, `server/runnerkeys.go:39-41`), while grants naming action ids are in the WAL. If the file were lost or restored from an older copy, a counter would restart and a new action would take an old id — every grant still naming it would silently run the new command. A random 128-bit id is not re-issued in practice (the same odds TaskID relies on); a grant naming a lost id fails closed (C7). Replaces the `u32` id of the first draft | this spec |
| C17 | **Code-level shape of actions** (§ Actions): `argv[0]` is always a literal, so no parameter chooses the program; a create and an edit are one request kind, discriminated by a zero id as `OpenInteractiveRequest.resume_task_id` is; an edit names the revision it edits and is refused on a mismatch, so two editors cannot overwrite each other unseen; an action run forces `pty = 0`, `shell_line = 0` and `stdin_enabled = 0`; an invocation does not pin a revision (the exec list records which ran) | this spec |
| C15 | **Task-control responses get a size guard.** All of them are sent through one function that runs `CheckControlMessage` against the connection's transport and datagram budget; over budget, it sends a new `response_too_large` answer for the same request id instead. A test fails if any other server code appends `AppKind_TaskControl` to a response and sends it. `WhoAmIResponse` stays inline | operator (guard, no move to a stream), this spec (one function + test) |

## Anti-goals

- **A conditioned grant that reads as unconditioned** on any path: a gate that
  forgets the object, a client that did not fetch the body, a WAL record
  missing its body (C7).
- **An action whose meaning changes under an existing grant by someone who
  could not have re-granted that holder** (C11).
- **A bound runner that takes a task outside its binding** (C2), including a
  foreground open.

## Non-goals

- Pillar D itself (provisioner, reclamation, reconciliation).
- Host conditions on forwards (C14), path conditions on file transfer, and
  conditions on any bit not listed in C9.
- Binding a key to a root instead of a task (a warm pool that serves several
  tasks). It would need a condition-like root set; per-task ephemeral runners
  do not.
- Making the inputs of an action unwritable. An action is only as narrow as the
  files it reads: a grantee that also holds `file_write`, `exec_cowrite` or an
  unconditioned `exec_run` over the same task can change what the command does,
  and so can an agent running in that task. Where the command lives is the
  operator's arrangement; § Surfaces requires the overlap to be shown.

## Design

### Bound enrollment (C1–C3)

- `RunnerKeyRecord` gains `BoundTasks` (task ids; empty = unbound), persisted as
  `bound_tasks` (omitted when empty) in the key store's JSON file
  (`server/runnerkeys.go`). Existing files load as unbound.
- `RunnerEnrollRequest` (`message.bgn:3598`) gains `bound_len :u8` and
  `bound :[bound_len]TaskID`, at most 16.
- Gates, all through the caller's `runner_enroll` scope:

  | Verb | Unbound key | Key bound to S |
  | --- | --- | --- |
  | enroll | global scope | every task in S is in scope |
  | unenroll | global scope | every task in S is in scope |
  | kick (names a runner; its key decides) | global scope | every task in S is in scope |
  | keys (list) | listed to global holders | listed when every task in S is in scope |

  Out of reach answers as an unknown key or runner does, not as
  `permission_denied` (no existence oracle; `server/capabilities.go:270-282`).
  `capAuditTable` moves `runner_enroll` from `ScopeNone` to scoped, and the four
  kinds move from `noTarget` to `targetGated` in
  `server/scope_completeness_test.go`.
- Placement: `Registry.Candidates` takes the task id. A runner whose key is
  bound is skipped unless the task id is in its set; an unbound runner is
  unchanged. The registry entry already holds the key it authenticated with
  (`RunnerEntry.Key`, `server/registry.go:72-75`); it also keeps the binding
  read at registration. The five call sites (`server/background_open.go:166`,
  `:212`, `server/scheduler.go:81`, `server/task_handler.go:1890`,
  `server/schedule_handler.go:670`) pass the task id. Where there is none yet
  (a new foreground open, a schedule check at creation), they pass zero, and
  zero matches no binding.
- A key bound to T gives whoever holds the private half T's prompt and a ticket
  with T's caps. Granting `runner_enroll` over T is therefore a statement that
  the holder may take over T's execution; the bit stays `TierStrong`.

### Grant conditions (C4–C7, C9)

- Wire: `ScopeOverride` takes one bit of `reserved` as `conditioned :u1`. Its
  size does not change. Override masks are pairwise disjoint (comment at
  `message.bgn:5236-5238`), so (task, mask) identifies the override a body
  belongs to.
- Body format, one per conditioned override:

  ```
  enum ConditionKind: action_set, port_range
  format GrantCondition:
      caps :Capability            # the override's mask
      kind :ConditionKind
      match kind:
          action_set => actions :[n ≤ 32]ActionID   # C16
          port_range => ranges  :[n ≤ 8]{lo :u16, hi :u16}
  ```

- Every format that embeds `ScopeOverride` (`grep -n ScopeOverride
  runner/protocol/message.bgn`, 2026-11-26) is one of three kinds:

  | Format | Kind | Change |
  | --- | --- | --- |
  | `OpenInteractiveRequest`, `CredentialIssueRequest`, `CredentialRekeyRequest`, `SetCapsRequest`, `ScheduleSpec` | writes a grant | gains `conditions_stream_id :u64` (0 = no conditions); the bodies (`GrantConditions { n ≤ 8, GrantCondition… }`) are read from that stream before the request is processed |
  | `TaskInfo` (in `ListResultBody`), `ScheduleDetail` | read; travels on a stream | gains the bodies |
  | `WhoAmIResponse` | read; inline, unguarded today | marker only; `whoami` on the CLI fetches the caller's own row through the list path to print the bodies |

  A schedule carries its own authority and writes it onto its task at each
  firing (grand design G36, G38), so its conditions are stored and written
  the same way.
- Request path: the client creates a send stream, writes the encoded
  `GrantConditions`, and names it in `conditions_stream_id`. The server reads it
  as `readExecBody` reads an exec body (`server/exec_run.go:39`, the stream named
  by `ExecRunRequest.payload_stream_id`). The request is refused whole, and no
  grant is written, when: the stream cannot be read; the body fails to decode
  or exceeds the limits (8 conditions, 32 action ids, 8 port ranges); a
  condition names a mask that is not one of the request's overrides; or an
  override the client marked `conditioned` has no body. A request never
  produces an unconditioned grant in place of a conditioned one it could not
  read. The inline size of the five requests grows by 8 bytes each.
- Storage: the bodies live on the `TaskEntry` beside the overrides and in the
  WAL beside `scope_overrides` (`server/wal.go:76`, `:216`); a schedule's live
  in the schedule store beside its overrides. Old records have no bodies and
  no marker.
- Enforcement: `inScope`, `authorize` and `scopeSet` (`server/capabilities.go`)
  answer "not reached" for a conditioned bit. A new
  `authorizeObject(cid, want, target, obj)` evaluates the condition. It is the
  only way past a conditioned bit, so a gate that does not pass the object
  denies. Objects: the action id for `exec_run`; the port for a forward.
- Attenuation, in `authority` beside `Attenuate` / `ClampUnder`: a child naming
  no condition for a bit the granter holds conditioned inherits the granter's
  condition. A child naming a condition not contained in the granter's is
  refused, naming the offending bit, as an out-of-reach scope id is. When a
  granter's condition is narrowed (set_caps), descendants are clamped to it, as
  `ClampDescendant` does for scope. An unconditioned bit contains every
  condition.

### Actions (C8, C10–C13, C16, C17)

#### Wire (`runner/protocol/message.bgn`)

```
format ActionID:
    id :[16]u8                         # random (C16); all-zero = "new" in ActionPut

enum ActionParamType:
    :u8
    choice = 1                         # one of the definition's literals
    token  = 2                         # 1-128 bytes of [0-9A-Za-z._-], no leading '-'

format ActionLiteral:                  # one argv element, as written
    len   :u16                         # 1..4096
    bytes :[len]u8                     # no NUL

format ActionParam:
    name_len :u8
    name     :[name_len]u8             # [a-z][a-z0-9_]{0,31}, unique in the definition
    type     :ActionParamType
    match type:
        ActionParamType.choice => choices_len :u8                    # 1..32
                                  choices :[choices_len]ActionLiteral
        ActionParamType.token  => ..

enum ActionElementKind:
    :u8
    literal = 1
    param   = 2

format ActionElement:
    kind :ActionElementKind
    match kind:
        ActionElementKind.literal => literal :ActionLiteral
        ActionElementKind.param   => param_index :u8                 # into params

format ActionDefinition:               # what a client writes
    name_len        :u8
    name            :[name_len]u8      # [a-z0-9][a-z0-9._-]{0,63}, unique across the store
    params_len      :u8                # 0..16
    params          :[params_len]ActionParam
    template_len    :u8                # 1..64
    template        :[template_len]ActionElement   # [0] is a literal (C17)
    description_len :u16
    description     :[description_len]u8           # 0..1024, display only

format ActionRecord:                   # what the server reports
    id            :ActionID
    revision      :u32                 # 1 at create, +1 per edit
    definition    :ActionDefinition
    created_at_ms :u64
    edited_at_ms  :u64                 # = created_at_ms until the first edit
    created_by_len :u8
    created_by     :[created_by_len]u8 # "operator" | "task:<32 hex>", as RunnerKeyRecord.EnrolledBy
    edited_by_len  :u8
    edited_by      :[edited_by_len]u8

format ActionValue:                    # one parameter value at invocation
    len   :u8
    bytes :[len]u8

format ActionInvocation:
    id         :ActionID
    values_len :u8
    values     :[values_len]ActionValue    # one per param, in params order
```

`TaskControlKind` gains `action_put`, `action_remove`, `action_list`
(appended), with these arms:

```
format ActionPutRequest:
    id                :ActionID        # zero = create
    expect_revision   :u32             # edit: must equal the current revision; create: 0
    payload_stream_id :u64             # client send stream carrying one ActionDefinition

enum ActionStatus:
    :u8
    ok
    not_found              # unknown id, or one the caller may not see
    invalid_definition     # message says which rule
    name_taken
    stale_revision         # edit: expect_revision is not current
    holders_unreachable    # edit: C11 refused; holders_refused says how many
    body_too_large
    bad_body

format ActionPutResponse:
    status          :ActionStatus
    id              :ActionID          # the new or edited action on ok
    revision        :u32
    holders_refused :u32               # holders_unreachable only
    message_len     :u16
    message         :[message_len]u8

format ActionRemoveRequest:
    id :ActionID

format ActionRemoveResponse:
    status :ActionStatus

format ActionListRequest:
    reserved :u8

format ActionListResponse:
    status    :ActionStatus
    stream_id :u64                     # server send stream carrying ActionListBody

format ActionListBody:
    actions_len :u16
    actions     :[actions_len]ActionRecord
```

`ExecRunBody` appends `action_present :u1`, `reserved :u7` and, when set,
`action :ActionInvocation`; `argv` must then be empty. `ExecRunInfo` appends
`action_present :u1` and, when set, `action_id :ActionID`,
`action_revision :u32` and the action's name. `Capability` gains
`action_admin = 0x100000000`, and `all` widens to `0x1ffffffff` (operators get
`Capability_All` from `callerCaps`, so a narrower `all` would deny the operator
the new bit; the comment above `all` already says so).

#### Shared validation (`actiondef/`, a new package)

Pure functions, imported by `server`, `cli` and the wasm build so that a client
can refuse a bad definition before sending it and the server applies the same
rule:

```go
// Validate reports the first rule def breaks (C12, C17, the limits above).
func Validate(def *protocol.ActionDefinition) error

// Resolve checks values against def's params and returns the argv:
// literals as written, each param element replaced by its value as ONE element.
func Resolve(def *protocol.ActionDefinition, values []protocol.ActionValue) ([]string, error)

// ValidToken is the token rule; fixed code, no pattern from any caller.
func ValidToken(v []byte) bool

// ParseTemplate turns CLI words into elements:
//   - a word that is exactly "{name}" for a declared param is a reference;
//   - a word starting with "\{" is a literal with that backslash dropped;
//   - a word that contains "{name}" for a declared param anywhere else (e.g.
//     "--env={env}") is refused: there is no partial substitution, and
//     passing it through as a literal would surprise whoever wrote it;
//   - every other word is a literal.
func ParseTemplate(words []string, params []protocol.ActionParam) ([]protocol.ActionElement, error)
```

#### Server

- `server/actions.go`: `ActionStore`, the sibling of `RunnerKeyStore` —
  `<data-dir>/actions.json`, `sync.Mutex`, `map[[16]byte]ActionRecord`,
  `LoadActionStore` (a file that does not parse is an error),
  `Put` / `Remove` / `Get` / `List`, `saveLocked` writing a temp file and
  renaming it, as `RunnerKeyStore.saveLocked` does. Persisted as native JSON
  fields (name, params, template, …), not as encoded wire bytes, so a later
  `.bgn` change does not reinterpret the file.
- `server/action_handler.go`: `handleActionPut`, `handleActionRemove`,
  `handleActionList`. `action_put` with a non-zero id collects the holders
  (tasks whose conditions name the id, from the task store; schedules, from
  the schedule store), applies C11 to each before writing, then bumps the
  revision.
- `authority/need.go`: `action_put` and `action_remove` →
  `Capability_ActionAdmin` (central); `action_list` →
  `AnyOf: ActionAdmin | ExecRun`. `server/scope_completeness_test.go`: all
  three `noTarget`.
- `server/exec_run.go` `openExecRun`: when `action_present`, look the action up
  (missing → `not_found`), `actiondef.Resolve`, gate with
  `authorizeObject(cid, exec_run, task, action id)`, force the C17 flags, and
  continue with the resolved argv; record id, revision and name on the
  `execRun`. A raw argv against an `ActionSet`-conditioned `exec_run` fails the
  same gate.

#### CLI (rows in `cli/verb/table.go`, so TUI cmdline and WebUI get them)

```
harness-cli action new  --name deploy [--description TEXT] \
    [--param env=choice:staging,prod] [--param ref=token] -- ./deploy.sh --env {env} --ref {ref}
harness-cli action edit <id|name> --revision N [same flags] -- <template words>
harness-cli action rm   <id|name>
harness-cli action ls   [--json]
harness-cli exec <task> --action <id|name> [--value env=prod --value ref=v1.2]
```

A name resolves to an id client-side through `action_list`; every wire request
carries the id.

### Response size guard (C15)

- One function, e.g. `sendTaskControl(conn, resp)`, replaces `respondAgent` and
  the 50 direct sites. Over budget it logs the kind and size and sends
  `TaskControlKind.response_too_large` with the same `request_id` and the
  original kind. The client maps it to an error naming both.
- A test parses `server/` and fails on any non-test `SendMessage` of a buffer
  that `TaskControlResponse.MustAppend` / `Append` produced outside that
  function. The negative control: the test is red before the sites move.

## Surfaces

Every new input and display goes on the CLI, TUI and WebUI; walk
`surface-parity-checklist` items 1–39 for:

- the `--bind` task list on enroll, and the binding in `runner keys` and the
  runners view;
- conditions on spawn, set_caps and credential issue, and their display beside
  `scope_by_cap` everywhere caps are shown (marker + body; "conditioned,
  body unknown" when only the marker is available);
- the action verbs and the action variant of exec;
- a warning wherever a grant is shown that holds an `ActionSet`-conditioned
  `exec_run` together with `file_write`, `exec_cowrite` or unconditioned
  `exec_run` over an overlapping target (Non-goals, last bullet).

## Wire and disk

Appended fields and arms only: `ScopeOverride.conditioned`,
`conditions_stream_id` on the five grant-writing formats, `GrantCondition`
lists on `TaskInfo` and `ScheduleDetail` (both already on streams),
`RunnerEnrollRequest.bound`,
`ExecRunBody`'s action variant, the action verbs, `response_too_large`. The
WAL gains condition bodies; the key store gains `bound_tasks`; the action store
is new. Run `scripts/wire-skew-check.sh`. `RunnerHello` and every
runner-decoded format are unchanged (C8), so a runner does not need a restart.
Runner registration is the path the harness upgrades itself through (Pitfall 10).
Enrollment changes only its client→server request, and an old key-store file
loads as unbound, so no transition window is needed.

## Testing

- C1/C2: a holder scoped to `ids:[T]` enrolls a key bound to T, and is refused
  an unbound key and a key bound to U, with the unknown-key answer. A global
  holder enrolls unbound. Unenroll, kick and keys follow the C1 table.
- C2 placement: a runner with a key bound to T is not a candidate for queued
  U on the same repo, nor for a foreground open; it is for T. With an unbound
  runner and a bound runner both connected, U goes to the unbound one.
- C5: refused when the child's `ActionSet` is not a subset or its `PortRange`
  is wider; inherited when the child names none; clamped when the granter is
  narrowed.
- C7: for each of `inScope`, `authorize` and `scopeSet`, a conditioned bit
  reaches nothing. A WAL record with the marker and no body replays as reaching
  nothing.
- C12: `token` refuses a leading `-`, an empty value, 129 bytes, and a byte
  outside the class; `choice` refuses a value not in the list; a parameter
  value is one argv element even when it contains a space.
- C11: an edit by the operator succeeds; by a task that `regrantReaches`
  every holder and whose unconditioned `exec_run` covers their targets,
  succeeds; with one holder outside its resume scope (R1), or one holder whose
  `exec_run` target its own `exec_run` does not reach (R2), or a holding
  schedule it may not `schedule edit`, is refused and changes nothing. After an
  edit, a holder's next run uses the new template. Deleting an action makes
  `exec_run` of it refused for a grantee that could run it before.
- C15: a response encoded over the budget arrives as `response_too_large` over
  UDP. Demonstrated red first: an oversized `whoami` over `scripts/netem-lab`
  hangs before the guard.
- C6 request path: for each of the five grant-writing requests, an unreadable
  conditions stream, an undecodable or over-limit body, a body naming a mask
  that is not an override, and a `conditioned` override with no body are each
  refused, and the task / credential / schedule is unchanged afterwards.
- Dummy-harness E2E: issue a credential holding `runner_enroll` over one task T;
  from it, enroll a key bound to T; start a runner with that key; see T placed
  on it and another background task for the same repo not placed; run an
  action through `exec` with an `ActionSet` grant and see a raw argv refused.
