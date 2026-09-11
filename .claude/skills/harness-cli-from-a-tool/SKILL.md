---
name: harness-cli-from-a-tool
description: Use when bolting harness-cli onto a tool you already have — adding a "send this to an agent" button to a viewer, a dashboard, a dispatch script. A recipe for the additive part: the send call, getting a destination list, the three fields to read back, the size ceiling that fails silently, cleanup, waiting for a peer without holding your tool open, and what to tell the human when the handshake is rejected (which rejections mean restart me, and the one that must not). Everything about the agentboard itself is the harness-cli skill; read that for anything this does not cover.
---

# Bolting harness-cli onto your own tool

You have a tool. You want it to reach an agent. This is the additive part —
what to call, what to read back, and the places it goes wrong quietly.

For what the agentboard IS — topics, replies, subscriptions, retract
semantics, the trust model — run `harness-cli skill harness-cli`. Nothing
here repeats it.

## 1. Send

```bash
harness-cli agent send --topic chat.<first-8-hex-of-task-id> --data - < body.txt
```

**Always `--data -` with the body on stdin, never as an argument.** In argv the
shell substitutes `$VAR`, backticks and `$(...)` before harness-cli sees them,
and credential-bearing environment is in scope for that — so this is a
disclosure hazard, not only a formatting one. (A body starting with `-` is also
re-read as a flag; one rule covers both.)

## 2. Get a destination

The address is `chat.` + the first 8 hex of the task id. Nothing else is
needed to reach a live task.

```bash
harness-cli ls --json    # skip status succeeded|failed|cancelled
```

**`ls` shows only what your visibility rank allows** — a confined caller sees
its own row and nothing else. So a tool cannot treat `ls` as a directory it
can always enumerate. Design for the destination to be **given** to your tool
(passed at spawn, typed by the operator, configured) and treat `ls` as a
convenience for callers that happen to see more.

Whether anyone is actually listening comes from **`board topics`**, and only
from there. `agent topics` is not the same listing under another name:
it carries no `subs` field at all, and it omits topics that have never been
published to — which is exactly the state a freshly seeded
`chat.<short-id>` is in. Both need `board_observe`, and without it the server
answers denied: an error, not an empty board.

So treat the subscriber count as an upgrade. Degrade to an unlabelled list,
and never render "nobody is listening" out of a call that failed or out of a
topic that simply is not in `agent topics`.

## 3. Read back all of it

```json
{"bytes":722,"delivered_to":1,"seq":1875942702654160897,"source":"stdin","status":"ok"}
```

| | |
|---|---|
| **exit code** | the only failure signal |
| `delivered_to` | `0` = the topic exists and nobody is listening |
| `bytes` | whether the body you meant actually went out |
| `seq` | keep it — retract and correlation both need it |

**stderr is not the error channel.** harness-cli writes connection INFO lines
there on completely successful calls, so "stderr is non-empty" is not failure
— and `2>/dev/null` throws away the real diagnosis when there is one, because
capability denials arrive on the same stream, mixed in with the INFO.

```python
r = subprocess.run([CLI, *args], input=body, capture_output=True, text=True, timeout=60)
ok = r.returncode == 0          # not: not r.stderr
```

Denial text also differs between the two surfaces for the same missing
capability (`topics denied: requires capability "board_observe"` versus
`permission denied: BoardTopics requires capability board_observe`), so
matching on the string breaks on one of them. The exit code does not.

Treat "no harness-cli on PATH" and "no server" as ordinary states with a
reason a human can act on, not as a stack trace.

## 4. Anything large

Over **64 KiB** the send succeeds, is acked, reports `delivered_to: 1` — and
the recipient is woken with **no body**. The record carries `payload_omitted`
and a `read_with` command instead, and nothing reads it until someone runs
that. Over 1 MiB the send is refused outright, which is the safer failure.

So measure the composed body and show the size in your UI before sending. See
it once, on your own topic:

```bash
head -c 70000 /dev/zero | tr '\0' 'x' | harness-cli agent send \
    --topic chat.<your own short id> --data -
```

The ok line looks entirely normal; the wake that follows carries no text.

## 5. Clean up after yourself

```bash
harness-cli agent retract <seq>
```

No capability — the check is authorship, so you withdraw only what your own
process sent. (`board retract` is the operator surface and needs `purge`.)
Probe and test messages on someone else's topic are litter; withdraw them when
the probe is done.

