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
  Re-measured 2026-08-21 because every later probe here sent a single turn,
  which makes their `num_turns: 1` a fact about the probe rather than the CLI:
  three turns down one framed stdin produced **three `result` messages under
  one `session_id`**, and turn 3 recalled a codeword set in turn 1, so context
  carries. `exit 0` on stdin close.
- **`num_turns` counts agent-loop iterations for ONE user message, not turns in
  the session.** Each of those three results carried `num_turns: 1`, while the
  deny probe's single turn carried 2 (a tool call plus the reply after it).
  Worth stating because it is easy to read as a session counter and cite as
  evidence for the wrong thing.
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

`-p` reads as the wrong mode for a long-lived multi-turn agent. **Whether it is
required depends on what stdout is**, which took three passes to get right and
is worth the space because the conditional is invisible from either the docs or
a single test.

| stdout | `--input-format stream-json` without `-p` |
|---|---|
| a pipe | **works** — measured at one turn and again at three: three `result` messages, one `session_id`, a codeword from turn 1 recalled in turn 3, `exit 0` on stdin close, line for line identical to the `-p` run |
| a terminal | **refused** — `Error: --input-format=stream-json requires --print.`, exit 1 |

So the help text's "only works with `--print`" is enforced, but only when
stdout is a TTY; with a pipe the CLI behaves as though `-p` were given.

What this section said before, twice, both wrong in different directions: first
that `-p` was required (read from the help, never run), then that it was not
(run, but only against a pipe — and I dismissed the TTY case as unreachable
rather than testing it, which is exactly where the answer changes).

**The harness always pipes**, so either form would work here. Pass `-p` anyway:
it makes the invocation correct independent of what stdout happens to be, which
matters the first time a human runs the adapter's inner command by hand to
debug it. It is also what the vendor's own programmatic spawn passes.

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

### How a session ENDS, measured

There is no session-end message in the protocol (the documented control
requests are `setPermissionMode`, `setModel`, `setMaxThinkingTokens`,
`applyFlagSettings`, `interrupt`, `reconnectMcpServer`, `toggleMcpServer`,
`setMcpServers`, `stopTask`; none ends anything, and the SDK closes a session
with `close()`/abort). So the two candidates are transport-level, and both were
taken on faith until measured. A `SessionEnd` hook was configured to see which
one fires it, because that was the supposed tradeoff:

| ending, sent MID-TURN | exit | the turn | `SessionEnd` hook | time to exit |
|---|---|---|---|---|
| close stdin | 0 | **completes** — `result/success`, full answer | **fires** | 4.7 s |
| SIGTERM | 143 | **aborted** — no `result` at all | **fires** | 1.1 s |

**Both run SessionEnd.** An earlier draft of this section claimed stdin close
would skip the hooks the runner's injected `.claude/settings.json` configures,
and made that the reason to prefer SIGTERM. It was wrong, and it was written
without running either.

With the hooks equal, the remaining difference is the only one that matters and
it is clean:

- **`finish` = close the agent's stdin** — a zero-length Stdin frame, which
  `agentexec.handleInput` already implements and `CommandExecutionStream`
  already exposes as `Stdin().Close()`. The in-flight turn completes and the
  agent exits 0.
- **`kill` = SIGTERM** — reachable today via `ControlType_Signal` /
  `SendSignal`. Immediate, exit 143, work in progress discarded.

That is the distinction §3 wanted and could not name. It needs no new
mechanism on either side.

(Not measured: closing stdin with NO turn in flight. A mid-turn close
completing the turn and exiting 0 makes the idle case the easier one, but that
is an inference, not an observation.)

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

**`exec_view` generalises, and the task log moves under it.** Its catalog text
said "watch a session's **PTY** read-only", which this kind already outgrew.
Extending it to the event stream exposed a gap next to it: a task log is the
agent's output RECORDED — tool inputs, command output, a `Write`'s whole
content — and it was gated by visibility alone, in the same tier as an `ls`
row. So the payload `exec_view` guards live was readable by a caller holding no
capability at all.

Fixed at the source rather than around it: `GetTaskLog` requires `exec_view`
for every kind, oneshot included. The bit now means **observe an agent's
output, live or recorded**; visibility keeps meaning "this task exists and here
is its row".

