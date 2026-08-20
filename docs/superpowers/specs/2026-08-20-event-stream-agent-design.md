# Event-stream agents: a third task kind, driven by structured events instead of a PTY

Date: 2026-08-20

**Status: PENDING.** §§1–4 were agreed in discussion and are recorded here so
they survive a context compaction. §5 is blocked on
[`2026-08-20-per-capability-scope-design.md`](2026-08-20-per-capability-scope-design.md).
Nothing here is approved for implementation, and no implementation plan has
been written.

## Problem

A task runs either as `TaskKind_Oneshot` (fire-and-forget, argv template
`{args} -p {prompt}`) or `TaskKind_Interactive` (a PTY the operator splices
into). The interactive kind is the only multi-turn one, and its data plane is
**bytes on a terminal**. Three consequences follow from that alone:

1. Idle detection is PTY byte-quiescence — a heuristic, because a byte stream
   carries no turn boundary.
2. Reattach replays a byte ring with no VT state, so an alt-screen program
   redraws wrong (recorded as WONTFIX for the PTY path).
3. Approval prompts are answered by **keystrokes**, so nothing structural
   distinguishes "allow once" from "stop asking" — see §3.

The oneshot kind already receives structured events: the live claude runners
run `--output-format stream-json --verbose` with `--agent-log-format
claude-stream-json`, and `runner/agentlog` decodes claude's stream-json and
codex's `--json` into one agent-neutral `Event` type. But that package
deliberately **renders those events to text log lines** at the runner and
publishes the lines, precisely so the display surfaces need no changes. The
structure never reaches the wire, so nothing downstream can act on it.

### What was verified before designing this

Probed against `claude 2.1.237` on 2026-08-20 (`project_claude_stdio_can_use_tool_channel`):

- One process with `-p --input-format stream-json --output-format stream-json`
  accepts **multiple user turns**, keeping one `session_id`, and exits cleanly
  when stdin closes. A PTY-free multi-turn agent is mechanically possible.
- Without `--permission-prompt-tool`, a tool needing approval produces a
  one-way `{"type":"system","subtype":"permission_denied"}` notice and the tool
  is refused outright.
- With the sentinel `--permission-prompt-tool stdio` — the value the Agent SDK
  itself passes, found verbatim in its bundled spawn code — the CLI emits a
  `control_request` of subtype `can_use_tool` carrying `request_id`,
  `tool_name`, `input`, `tool_use_id` and `permission_suggestions`, and
  **blocks synchronously** until answered. Answering with a `control_response`
  bearing `{"behavior":"allow","updatedInput":{…}}` ran the tool. Leaving it
  unanswered aborted that call with `AbortError` when stdin closed.
- `updatedInput` lets the answering host **rewrite the tool's arguments**
  before allowing.

Untested and therefore not designed around: the `deny` response shape, and the
`hook_callback` / `mcp_message` control subtypes that exist in the binary.

The channel is undocumented and version-coupled. `system/init.capabilities`
advertises `interrupt_receipt_v1`, `interrupt_cancel_queued_v1` and
`msg_lifecycle_v1` — **not** `can_use_tool` — so feature detection must be
"did a `control_request` arrive", never a capabilities check.

## Design

### 1. Neutral event and declared extension

Defined **once**, in `.bgn`. The adapter protocol (§2) is the JSON projection
of that same type; there is no second schema.

```
AgentEvent {
  kind:  SessionStart | Thinking | ToolStart | ToolEnd | Text | Finish | Error
  text, tool, args, result : string
  exit_code : optional int
  is_error, warning : bool
  stats  : { duration_ms, cost_usd, input_tokens, output_tokens }
  extras : []AgentEventExtra
}
AgentEventExtra { key: string, value: string }
```

The core vocabulary is `runner/agentlog.Event` promoted onto the wire. It is
already proven across two vendors, which is the evidence that it is stable
enough to schema.

**Rules that keep `extras` from becoming a dumping ground:**

- Keys are vendor-namespaced; a bare key is invalid — `claude.subtype`,
  `codex.item_type`.
- Values are UTF-8 strings only. Wanting structure in a value is the signal
  that the field should be promoted, not that the value should carry JSON.
- Surfaces render `extras` in a **detail view only**. No primary display
  depends on one.
- Two vendors emitting the same key with the same meaning makes it a promotion
  candidate.

**Promotion** turns an `extras` key into a named core field, and the adapter
stops emitting the extra in the same change — never both at once. It costs a
schema bump and a pass over three surfaces, so it happens when a second vendor
actually needs it, not on suspicion.

**The neutrality line**, decided against events observed in the probe:

| observed | destination | why |
|---|---|---|
| `thinking` | core `Thinking` | already in the two-vendor vocabulary |
| `total_cost_usd`, token counts | core `Stats` | codex reports the same concept |
| `system/thinking_tokens` | `claude.thinking_tokens` | claude-specific breakdown |
| `rate_limit_event` (five-hour window) | `claude.rate_limit.*` | subscription windows are claude-specific |
| `system/informational` | `claude.informational` | meaning not established |
| `permission_suggestions` | **core**, on the request type (§4) | the approval UI needs it structurally, and "allow and stop asking" is not vendor-specific |

Only concepts that exist for agents in general belong in the core.

### 2. Adapter placement

The volatile thing is the vendor protocol itself — type names, envelope shapes,
the sentinel, subtypes that appear and disappear. It goes behind a seam, in the
shape the server's `notify-hook` already established.

```
runner ──spawn──▶ adapter <resolved agent argv…> ──spawn──▶ [sandbox wrapper ─▶] agent
        │  stdin  ◀── neutral responses (approvals, user turns)
        │  stdout ──▶ neutral events and requests
        └  stderr ──▶ task log, verbatim, as [err]
```

| | owns |
|---|---|
| **runner** | worktree, env injection, argv resolution, whether a sandbox wrapper is applied, process lifetime and kill, log forwarding — **exactly its current job** |
| **adapter** | vendor flags, vendor JSON ↔ neutral JSON, `request_id` correlation |
| **runner, newly** | an NDJSON framer over the adapter's stdio and a router: events → pubsub, requests → the pending table. Nothing else |

The runner does not parse vendor JSON, does not know the sentinel, and does not
decide approvals. Those three negatives are the acceptance criterion for
"embedding did not simply move".