## 6. Waiting for a peer, without holding your tool open

```bash
harness-cli session await-idle <task-id>                          # blocks until quiescent
harness-cli session await-idle --topic chat.<your short id> <task-id>   # arms, returns now
```

Fires once when that session's PTY output goes quiescent. Which form you want
follows from the shape of your tool, and the blocking one is the common case:

- **A script driving a session step by step** — send, wait for the turn to
  end, read, send the next thing — wants the **blocking** form. That is what
  it is for, and an event-driven rewrite buys nothing.
  **But it takes no `--timeout`** (`--threshold-ms` sets the quiescence
  threshold, not a deadline), so it waits indefinitely on a peer that never
  goes quiet. Bound it yourself — `timeout 300 harness-cli session
  await-idle …` — or one stuck peer hangs the loop.
- **A request handler, or anything with its own event loop** should not hold a
  request open for a peer's turn. `--topic` arms a server-side sink and
  returns immediately; the fire arrives as a board message. This only pays off
  if you already consume the board — otherwise you have built an event loop to
  avoid a wait.

If your tool types into a session, **a clear needs a wait on both sides**:
before it, or the keystrokes land on a screen that is still working; after it,
or the next thing is sent before the clear has taken effect.

The mechanics of injecting keys — why Enter is a second call, why `/clear`
must go over `session send` and not the agentboard — are the
`session-debugging` skill. Driving a worker's whole lifecycle is
`supervising-workers`.

## 7. If your tool is long-lived

`agent wait` and `agent dispatch` are blocking, which is why an agent turn must
not call them. A background script has no turn to freeze, so there they are
fine — but read that skill's warning about `wait` before you use it: without
`--since` the cursor is 0 and it returns old messages instantly instead of
waiting, which is precisely a script author's trap.

`HARNESS_TASK_ID`, `HARNESS_AUTH_TICKET` and friends are whatever the shell
that started you had exported. They name the task you were launched **under**
— whose credential you present — not the task your tool acts **on**. Do not
treat the inherited id as "self"; if you want that task marked in a list,
label it rather than filtering on it.

The ticket does not outlive the agent process it came from. A new process, or
a resume, means a new ticket — and a resume replaces the runner id along with
it, so the two must be refreshed **together**. Read them from the environment
at the moment of use and write them nowhere: not to a config file, not to a
unit file, not into your own cache.

## 8. When the handshake is rejected, say which kind

`harness-cli whoami` is the probe to open with. It needs no capability, and it
answers with the principal your tool is actually acting as, the caps and scope
the **server** will enforce, and the commit the **server** is running — the
last of which nothing else on any wire carries (`version` answers for your
local binary, a different process). Call it at startup and you learn whether
your tool can reach the harness before a user action depends on it.

A rejection arrives as `psk: server rejected: <Status>` on a nonzero exit.
Keep reading the exit code for pass/fail (§3); read the status name only to
choose what to tell the human, because the branches lead opposite ways:

| status | what your tool should do |
|---|---|
| `BadPsk`, `BadTicket`, `Expired` | fatal — **ask the human to restart your tool** |
| `NotPermitted` | fatal, and a restart will not help: the grant is live but does not cover this call |
| `NoIdentity` | **not** a credential failure — back off and retry |

`cli/persist.go`'s `Retryable()` draws exactly this line, and `NoIdentity` sits
on the far side of it because it is what a version-skewed server answers when
it cannot decode a hello it is too old to understand. That clears itself the
moment the server is upgraded. Treating it as fatal is what emptied the fleet
on 2026-07-16 — every runner exited within about a second and none came back.
So do not collapse the five into one "auth failed, tell them to restart".

**Why a restart, and not a re-read.** The credentials come from the
environment your process was handed at launch, and a long-lived parent freezes
that environment for everything it later starts. Nothing in-process refreshes
it: not re-reading a config file, not a reload verb, not reconnecting. Only a
new process from a current shell picks up current values — which is why the
message that helps is "restart me", not "check your credentials".

**And `BadTicket` is not always about the ticket.** The PSK gate has no
`UnknownTask` arm, so a task that no longer exists is reported as `BadTicket`
too. If a harness-server restart is anywhere in the timeline, read it as "that
task is gone; resume it" before suspecting rotation or PSK configuration. What
a resume then does to the two credentials is §7.
