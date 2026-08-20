# Event-stream agents: a third task kind, driven by structured events instead of a PTY

Date: 2026-08-20

**Status: READY FOR A PLAN.** §§1–5 are agreed and unblocked. The three open
questions that gated the adapter were probed against the real binary on
2026-08-20 and are answered below; §5's dependency
([`2026-08-20-per-capability-scope-design.md`](2026-08-20-per-capability-scope-design.md))
shipped. What remains before code is the implementation plan itself — nothing
here has been implemented.

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

### Second probe round — the three open questions, answered

Probed 2026-08-20 against the same `claude 2.1.237`, driving the process over
pipes with no PTY. Transcripts were kept; each claim below is an observation,
not a reading of the `allow` shape.

**`deny` (was: "do not infer it from the `allow` shape").** The envelope is the
same `control_response` with `subtype: "success"` — that field reports the
TRANSPORT, not the verdict — and the payload is
`{"behavior":"deny","message":"…"}`. Three consequences:

- The tool does not run, and the agent does not retry it.
- **The `message` reaches the agent verbatim**, as a `tool_result` with
  `is_error: true`. A deny reason is not swallowed by the CLI; it becomes
  something the model reads and reasons about. So `session approve --deny`
  takes a reason, and that reason is operator-authored text entering an agent's
  context — worth saying out loud, not a private audit note.
- The request carries `description` and `display_name` as well as the fields
  the first probe recorded.

**Hooks do NOT ride the control channel** — the guess in the old text was
wrong in an interesting direction. No `hook_callback` ever arrived. Hooks
surface on the EVENT stream as `system/hook_started` and
`system/hook_response`, the latter carrying `hook_name`, `hook_event`,
`output`, `stdout`, `stderr`, `exit_code` and `outcome`.

The interaction that does exist is the opposite of the one feared: a
`PreToolUse` hook that exits non-zero **preempts the permission request
entirely** — the tool is refused and no `can_use_tool` is ever emitted, so the
operator is never asked. Verified with a positive control, because "no hook
event arrived" is equally consistent with "`--settings` was never loaded": a
blocking hook produced `PreToolUse:Write hook error: …` in the tool result and
zero `control_request`s. Since the runner already injects
`.claude/settings.json`, a hook there is a silent suppression path for the
approval gate, and §4's audit invariant has to cover it.

`hook_response.output` can be large — in this probe it carried a whole
injected-context blob — which is a sizing input for whatever forwards it.

**`interrupt` works and is not a kill.** `{"subtype":"interrupt"}` is answered
with `{"subtype":"success","response":{"still_queued":[]}}` (the
`interrupt_receipt_v1` receipt). The in-flight turn stops and terminates as
`result` with subtype **`error_during_execution`** and zero cost; the process
survives and answers the next user turn normally. So for this kind, `cancel`
has two distinct meanings the PTY kind cannot distinguish: cancel the TURN
(interrupt) and kill the TASK (what the PTY kind does).

One thing an implementer must not be surprised by: **a second `system/init` is
emitted after an interrupt**. A reader that treats `init` as "the session
started" will double-count sessions or reject the stream.

Still unobserved: `mcp_message`. The probe configured no MCP server, so this
says nothing about whether it rides the channel — it remains untested rather
than answered.

### Why `-p`, and what stdin actually carries

`-p` reads as the wrong mode for a long-lived multi-turn agent. **It is a
choice, and a corrected claim**: an earlier version of this section said it was
not one, on the strength of the help text alone — `--input-format <format>` is
documented as *"only works with `--print`"*. That is not enforced. Measured
2026-08-21, same prompt, piped stdin, non-TTY stdout:

| | exit | `result` lines | answer |
|---|---|---|---|
| `-p --input-format stream-json --output-format stream-json` | 0 | 1 | correct |
| the same **without `-p`** | 0 | 1 | correct |

Identical. `--output-format stream-json` is what suppresses the TUI; `-p` adds
nothing here. (Limit of the test: one prompt, one turn, stdout not a terminal.
It says nothing about how the two diverge under a TTY, or across many turns.)

So the reason to pass `-p` is not necessity. It is that the vendor's own
programmatic spawn does, verbatim in the binary (below), and that naming the
non-interactive mode explicitly is worth more than the flag costs. `-p`'s own
help ("Print response and exit") describes the `text` input default, not what
it becomes with a framed stdin.

The vendor's own programmatic spawn, verbatim in the binary, is that pairing:

```
"--print", "--sdk-url", …, "--session-id", …, "--input-format", "stream-json",
"--output-format", "stream-json", "--replay-user-messages", "--resume=…"
```

**Stdin carries two kinds of message** — user turns and control responses — and
that is what forces the framing, not a flag dependency. Measured:

| stdin | result |
|---|---|
| plain text, no `--input-format` | read to EOF as **one prompt**; two lines written seconds apart became a single turn (`num_turns: 1`). This is today's oneshot. |
| `--input-format stream-json` | multiple user turns under one `session_id`, and a control response and a further user turn interleave on the same pipe |

**The misconfiguration fails silently, and loudly enough to matter.** Running
`--permission-prompt-tool stdio` WITHOUT `--input-format stream-json` does not
error. The CLI warns `no stdin data received in 3s, proceeding without it` and
closes stdin; every subsequent tool permission request then fails with
`Tool permission request failed: AbortError: Stream closed`. In the probe the
agent retried Write, Write, Bash, gave up, wrote a paragraph blaming "a
connectivity or session issue" — and the process exited
`result: success, is_error: false`. **A run that accomplished nothing reported
success.** So the adapter validates its own flag set at startup and fails the
task loudly (§2's failure discipline), rather than trusting that a missing flag
would surface.

Two flags found while establishing the above are worth the plan's attention:

- `--replay-user-messages` re-emits user messages from stdin back on stdout
  "for acknowledgment". That is an ack for "the turn was accepted", and it also
  gives a read-only observer (`exec_view`) the text a cowriter sent — which on
  the PTY path is only visible because keystrokes echo.
- `--session-id <uuid>` lets the CALLER choose the session id. **Create only** —
  see the next section before building anything on it.

### Resume: `--continue` works, and `--session-id` is not the resume mechanism

Measured, because the harness's resume path is the one that would have broken.

| | result |
|---|---|
| `--session-id <uuid>` on a fresh session | honoured; `init` reports the chosen id |
| `--continue` with `--input-format stream-json` | **works** — recalled a codeword set in the previous run, and stayed on the SAME session id rather than forking |
| `--resume <uuid>` with `--input-format stream-json` | works, same recall, same id |
| `--session-id <uuid>` naming an EXISTING session | **refused**: `Error: Session ID … is already in use.`, exit 1, no `init` |

So conversation resume needs nothing new: `--continue` composes with the framed
stdin exactly as it already does with the text one, which is what
`resumeOneshotArgv` uses today (`scripts/agent_presets.py`). The existing
`--resume-conversation` option carries over to this kind unchanged, which is
the answer this section was written to settle.

**Correction to the previous section, which this supersedes.** It recorded
`--session-id` as a way to correlate a task id with a session "without parsing
it back out of `init`". That works exactly once. Keying a session id off a task
id would succeed on the first spawn and fail on every resume of that task, with
an exit-1 and no `init` — the fresh path working while the resume path dies is
the failure shape this repo has already had once, when a fresh interactive
launch carried no argv template. If a session id is wanted, take it from `init`
and resume with `--resume <id>`; do not mint it.

`--continue` is documented as "the most recent conversation in the current
directory", and each task owns its worktree, so per-task cwd makes it
unambiguous without bookkeeping. `--resume <id>` is the exact form if a
worktree ever holds more than one conversation.

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

The one suggestion actually observed is
`{"type":"setMode","mode":"acceptEdits","destination":"session"}`, so the core
type carries those three fields rather than a rendered label — a label cannot
be acted on. Only `setMode` has been seen; schema it as a typed entry with an
unknown-`type` entry surviving as an `extras`-style passthrough, because a
suggestion the harness cannot render must not become a suggestion the harness
silently drops from the button row.

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

**That need appeared, in a concrete form, and the bit is still not being
added.** The second probe round found that `permission_suggestions` is not a
per-call allow list — the one observed entry is
`{"type":"setMode","mode":"acceptEdits","destination":"session"}`, a change to
the session's standing permission mode. Accepting it is "stop asking", which is
exactly the durable decision the paragraph above says `exec_cowrite` was not
scoped to.

Decision (2026-08-20, operator's call): **`exec_cowrite` covers it for now**,
and whether it earns its own capability is decided on operational evidence
rather than in advance. The consequence is recorded here rather than argued
away: a holder of `exec_cowrite` can accept one suggestion and stop the gate
asking for the rest of the session, so `exclude_self` (§5) closes self-approval
of individual requests while leaving this second route open. The two are worth
distinguishing when the evidence arrives — self-approving one write is a small
act, disarming the gate is a standing one.

Two things keep that from being invisible while it stands: §4's rule that every
answered request is emitted as an event applies to an accepted suggestion too,
and the mode in force belongs in `session requests` output, so "why did it stop
asking" is answerable without reading a transcript. Revisit when an operator
first wants a task that may answer requests but may not disarm the gate.

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

### 5. Self-approval — UNBLOCKED

The default scope is `subtree`, which **includes self**. A task holding
`exec_cowrite` can therefore answer its own approval requests, which makes the
gate self-satisfying.

This was going to be a hardcoded "responder id ≠ requester id" rejection in one
handler. It is instead the motivating case for
[`2026-08-20-per-capability-scope-design.md`](2026-08-20-per-capability-scope-design.md),
where it becomes an ordinary grant: `exec_cowrite` with
`exclude_self = 1`. Nothing in this document should be implemented against the
hardcoded form.

**Unblocked 2026-08-20.** Per-capability scope shipped (`c8965b3` and the
commits before it), so the grant exists and needs no new mechanism:

```
--caps exec_view,exec_cowrite --scope-for exec_cowrite=subtree-self
```

`subtree-self` is `descendants`: the task may answer its workers' requests and
not its own. It is an ordinary authority, so it appears in `ls` as
`+exec_cowrite:descendants`, in `whoami --json` under `scope_by_cap`, in the
TUI picker and in both WebUI dialogs, and `caps set` can change it live.

One thing that changed under this section while it waited. The design it was
blocked on originally forbade an action rank wider than the visibility rank;
that rule was **removed** (`c8965b3`) as wrong, on a use case this kind makes
concrete — an agent that acts only on ids handed to it over the agentboard and
must not enumerate the server. Nothing here depended on the rule, and the
removal widens what a spawner may grant an event-stream task.

**The default a spawn hands this kind is still `--caps` with no bits**, as for
every other kind. The `exclude_self` grant above is what an operator writes
when they want a task to supervise its workers; it is not applied implicitly.
A task that holds no `exec_cowrite` at all cannot self-approve either, which is
the common case and needs no override.

## Open questions

The three that stood here — the `deny` shape, `hook_callback`, and whether
`interrupt_receipt_v1` is the right cancel — are answered in the second probe
round above. What the answers left open is narrower and no longer empirical:

- **Does "accept a permission suggestion" need its own capability bit?**
  Decided in §3: no, `exec_cowrite` covers it, revisited on operational
  evidence rather than in advance. Listed here because it is a decision taken
  with a known consequence, not a question with no answer.
- `mcp_message` remains untested — no MCP server was configured. If the harness
  ever runs an event-stream agent with MCP servers, probe it before assuming
  the router can ignore that subtype.
- Whether the interrupted turn's `result` (`error_during_execution`) should
  surface as `AgentEvent.Error` or as a `Finish` with a cancelled marker. It is
  a display decision, not a protocol one, and the implementation plan decides
  it rather than leaving it to whoever writes the renderer.

## Implementation status

**The adapter half exists; nothing is wired to the runner.** Built first on
purpose: §2's seam is the part that decides whether the rest is worth building,
and it can be proven without touching the wire or the fleet.

Shipped — `runner/streamagent` + `cmd/harness-stream-adapter`:

- the neutral NDJSON protocol (`hello` / `event` / `request` / `response` /
  `user` / `exit`) with a `protocol_version` in the hello
- the claude adapter: appends the vendor flags itself, refuses an argv that
  already names one, translates through `runner/agentlog` (so the neutral
  vocabulary IS the proven one), owns the vendor↔neutral request-id mapping,
  and carries `--resume-conversation` as an intent rather than a flag
- the §1 extras rules, for the keys the neutrality table names
- unit tests against a fake agent, each negative-controlled, plus a smoke run
  against real claude: allow wrote the file, a second user turn ran on the same
  process, deny did not write, exit 0

**The schema is NOT in `.bgn` yet**, deliberately — see the package doc for the
condition on that. The vocabulary is inherited from `agentlog`, but the extras
rules and the approval types are new, and putting them on the wire before the
shape is exercised buys a migration rather than a design. They move to `.bgn`
the moment any of it reaches the server, a client or the WAL, and the Go
structs are deleted in the same change rather than kept "for the adapter".

Not started, and what the implementation plan has to cover: the runner side
(framer, pending table, `pending=N` on the task, capability declaration on
`RunnerHello`), the `TaskKind`, the verbs of §3 with their per-kind verdicts,
and the three UIs.

One observation from the smoke run that the plan should not rediscover:
`tool_start` is emitted BEFORE the approval request for that tool, so a
surface shows "Write starting" and then blocks. Count `pending` from requests,
never from tool events.

## Not in this design

- Replacing the PTY kind. It stays exactly as it is; this is a third kind
  beside it.
- Event-stream support for codex or agy. The seam is vendor-neutral by
  construction, but only a claude adapter is designed here, and the neutral
  vocabulary is not to be extended on speculation about a second one.
- Persisting the event stream for replay beyond what the existing per-task log
  already retains.