**The runner resolves argv and hands it to the adapter**, which execs it. The
adapter holds no policy of its own. This is the shape the VS Code extension
uses for `claudeProcessWrapper` ("the bundled binary path is passed as an
argument"), and it composes with the podman kit for free: when the runner
resolves `--agent-bin` to `agent-in-podman.sh`, the adapter execs that, leaving
**the adapter outside the container and the agent inside** — the correct side
for harness-side glue.

**Vendor flags are appended by the adapter**, never by the runner's argv
template; the sandbox wrapper forwards trailing arguments, so appending works
through it. Putting them in the template would return vendor knowledge to the
runner and is the one thing this seam exists to prevent.

**Resolution and hot reload.** Profile field `streamAdapter` → `--stream-adapter`
flag → env. The path is read **on every task assign**, so editing an adapter
reaches the next task with no rebuild and no restart — the same trade
`--agentskills-dir` makes.

**Acceptance criterion, stated as a rule**: changing an adapter must never
require rebuilding or restarting the runner. Consequently the adapter protocol
is a **public contract** — third-party adapters are a first-class use, not a
side effect — and carries a `protocol_version` in its startup handshake that
the runner checks and refuses on mismatch. Without it, the day the neutral
event grows a field is the day every hand-written adapter breaks silently.

**Failure discipline:**

- Adapter path unresolvable or not executable → the task **fails at start**,
  loudly. Skill injection is warn-only, and a warn here would hand every task a
  silently agent-less run.
- A malformed line from the adapter → one raw event, task continues. This
  mirrors `agentlog`'s existing rule that decoding never returns an error.
- Adapter dies → the process group, agent included, is torn down.

**Capability declaration.** Whether a profile can serve this kind rides on
`RunnerHello` and is stamped onto every task the server assigns, the same
mechanism as the `skills_injected` / `+skills` marker. A client asking for the
kind against a runner that cannot serve it is refused at dispatch instead of
failing obscurely later.

### 3. Authority, verbs, surfaces

**Capabilities are the existing three, given per-kind meaning. No new bit.**

| cap | PTY kind | event-stream kind |
|---|---|---|
| `exec_view` | read the PTY | read the event stream |
| `exec_cowrite` | type into it | append a user turn, **and answer a pending request** |
| `exec_control` | sole writer, owns size | the bookkeeping seat only (`Running` vs `Detached`) |
| `exec_resize` | resize | **not applicable — refused explicitly**, never a silent no-op |

Approval answering sits on `exec_cowrite` because that is where it already sits
on the PTY path: a permission prompt is answered by keystrokes, and `session
send` is `exec_cowrite`. Putting it on `exec_control` would have invented a
distinction the rest of the system does not make.

That placement settles exclusivity by derivation rather than by choice: a
cowriter takes no writer slot, so **answering is non-exclusive** — first answer
wins, a second answer for the same `request_id` gets "already answered".

`exec_control` gains nothing for this kind. Gating "may change the standing
policy" was considered and rejected: the documented axis for `exec_control` is
*exclusive ownership of the seat*, not the durability of a decision, and this
codebase's precedent for a power on a different axis is a sibling bit
(`exec_resize` exists for exactly that reason). If the need appears, it gets
its own bit.

**One divergence from the PTY path is permanent and is documented rather than
papered over.** On a PTY, "allow once" and "allow and stop asking" are the same
keystroke class, so `exec_cowrite` can do both and no gate can tell them apart.
A byte stream carries no intent. This is a structural limit of the PTY kind,
not a defect introduced here.

**Verbs stay under `session`** — lifecycle is identical, so a second noun buys
nothing — with a per-kind verdict for every verb, which the implementation plan
must enumerate rather than summarise:

- `session new --kind stream`, `ls`, `kill` — unchanged
- `send` — same verb, different meaning (keystrokes → a user turn)
- `attach` — follow events and take the seat, not a terminal splice
- `snapshot` — last N events rendered, not a VT screen
- `resize`, `exec` — refused as not applicable
- **new**: `session requests <id>`, and
  `session approve <id> <request-id> --allow|--deny|--modify <json>`

**A blocked task must not read as idle.** Rather than adding a `TaskStatus`
value — a wire enum change rippling through three surfaces — tasks carry
`pending=N`. `ls` already prints `cowrite=N viewer=N` with zeros included, so
the column convention exists; zeros are printed here too.

**Notification.** A pending request with no control client attached raises
`notify --level warn`. This is the decision-point-while-autonomous case the
operator already expects to be told about.

**WebUI.** The approval modal renders `tool_name` and the structured `input`
— a diff for `Write`/`Edit` — which is strictly better than the PTY equivalent,
where the same decision arrives as screen bytes. `permission_suggestions`
becomes the button row.

### 4. Approval policy and the unattended default

Mechanism is fixed; policy is pluggable. That split is the whole point of §2.

**Fixed, in the harness:**

- A pending request is always visible, always answerable, and blocks the tool
  call until answered.
- The default is **block indefinitely and notify**. No timeout, no auto-answer.
  A stalled agent costs nothing; a wrongly auto-approved write does not.
- Optional per task: `--approve-timeout DUR` with `--approve-on-timeout
  allow|deny`. Omitted means block.

**Pluggable, behind the adapter:** an adapter may answer locally from an allow
list and never surface the request. "What to do when nobody is watching" is
policy, not mechanism. Trusting the adapter is equivalent to trusting the
runner's configuration — it is a path on the runner host — so this adds no
trust boundary.

**Invariant on that freedom:** an auto-answered request is still emitted as an
event. An approval mechanism whose bypass is invisible is an audit log that
lies.

**Resume.** Pending requests are not persisted. A task blocked when its process
died has no request to restore; after resume the agent re-issues whatever it
still wants.

### 5. Self-approval — PENDING

The default scope is `subtree`, which **includes self**. A task holding
`exec_cowrite` can therefore answer its own approval requests, which makes the
gate self-satisfying.

This was going to be a hardcoded "responder id ≠ requester id" rejection in one
handler. It is instead the motivating case for
[`2026-08-20-per-capability-scope-design.md`](2026-08-20-per-capability-scope-design.md),
where it becomes an ordinary grant: `exec_cowrite` with
`exclude_self = 1`. Nothing in this document should be implemented against the
hardcoded form.

**Blocked on**: that design being reviewed and implemented. When it is, this
section becomes "the default grant for a spawned event-stream task sets an
`exec_cowrite` override with `exclude_self = 1`", and the rule is visible in
`ls`, `whoami` and both spawn dialogs like any other authority.

## Open questions

- The `deny` control response shape is unverified. Probe it before the adapter
  is written; do not infer it from the `allow` shape.
- `hook_callback` and `mcp_message` exist as control subtypes in the binary and
  are unexplored. If hooks ride the same channel, the runner's existing
  `.claude/settings.json` injection may interact with the adapter in ways this
  design has not considered.
- Whether `interrupt_receipt_v1` (advertised in `system/init.capabilities`) is
  the right mechanism for `cancel` against this kind, versus killing the
  process as the PTY kind does.

## Not in this design

- Replacing the PTY kind. It stays exactly as it is; this is a third kind
  beside it.
- Event-stream support for codex or agy. The seam is vendor-neutral by
  construction, but only a claude adapter is designed here, and the neutral
  vocabulary is not to be extended on speculation about a second one.
- Persisting the event stream for replay beyond what the existing per-task log
  already retains.