Visibility is NOT also required for the log. An action scope may deliberately
be wider than the visibility one — `scope=none +exec_view:global` is the
observer that acts on ids it is handed and must not enumerate — and refusing it
the record of a stream it may watch live would be incoherent. The two failures
answer differently, as `authorize` documents: a missing cap is
`permission_denied` (it says nothing about any task), an out-of-scope target
looks absent (or it is an existence oracle).

That is what makes it safe for this kind to render its events into the task log
the way oneshot does — which it needs, because §4's default is to BLOCK, so
nobody being attached is the expected state and the ring evicts. The PTY kind
has no equivalent and cannot: terminal bytes replay wrong without VT state, so
a text log of them would be worse than none.

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

**Lifecycle verbs stay shared; the data plane gets a namespace.** The rule:
a verb stays under `session` if its MEANING does not change, and moves under
`session stream` if it does.

| verb | verdict |
|---|---|
| `session new --stream`, `ls`, `kill` | **shared** — lifecycle is identical, which is what "a second noun buys nothing" was about |
| `session send` | **shared, and stays the low-level one** — raw bytes at a stream, the escape hatch for both kinds |
| `session resize`, `session exec` | **refused** — a window size and a command run inside a PTY; there is no terminal for either to mean anything |
| `session attach`, `session snapshot` | **the CLI verbs refuse and name their replacement**; the ATTACH RPC itself must work, since it is how a client reads the stream at all |
| `session stream turn <id> "text"` | one user turn → `user` |
| `session stream approve <id> <req> --allow\|--deny\|--modify` | → `response` |
| `session stream interrupt <id>` | abandon the running turn → `interrupt` |
| `session stream finish <id>` | close the agent's stdin → `finish` |
| `session stream requests <id>` | read the pending state |
| `session stream attach <id>` | follow events; NOT the terminal splice `session attach` performs |
| `session stream snapshot <id>` | the last N EVENTS rendered — the answer §3 always gave for `snapshot`, which an earlier draft of this table dropped by filing the verb under "refused" and giving it no replacement |

Two things that table gets right only after being wrong. Filing `attach` under
"refused" conflated the CLI verb with the RPC: `AttachSession` is deliberately
open to this kind — the gate reads `IsSessionKind` — because attaching IS how a
client reads events. What must refuse is the local command that hands the
terminal to a PTY splice. And filing `snapshot` there deleted a feature: §3's
verdict for it was always "last N events rendered, not a VT screen", i.e. it
works with a different meaning, exactly like `attach`.

**A client cannot interpret either kind until it knows which one it attached
to, so the attach response says which.** `AttachSessionResponse` gains a `kind`
field. The reason is not that a verb wants to decline politely — that framing
produced "fetch a snapshot first", which breaks a caller whose action scope is
wider than its visibility (`exec_view: global` with visibility `none` sees
nothing in `ls` and would be refused an attach it holds the authority for).
The reason is that terminal bytes and NDJSON are not distinguishable from the
stream itself, and that holds no matter who is asking.

Two alternatives were rejected. Appending an `ok_event_stream` value to
`AttachSessionStatus` is layout-neutral and decode-safe — the generated decoder
casts the byte without validating it, so an old client falls out of its `ok`
branch and declines, which is correct for a client that cannot render NDJSON —
but it overloads a RESULT enum with a task PROPERTY, and the third kind turns
it into `ok_×N`. Letting the STREAM identify itself — the adapter's `hello` is
its first line, retained and replayed on reattach the way the mux already
replays `lastWinSize` — needs no wire change at all, and was recommended here
before the field's cost was priced honestly. It makes the server withhold what
it already holds (`TaskKind` is in the task record) so the client can re-derive
it from payload bytes, and the mux would consult that same field anyway to
decide which tasks get a pinned first frame.

The field's real cost is a layout change, and the skew turned out to be
**asymmetric — measured on 2026-08-21, after this paragraph first claimed the
opposite**. The claim was "`DecodeExactCopy` rejects trailing bytes, so an old
client breaks the moment the server is new"; but the client's live decode path
is the tolerant `Decode` (`cli/client.go dispatchControl`), which leaves
trailing bytes unread, so an OLD client against a NEW server simply ignores
the field and works. The direction that breaks is a NEW client against an OLD
server — the shorter arm fails decoding — and before 2026-08-21 that failure
was a **silent hang**: the response was dropped after a log line, and
`RoundTripTaskControl`'s only other exit is a context most CLI paths pass as
`Background()`. It now routes to the waiting caller as
`cli.ErrResponseUndecodable`, naming the server restart as the fix. Deploy
server-first — the fleet rule already said so, and this is the direction it
actually protects here. On this deployment that is one script plus a WebUI
hard reload, with no third-party clients — cheaper than the new mux state the
alternative needs. (`wire-skew-check` proved none of this: it exercises the
runner↔server HELLO axis, and the runner never decodes this response.)

