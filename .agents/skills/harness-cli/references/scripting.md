# `agent wait` and `agent dispatch` — for scripts, never an agent turn

Read with "Async by default" in the harness-cli SKILL.md.

`harness-cli agent wait` and `harness-cli agent dispatch` exist as
shell-level escape hatches for scripting **outside** the agent's turn
loop. The agent itself must not call them.

For those scripts, briefly, so the rule above is the only thing you have to
remember here: `wait --topic T [--since N] [--in-reply-to SEQ]` blocks for a
matching message; `dispatch --topic T TEXT... [--reply-to R]` publishes and
then blocks for the reply to THAT message — on your own `chat.<short-id>`,
where the server routes a reply, or on `R` when you name one, since
`--reply-to` declares the destination and waits there in a single flag.
`dispatch` takes its body the same three ways `send` does, and prints what it
published — `agent dispatch: published N bytes from <source> as seq S,
delivered_to N` — on **stderr** before it starts waiting, so a body that went
out empty shows up there rather than as a timeout minutes later; stdout stays
the reply's JSON Lines. Neither writes any persistent position — `--since` is
the caller's own resume point. A wait does not wake the task it belongs to for
the topic it is waiting on: the waiter is already being handed the message, so
a wake on top of it would type a `<harness:agentboard-wake>` prompt into an
agent that has nothing new to read.

**`wait` is "take everything after the cursor, block only if there is
nothing" — not "wait for the next one".** It scans the retained ring BEFORE it
blocks, so anything already there above `--since` comes back at once, and it
comes back as a BATCH rather than one message. With `--since` omitted the
cursor is 0, which the whole ring is above: on a topic that already holds
messages, `wait` returns instantly with old ones and never waits at all.
Measured: two pre-published messages came back together in 133 ms.

That is why **a script meaning "wait for what happens NEXT" must pass
`--since`** — the previous response's `next_cursor`, or a seq you already hold.
Leaving it off does not look like a mistake: something IS returned, so the call
reads as a successful wait for a message that in fact arrived earlier. It cost
this repo a wrong diagnosis: a wake-suppression check ran `wait` with no
`--since`, the wait exited on an old message before the publish under test, and
the resulting wake was read as the suppression being broken.

`dispatch` is built on the same scan and is safe from it in both directions: it
sets `--since` to the seq it just published and filters on `in_reply_to`, so
nothing retained beforehand can satisfy it — and a reply that arrives before its
wait is even registered is still found, because the scan looks at the ring
first. The property that traps a hand-written `wait` is the one that makes
`dispatch` not miss a fast peer.
