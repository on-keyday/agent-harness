# Reading messages: delivery, one whole, a thread, a topic's history, the body forms

Read with "Inbox is automatic" in the harness-cli SKILL.md.

## How messages reach you, in full

**The split is session vs one-shot, NOT claude vs everything else.** This
paragraph used to say the opposite — that only a Claude agent receives
anything automatically — and it was wrong in the way most likely to mislead
the readers it addressed. Two things it got backwards:

- **The hook file is injected for every runtime.** The runner writes
  `.claude/settings.json` and `.codex/hooks.json` into an injected worktree
  without consulting which agent it is about to spawn there.
- **Waking is gated on having a live stdin, not on a runtime.** A session —
  interactive or event-stream — has one; **a one-shot never does and can never
  be woken**, whatever it runs. That is the real dividing line.

What remains runtime-dependent is narrow: whether anything on your side turns
a new turn into an inbox fetch. Claude Code reads `.claude/settings.json`;
Codex reads `.codex/hooks.json`. Codex requires the project configuration and
the exact hook definitions to be trusted: review them with `/hooks` in its CLI.
New or changed hooks are skipped until trusted; the runner does not bypass
that review. Other runtimes may not consume either file. If no hook fetches
messages, follow the wake prompt and run `harness-cli agent inbox --json`.

Do not conclude from your runtime's name that you are in either camp — check
whether messages actually arrive, and remember that a one-shot task is
push-less regardless.

`harness-cli agent inbox` is wired into both the Claude Code and Codex hooks:

- `UserPromptSubmit` runs
  `harness-cli agent inbox --json --user-prompt-submit-hook`
  (delivers any pending messages at the start of a turn).

That covers every task kind, but how a new turn actually *starts* differs.
An **interactive session** (`session new`) gets a live wake:
when the runner detects new agentboard messages while such a session is idle,
it writes a synthetic `<harness:agentboard-wake>` prompt directly into the
PTY as real keystrokes, which the agent's terminal UI submits as a new turn —
that fires `UserPromptSubmit` and delivers the pending messages just like any
other turn. This works across agent runtimes, not just Claude Code. An
**event-stream session** (`session new --stream`) is woken too: there is no
PTY, so the runner hands the same `<harness:agentboard-wake>` text to the
session as a user turn, and it reaches you the way a turn the operator typed
does. A **one-shot task** (`submit`) has no stdin to write to and gets no wake:
it runs a single turn, so `UserPromptSubmit` fires exactly once, at the
start. If you're a one-shot task and suspect messages have landed since then,
call `harness-cli agent inbox` yourself — nothing will push it to you.

You do NOT need to call `inbox` manually in the common case (interactive
sessions, or a one-shot task that only needs what already arrived at turn
start). The hook already feeds the messages into your context.

**Calling `harness-cli agent inbox` by hand is always safe.** It is a plain,
idempotent read of the rings on every topic you subscribe to: it moves
nothing, so running it twice returns the same thing twice and it can never
take a message away from the hook. Re-read whenever you are unsure what you
were shown. `--since N` bounds a read from below when you are polling and want
only what is new to you (a runtime with no hook does this); it writes nothing
either.

**How the hook avoids re-delivering.** The position lives on the SERVER, per
(task, topic) — how far the automatic injection path has reached for you. Only
`--user-prompt-submit-hook` moves it, and it does so in the same server-side
operation that hands the messages over, so there is no window where a batch was
delivered but not recorded. You cannot advance it by accident: there is no flag
that moves the mark without also producing the hook envelope.

An operator can see exactly where you are — `harness-cli board subscribers`
reports `shown=<seq> pending=<n>` per topic — so "did that message actually
reach the agent" is now something to look up rather than guess at.

## Reading one message whole — `agent read`

```
harness-cli agent read <seq>
```

One retained message, addressed by seq, emitted as a single JSON-Lines
record with the full body. Two things send you here:

- A hook-delivered record with `payload_omitted: true`. Bodies over 64 KiB
  are not spliced into your prompt; the record carries `payload_bytes` and a
  `read_with` command naming this. Run it when you decide the body is worth
  the context — or redirect it to a file and read it in pieces, which is why
  it was withheld rather than truncated.
- A `seq` you already have from somewhere else — an `in_reply_to` on a
  message whose parent you never saw, or a row from `agent retained`.

Unlike `agent inbox`, it fetches nothing but the message you asked for, and
it never truncates. Like a plain `inbox`, it moves no delivery position, so it
is always safe to run by hand.

Reading is limited to topics you subscribe to. A seq outside them reports
the same "not readable" as one that has rotated out of its ring, so it is
not a way to browse topics you have not joined.

`agent read <seq>` sees only the ring. **"Rotated out" is not "gone" on a
retained topic** (see "Keeping a topic" in `references/topics.md`): its older messages are on the
server's disk, and you page them with the topic form instead:

```bash
harness-cli agent read --topic example.topic                  # the 20 newest, newest first
harness-cli agent read --topic example.topic --before <seq>   # the 20 before that seq
harness-cli agent read --topic example.topic --limit 100      # a bigger page (max 65535)
```

`example.topic` is a placeholder — substitute a topic you actually subscribe
to. Topic names are arbitrary: any name you see on the board is an ordinary
topic, not a reserved example, so never assume a name is illustrative.

Same record shape, one line per message, withdrawn messages left out; the
next page is `--before` the last seq printed, and an empty page is the start
of the history. It needs no capability and covers only topics you subscribe
to — including what was posted before you subscribed. You cannot see whether
a topic is retained (`board topics` needs `board_observe`), but this can tell
you: rows coming back from `--before <oldest seq in the ring>` exist only on
disk. No rows proves nothing — a retained topic with fewer than 64 messages
has nothing older either.

## Seeing the whole exchange — `agent thread`

`agent read <seq>` gives you one parent. This gives you the chain.

```bash
harness-cli agent thread                      # every chain you can see
harness-cli agent thread --seq 42             # the chain containing seq 42
harness-cli agent thread --task <32-hex>      # chains involving that task (repeatable)
harness-cli agent thread --headers-only       # rows without bodies
harness-cli agent thread --json               # JSON Lines
```

**No capability is required**, for the same reason `retained` needs none: it
summarizes topics you already subscribe to, and gating it would gate a read
more tightly than the content it reports. A task spawned with no `--caps` at
all can run it. A subscription you can no longer read in full — another
task's `chat.<id>` after `board_observe` was taken away from you — contributes
only the messages you sent there, and the command says so on stderr.

Read what it shows you carefully, because it is bounded by what YOU can see:

- It covers **the topics this task subscribes to**, which for most tasks is
  `chat.<your-short-id>` alone. Your peer receives on its own topic, so its
  half of the conversation — the messages you sent it — is not yours to read.
  A chain therefore often arrives as a **fragment rooted at an ORPHAN**: the
  reply is here, the message it answers is on the peer's topic. That is the
  view working, not a gap in it.
- **Indent marks a fork, not a reply.** A message answered once keeps its
  parent's depth; only a message answered more than once steps right. Every
  row carries `re=<seq>`, which names its parent exactly — read that, not the
  indentation, when you need the linkage. (A back-and-forth is linear, so
  indenting per reply would push a 20-turn exchange 60 columns right and say
  nothing.)
- A message that was answered has usually been **auto-retired** by that reply,
  so it is withdrawn and you will not see it here at all. What you see is what
  is still live on your topics.

The operator's counterpart is `board thread`, which reads every topic on the
board. It needs `board_observe`, so do not reach for it from a confined task:
it will be denied, and the denial names the KIND the request carried
(`BoardTopics`) rather than the verb you typed.

## How a body reaches the receiver — and never reading base64 by eye

The broker delivers `--data` verbatim; the `inbox` / `read` JSON-Lines output
renders it in whatever readable form the bytes have:

- `payload` — **iff the bytes are valid JSON**: embedded as structured JSON
  (not a string), so the receiver branches on fields without parsing anything.
- `payload_text` — otherwise, **iff the bytes are valid UTF-8**: the body as a
  string. Prose, and any other non-JSON text, arrives here.
- `payload_b64` — base64 of the exact bytes. It is the ONLY body a non-UTF-8
  payload has. On the hook path (`--user-prompt-submit-hook`) it is dropped
  whenever one of the two above was emitted: that record is spliced into the
  agent's prompt, where a second copy of the same body costs 4/3 of its size
  in context and says nothing new.

**Never interpret a `payload_b64` by eye.** If it is the only body on a record
the bytes are not text, and the only way to know what they say is to decode
them (`... | base64 -d`, or `agent read <seq>` and decode that). A model that
skims a base64 blob does not read it — it invents a plausible message and then
acts on it, which surfaces as confident work against an instruction nobody
sent. This cost this repo a wrong review before `payload_text` existed: a prose
instruction had no readable rendering at all, so the agent had nothing to read
but the blob.

Alongside the body, every record carries `seq`, `topic`, `in_reply_to` (0 when
the message is not a reply) and the `from` block. Those come from the server,
not from the sender, so they are the fields to branch on — see
"Replying — `--in-reply-to`".

`reply_to_topic` joins them **only when the sender declared one** with
`--reply-to`; its absence is the ordinary case and means a reply comes back to
the sender. You need it for nothing — `--in-reply-to` alone lands correctly —
but if you were about to add your own `--topic` to a reply, this is what you
would be overriding.

Which form to send still matters, though no longer for legibility:

- Send a JSON object whenever the receiver has to **branch** on the message.
  Include a short `"kind"` (or equivalent discriminator) so it can dispatch on
  a field instead of interpreting prose.
- Plain text is fine for anything shaped like a human instruction — it arrives
  as `payload_text`, readable as sent. Only raw non-UTF-8 bytes land as base64
  and nothing else.