An earlier version of this list said `send` was "the same verb, different
meaning". That was written before looking at its flags: `-enter` (a carriage
return), `-e` (escape sequences like `\x03`), `--snapshot` (render a VT
screen) are all terminal concepts, and none applies here. A verb whose options
are mostly "not applicable" for one kind is doing two jobs, and the
`--allow|--deny|--modify` grammar does not sit next to a free-text argument
either.

**The verbs under `stream` are one-to-one with the protocol's inbound kinds.**
That is the point of the namespace and the check on it: if a verb has no kind,
it is reaching past the protocol; if a kind has no verb, something is
unreachable. `interrupt` and `finish` are both here because that check found
them missing — `interrupt` was advertised in the adapter's hello with nothing
able to invoke it.

Cost, stated because every surface pays it: `session stream` is a third level,
so the TUI command line and the WebUI's `runCmd` dispatch a namespace rather
than a verb.

**What an answer can be, from the SDK reference rather than from the two shapes
I happened to probe.** `canUseTool` returns two variants with three fields, and
they compose into five operator actions:

```
{ behavior: "allow", updatedInput, updatedPermissions? }
{ behavior: "deny",  message }
```

| action | shape |
|---|---|
| approve | allow, `updatedInput` unchanged |
| approve with changes | allow, `updatedInput` rewritten (`--modify`) |
| approve and remember | allow + `updatedPermissions`, echoing the request's own `suggestions` |
| reject | deny + `message` |
| reject with a suggested alternative | deny + a `message` that guides |

Three things there change decisions taken earlier in this document.

**`updatedPermissions` is not necessarily session-scoped.** §3 records the
suggestion as a session-wide mode change, because that is what the probe
observed: `{"type":"setMode","mode":"acceptEdits","destination":"session"}`.
The documented mechanism also has a `localSettings` destination, which **writes
the rule into `.claude/settings.local.json`** — a file in the task's worktree,
surviving the session. Accepting a suggestion is therefore, in that form, a
file write and a durable policy change, which is a heavier act than the one §3
weighed when it let `exec_cowrite` cover it.

**`--modify` is invisible to the agent.** The reference is explicit: "Claude
sees the result but isn't told you changed anything." So an audit trail that
records only the request lies about what ran. The event stream must carry that
the input was rewritten, and by whom.

**There is a `defer`.** §4 fixes the unattended default as "block indefinitely
and notify" on the reasoning that a stalled agent costs nothing. The docs note
that a callback may stay pending indefinitely, and that a host expecting a slow
human should instead return the `defer` hook decision, which "lets the process
exit and resume later from the persisted session". That is a third option
between blocking and auto-answering, and §4 was decided without it.

**Approvals are serialised by the CLI, so `pending` is 0 or 1.** Measured: a
turn that emitted **three** parallel `tool_use` blocks produced exactly **one**
`can_use_tool` request, and no second one arrived in two minutes of answering
nothing. The rest wait behind it.

That changes what the `<request-id>` argument is FOR. It is not there to
disambiguate between several — there is only ever one to answer. It is there so
a STALE answer is refused: without it, `approve --allow` answers whatever is
pending at the moment it runs, which need not be the thing the operator read in
`requests`. With it, a mismatch is a refusal.

Two consequences follow:

- The id must not be reusable, or it stops being that guard. The adapter mints
  it (the vendor's own id stops at the seam), and a per-process counter —
  `req-1`, `req-2` — repeats after a resume, so a stale `approve req-1` would
  answer a DIFFERENT request. It needs a per-run nonce or a random id.
- `pending` stays a count rather than a flag. One model, one shape of batch was
  measured; a subagent's tools requesting approval alongside the main thread's
  is a different path and is untested, and a count does not lie if that turns
  out to produce two.

**A blocked task must not read as idle.** Rather than adding a `TaskStatus`
value — a wire enum change rippling through three surfaces — tasks carry
`pending=N`. `ls` already prints `cowrite=N viewer=N` with zeros included, so
the column convention exists; zeros are printed here too.

**Notification.** The original text here — "a pending request with no control
client attached raises `notify --level warn`" — never said WHO raises it, and
the answer is not free. `server/await_idle_handler.go` already settled the
principle for the same egress:

> an egress gate that depends on which RPC you arrive by is not a gate

`await-idle --notify` therefore requires `Capability_Notify` of its caller,
even though the fire text is server-synthesized, because otherwise a confined
task could push operator notifications through a path that happens not to check.
A pending-approval notification reaches the same notify-hook, so it cannot be
raised unconditionally without reopening that.

Decided: **the notification is gated, and it degrades honestly.**

- If the blocked task holds `notify`, the harness raises it. Causing an
  operator notification is inside what that task was granted, and the text is
  server-synthesized, so this is a noise vector at worst — the same standing
  `await-idle` has.
- If it does not, nothing is pushed. An operator who wants to be told arms a
  watch themselves, gated by their OWN `notify`, exactly as `await-idle` is.

So §4's default is **block indefinitely**, and notification is a property of
the grant rather than of the kind. The earlier phrasing read as though every
blocked task would announce itself.

That puts weight on `pending=N` being visible without any capability: it is a
`TaskInfo` field, so a task blocked with no `notify` and nobody watching is
still discoverable by looking, which is the floor this degradation rests on.

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
`RunnerHello`), the `TaskKind`, the `kind` field on `AttachSessionResponse`,
the verbs of §3 with their per-kind verdicts, and the three UIs.

## Amendment 2026-08-21: the kind field and the first stream verb shipped

Built, beyond the runner wiring the earlier amendments cover:

- **`AttachSessionResponse.kind`** — set on Ok from the task record; zero on
  errors. The layout changed (a coordinated restart; `wire-skew-check` run
  against the pre-change ref).
- **Every attach caller decides on the kind**, per the enumerate-all-call-sites
  rule: `session attach` (CLI and TUI cmdline) and the WebUI/wasm attach
  refuse a stream task and name the replacement; `session exec` and
  `session resize` refuse (no shell, no PTY — resize would otherwise time out
  into the misleading exec_resize hint); TUI grid panes refuse defensively;
  `session send` and the raw snapshot path stay deliberately kind-agnostic
  (the low-level byte routes of §3).
- **`session stream attach <id>`** — CLI: view-attach, decode the NDJSON,
  render through `streamagent.RenderText`, the SAME renderer the runner's
  task-log tap uses (one renderer, pinned by tests, so the follow view and
  `logs` cannot drift). A non-protocol line on the stream — `session send`
  can lawfully put one there — is shown marked, never dropped. TUI: the verb
  focuses the logs pane on the task, which is already this kind's follower;
  the WebUI follows through its log view (its command input has no `session`
  family at all yet, so the namespace lands there when that family does).
- **`session new --stream` without `-d`** now means open-then-follow on the
  CLI (it previously spliced NDJSON into the raw-mode terminal); the TUI
  refuses the combination and says why. `--stream` rides the TUI cmdline's
  session-new too (it was CLI-only).
- The other `session stream` verbs (`turn` / `approve` / `interrupt` /
  `finish` / `requests` / `snapshot`) are dispatched and answer "specified,
  not built yet" — a namespace that exists with one verb, rather than an
  unknown-verb error that hides the design.

One observation from the smoke run that the plan should not rediscover:
`tool_start` is emitted BEFORE the approval request for that tool, so a
surface shows "Write starting" and then blocks. Count `pending` from requests,
never from tool events.

## Amendment 2026-08-21b: the write verbs, and a TUI chat screen for the kind

**Scope DECIDED (operator, 2026-08-21):** `turn` + `approve` (allow / deny with
a reason / accept a suggestion) + `interrupt` + `finish`, landing together with
a TUI chat screen. `--modify` is DEFERRED — the wire already carries
`UpdatedInput`, so it is additive, and §3's audit consequence ("Claude sees the
result but isn't told you changed anything") is owed when it lands, not now.
`requests` and `snapshot` stay unbuilt. **WebUI is DEFERRED to the increment
after the TUI**, at the operator's direction — a follow-up, not the omission the
previous amendment recorded.

