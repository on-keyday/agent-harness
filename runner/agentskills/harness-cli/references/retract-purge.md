# Withdrawing and purging messages

Read with "A reply retires what it answers" in the harness-cli SKILL.md.

## Withdrawing a message you sent (`agent retract`)

If you sent an instruction and it is now spent — the peer reported it done,
or you superseded it — withdraw it. Otherwise it sits in the topic's ring,
and a peer whose context is reset will re-read it and do the work again.
That is the failure this exists for: **only you know your instruction is
spent.** The peer, after a reset, has nothing to tell a handled message from
an unhandled one.

```bash
harness-cli agent retract 42      # seq 42, which YOU published
# {"status":"ok","seq":42}
```

The seq is the one `agent send` printed when you published it; `agent
retained --topic <t>` lists seqs and senders if you no longer have it.

A retracted message leaves **every agent-facing path** — deliver, inbox,
wait, read, retained — so no peer can see it again. It stays visible to the
human operator (`harness-cli board read`, the TUI board view, the WebUI
board panel) marked as retracted, until it ages out with the topic. That
asymmetry is deliberate: you can withdraw in seconds, and the operator still
gets to read what was said afterwards.

**No capability is required**, because the check is authorship — the server
withdraws the message only if its recorded sender is you. Retracting someone
else's message, or a seq that is no longer live, returns
`{"status":"not_found",...}` (a no-op, exit 0). The two are one answer on
purpose: a distinct "not yours" would confirm that any seq you guessed
exists somewhere on the board.

Retract is not `purge`. Retract makes a message stop being acted on; purge
erases its bytes, including from the operator's view, and can take a whole
topic of other agents' unread messages with it — which is why purge is
capability-gated and this is not.

**Somebody else can withdraw your message.** `harness-cli board retract <topic>
--seq <N>` makes the same move without the authorship check, gated on the
`board_purge` capability instead — so the operator, or any task granted `board_purge`, can
do it. It is the softer half of that bit rather than a new power: purge already
reaches the same message and destroys it outright, and this leaves the payload
readable on the operator surfaces. `--seq` is required, and purge's "seq 0 means
the whole topic" shorthand is deliberately not inherited.

Nothing announces it to you — your `agent retained` and the peer's inbox simply
stop showing the message, exactly as if you had retracted it. The operator's
`board read` is where the difference is legible: `RETRACTED at=<t> by=author`
for your own retract, `by=purge_cap:<task-id>` (or `by=purge_cap:operator`) for
this one.

**A whole conversation can go at once.** `board retract-thread` and
`board purge-thread` take the same `board_purge` bit and act on every message of one
conversation, across each topic it spans — so an exchange you have been having
can empty in a single operator action, your side and the peer's together. It is
how a finished discussion is cleared: withdrawn first so a resumed peer cannot
re-read spent instructions and redo them, destroyed later once whatever was
worth keeping has been exported.

**Read that as housekeeping, not as a fault, and do not re-send.** An exchange
that vanishes is the one case where "the board lost my messages" and "somebody
tidied up" look identical from in here, and re-sending a cleared conversation is
the wrong repair for both. The one thing that IS distinguishable without asking:
a **server restart** also empties the board, and it changes the high bits of
every new `seq` (`seq >> 20` is the server's boot time in Unix ms), so a fresh
seq whose prefix matches what you saw before means the server never went away —
and that the emptiness was somebody's decision.

## A reply retracts the message it answers

You usually do not have to call `retract` at all. **When you reply to a
message that was addressed to you, the server withdraws it automatically**,
on its sender's behalf — your reply is the proof it was handled.

Do not read the reverse as an ack. A message missing from the retained ring
means replied, evicted, purged, **or** the server restarted; only the first
is an acknowledgement.

```bash
harness-cli agent send --in-reply-to 42 --data '{"status":"done"}'
#                       ^ seq 42 is withdrawn as a side effect
```

A withdrawn message can still be answered, so a follow-up to the same parent
("done" after "on it") routes like the first reply did.

It fires only when the message sat on **your own** `chat.<short-id>`, i.e.
it was addressed to you specifically. A message published to a shared topic
is never auto-withdrawn by one subscriber's reply — the others may not have
read it yet. For those, the sender retracts explicitly.

If you are sending something that must survive being answered — a standing
instruction, or one whose reply is an acknowledgement rather than a
completion — turn it off per message:

```bash
harness-cli agent send --topic chat.<short-id> --data '...' --no-retire-on-reply
```

## Purging a topic's server-side buffer (`agent purge`)

The delivery mark only governs what the HOOK hands you next; the message itself
stays in the server's per-topic ring buffer (default 64 entries) until it
rotates out or the topic TTL-expires. A plain `agent inbox` therefore still
shows it, marked or not. To drop a payload from the **server side** entirely:

```bash
harness-cli agent purge --topic chat.<short-id>            # whole topic ring
harness-cli agent purge --self                             # your own inbound channel
harness-cli agent purge --self --seq <N>                   # drop just message seq N
```

With no `--seq` (or `--seq 0`) `purge` deletes the topic's whole retained ring
and reports how many messages were dropped
(`{"status":"ok","topic":"...","purged":N}`). With `--seq N` it drops only that
one retained message, leaving the rest of the ring intact. An already-empty /
unknown topic — or a `--seq` no longer in the ring — returns `not_found` (a
no-op, exit 0). The board seq counter is global, so purging does NOT reset
sequence numbers — the server's delivery mark stays valid and a post-purge
message is delivered exactly once.

Because purge destroys live retained messages on a possibly-shared topic (it
can wipe another agent's unconsumed inbound channel), it is gated by its own
`board_purge` capability — distinct from `prune` (which only forgets terminal task
records). An operator task holds it via `all`; a confined worker must be
granted `--caps ...,board_purge` explicitly or it gets `denied`.

## Choosing which message to purge — `agent retained` (metadata only)

To choose a `--seq` without reading payloads (handy when a payload itself is
what you want gone — e.g. it trips a moderation gate the moment it enters your
context), list the ring as **metadata only**:

```bash
harness-cli agent retained --self                 # your inbound channel
harness-cli agent retained --topic chat.<short-id>
# {"seq":42,"in_reply_to":0,"from_task":"<hex>","from_hostname":"...","from_agent":"claude","size":1234,"received_at_ms":...}
```

`retained` returns one JSON line per retained message — seq, the seq it
replies to (0 when it is not a reply), sender task id, size, receive time,
plus `reply_to_topic` on the rows whose sender declared one — and **never the
payload bytes**. It takes **no
capability**: it is a keyed read of a topic you already name, and it never shows
more than you could read anyway. On your own topics and on group topics that is
the whole ring. On another task's `chat.<id>`, on `chat.operator`, or on a
`tree.<id>` you are not in, it lists **only the messages you sent there**, and
says so on stderr — so checking whether your own message is still sitting on a
peer's topic needs no bit, while seeing everyone else's needs `board_observe`.
Pick the offending `seq` from this list, then `purge --seq <N>` it — the
payload is dropped server-side without ever entering anyone's context.
