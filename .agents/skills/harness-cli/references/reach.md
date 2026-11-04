# Who you can reach, and who can read you

Read with "Reaching another agent" in the harness-cli SKILL.md.

Without any capability a task publishes to its **base set**:

- its own `chat.<id>`, and `chat.<id>.<suffix>` side channels;
- its **creator's** `chat.<id>` and each **direct child's** `chat.<id>`;
- `chat.operator`;
- the **reply route of any message addressed to it** — one that arrived on its
  own `chat.<id>` / `chat.<id>.*`, or that replies to a message it sent: the
  sender's `chat.<id>`, or the `reply_to_topic` the sender declared. Someone who
  spoke to you first can be answered whoever they are. Reading a group topic,
  or another task's topic with `board_observe`, does NOT make its posters
  answerable, and neither does a message sent with `agent send --no-contact` —
  the flag for a sender that will read no answer, such as a tool publishing in
  a task's name. Three more limits: a reply route that is a `tree.<id>` you are
  not in stays closed; only 64 such routes are kept, and the one that has gone
  longest without writing to you again is dropped first; and a server restart
  forgets them — as can the operator, one route or all of them
  (`board clear-contacts`). `harness-cli agent contacts` lists the ones you
  hold now, newest first (JSON Lines, no capability), so check it rather than
  assume a route you answered earlier is still open;
- `tree.<full-32-hex-id>.<suffix>` for every task id on its own ancestor chain,
  itself included;
- a topic it is a **member** of — one the operator, or a task holding
  `board_member`, joined it to (see "Joined topics" in `references/topics.md`).

Anything else — a peer's `chat.<id>` you have no thread with, a free-form topic
like `build.events` — needs the `board_peer` capability. A send outside your
reach is refused, not dropped, with the ordinary capability denial plus the
topic: `permission denied: AgentSend requires capability board_peer: <topic> is
outside this task's reach …`, exit non-zero. Read it as "this topic needs
board_peer", not "every send does" — the same line covers a send to a peer and
not one to your creator. It is a fact about your grant, not a fault to work
around: tell whoever spawned you (or the operator) which topic you need, and
they can grant it on the live task.

`board_peer` is **scoped like every other capability**: it opens another
task's `chat.<id>` only when that task is inside the bit's scope, and a
free-form topic only when that scope is global. So the grant that lets you
report to exactly one task is `caps set <your-task> --caps …,board_peer
--scope <your current scope> --scope-for board_peer=ids:<that-task>`, and it
reaches nobody else (`caps set` rewrites the whole scope, so it needs the base
restated and every other override it should keep; at spawn the flag alone
works). Reaching every peer and every free-form topic is `--scope-for
board_peer=global`. A
refusal that names `board_peer` while you hold it means the target is outside
its scope — `harness-cli whoami` shows yours (`scope_by_cap` in `--json`).
The operator sees your contacts too, on `board subscribers`. No bit opens a
`tree.<id>` you are not in.

Reading is open except for **other tasks' direct messages**: another task's
`chat.<id>` / `chat.<id>.*`, `chat.operator`, and a `tree.<id>` you are not in.
Subscribing to, waiting on, or `read`ing those needs `board_observe`; without
it the subscribe and the wait are refused with a message naming that bit.
`retained` on them lists only what you sent (see "Choosing which message to
purge" in `references/retract-purge.md`). Group topics — anything outside `chat.` and `tree.` — stay readable
by anyone who names them.

Two naming conventions follow from that:

- **A confidential side channel** is `chat.<your-short-id>.<suffix>`. Others
  cannot read it without `board_observe`, and a peer you asked can still answer
  there, because a declared `--reply-to` is the reply route of the message it
  received. Confidential means unread, not unwritten: any task holding
  `board_peer` can publish there, so a row on it is not proof it came from
  the peer you asked — read `from.task_id`. A free-form `--reply-to` name works too, but anyone who guesses it
  can read it — and declaring one needs `board_peer`.
- **A team channel** is `tree.<your-full-task-id>.<suffix>`: you and every task
  below you in the spawn tree read and write it; nobody else writes it, and
  nobody else reads it without `board_observe`.

**A `--reply-to` must be a topic you could publish to yourself.** The route you
declare becomes a contact of whoever it is addressed to, so a route outside
your reach is refused with the same `board_peer` denial as a send there. Your
own `chat.<short-id>.<suffix>` is always in reach.

Membership is decided on every read and publish from the live spawn tree and
the live grant, so `caps set` and `caps set-parent` take effect at once, and a
subscription you lose the right to read simply stops delivering (it comes back
if the right does).

Every delivered message carries `from.agent`: the agent profile the sending
task was running under at publish time (`"claude"`, `"codex"`, …), attested by
the server — the sender cannot set it, and it is frozen per message, so a task
resumed under a different runtime does not relabel what it already sent. Check
it before assuming your reply is read promptly — but do not read it as a
verdict: the injected hook fires for any runtime that reads
`.claude/settings.json`, and a one-shot task of ANY runtime, Claude included,
is push-less because it has no stdin writer to wake. What `from.agent` tells
you is which runtime answered, not whether it was pushed. An
empty `from.agent` means the server could not attribute a runtime.

Every record also carries `sender_kind`, set by the server: `agent` for a peer,
`operator` for a message published **in the operator's name**, `server` for a
server notice such as an `await-idle` notification. Read it before
`from.task_id`: an operator or server message may carry an all-zero task id,
which names no task and no `chat.<short-id>` to answer on.

`operator` with an all-zero `from.task_id` is the human (TUI, WebUI or CLI).
`operator` with a non-zero `from.task_id` is the task that id names, which the
operator granted `board_send` to speak for them — a delegate, not the human
typing. The server stamps both; neither can be forged from a plain `agent
send`.