Why `--modify` is not in the first cut, stated so it is not re-litigated as an
oversight: kscale's chat screen (below) has an edit step because its tool
arguments are `{"host":"…"}`-sized. Claude's `input` routinely carries a
`Write`'s whole content, and editing that in a one-line text input is not a
feature, it is a trap.

**The chat screen reads the STREAM, not the task log — and that is forced, not
stylistic.** §3 already says the log is a rendered progress feed. Concretely:
`streamagent.RenderText` renders a request as `⏸ approval needed: <tool>
(<id>)` and **drops `Input` entirely**, and `agentlog.Render` truncates tool
args and results at `maxFieldBytes = 200`. So every surface that reads the log
— `logs`, the TUI logs pane, the WebUI log view, and `session stream attach`
itself — shows an operator a decision they cannot see the subject of. The
`Input` exists verbatim on the stream (`Request.Input`, kept as
`json.RawMessage` for exactly this), so the chat holds its own **cowrite**
attach and decodes the NDJSON, the way `tui/pane_streamer.go` already reads a
grid pane. Cowrite is read AND write, so one attach serves both directions.

Two things follow that change the handoff's dependency order:

- **`stream requests` is NOT a prerequisite for `approve`.** It was listed as
  one because the pending table lives runner-side and reaching it needs
  `pending=N`'s new runner→server message. A surface reading the stream needs
  none of that.
- **Do not "fix" the 200-byte cap.** It is correct for the log, whose payloads
  are unbounded, and the chat is exempt by reading the stream instead. Raising
  it makes the log heavier without making it a transcript, and still cuts the
  case that matters.

**Request-id nonce (was handoff #3, now a prerequisite of `approve`).** The
adapter mints `"req-" + strconv.Itoa(a.seq)` from a per-process counter
(`runner/streamagent/claude.go`), so a resume restarts at `req-1` and a stale
`approve req-1` answers a different request — §3 says the id IS the staleness
guard. Fix inside the adapter: one nonce per run, ids `req-<nonce>-<n>`. No
wire change.

**Measured 2026-08-21, on a live preset-launched runner** (both were assumed
before they were run):

- `session send -e '<json>\n'` drives a real user turn end to end. It is the
  stopgap the write verbs replace, and it confirms the one-NDJSON-line-per-write
  shape works over the existing cowrite path. The `\n` is the operator's to
  supply; `--enter` appends a CARRIAGE RETURN, which is the PTY semantic and
  does not flush the adapter's line buffer.
- **A pending request cannot be evicted from the ring while it is pending.**
  This retracts a concern raised against §4's block-indefinitely default.
  `RingBuffer.Append` evicts from the FRONT (oldest) under a 1 MiB budget and
  always keeps one frame; a pending request is the newest, and the agent is
  blocked, so nothing is producing the 1 MiB that would push it out. An
  operator who returns hours later still reads it with `session snapshot --raw`.

**The TUI screen.** A full-screen overlay beside `GridModel` (same
`IsOpen`/`Update`/`View`/`SetSize` wiring), entered with `r` on a live stream
task — which today falls through to a hint. Borrowed from kscale's
`cmd/katui/chat.go`, which solved this shape already: a `you ▶` text input, a
bounded transcript ring, an elapsed-seconds ticker so a minutes-long turn
visibly advances, activity lines rendered muted against a primary answer line,
and an approval that freezes the transcript and offers its choices as keys.
Two of its hard-won details carry over verbatim — every exit path from a
sub-mode must restore the normal prompt (a leftover editor otherwise sticks as
the prompt forever), and in-progress text is committed to the transcript at
step boundaries rather than only on a finalizing event, or a cancelled turn
loses what was streamed.

Not borrowed: its `edit` step (see above), and its client-side tool execution —
kscale runs probes on the frontend, while every tool here runs in the agent's
own worktree.

## Not in this design

- Replacing the PTY kind. It stays exactly as it is; this is a third kind
  beside it.
- Event-stream support for codex or agy. The seam is vendor-neutral by
  construction, but only a claude adapter is designed here, and the neutral
  vocabulary is not to be extended on speculation about a second one.
- Persisting the event stream for replay beyond what the existing per-task log
  already retains.
