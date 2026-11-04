# Lifetimes: which process you are, and which skill text you read

Read with "You may not be the first process" and "Reading the skills" in the harness-cli SKILL.md.

`harness-cli version` reports the commit this binary was built from, which is
also the version of every skill it PRINTS — they are embedded in it. It does
not describe the copies on disk under `.claude/skills/` and `.agents/skills/`:
the runner writes those at spawn time out of its own binary, so their vintage
comes from a different build and `version` cannot vouch for it. `--json` for
the machine-readable form.

**Expect the two to disagree.** A runner is a long-lived process holding the
embed it started with, so every rebuild after that leaves the injected files
older than what `harness-cli skill` prints. That is the ordinary case — no
container involved — and nothing needs repairing: the next `harness-cli`
invocation already runs the new binary, while the files on disk keep recording
what the agent in this worktree was actually handed. Print the skill when you
want the current text; open the file when you want to know what was injected.

**The podman sandbox inverts it, and there the skew does not self-correct.**
`harness-cli` is bridged in as a single-FILE bind mount, so the container holds
the inode that existed at `podman run` time; a later rebuild writes a
replacement and the running container keeps the old, now-unlinked one — the
rebuild is invisible to it, not merely delayed, and only a NEW container picks
one up (measured, not inferred).
A confined agent can therefore be reading guidance several commits old with no
way to refresh it in place. `version` still names the commit it was built from,
but the repo's HEAD is not visible from in there, so it cannot judge whether
that commit is current — compare the revision against a peer or ask the
operator.

## You may not be the first process on this task

Three lifetimes are in play and only the last one is yours:

- **the task id** — with its worktree and `harness/<id>` branch. Outlives
  everything else.
- **the conversation** — replayed into a fresh process when a session is
  resumed.
- **this OS process** — new every time.

`session new --resume <id>`, and the TUI/WebUI resume actions, reuse the first
two and replace the third, with a **new auth ticket** and possibly a different
runner. Nothing in your environment marks the difference: `HARNESS_TASK_ID`,
`HARNESS_RUNNER_ID` and `HARNESS_AUTH_TICKET` look the same on a first run as on
a resume, and a replayed conversation reads as continuous from the inside. So
"I am the process that started this task" is the default assumption, and it is
silently wrong more often than not.

Each of the three has a knowable start. The harness reports only the first, and
the first alone settles whether you are a later incarnation:

- **The task** — the harness will tell you. `harness-cli ls --json`, your own
  row: a non-empty **`resumed_by`** means this task has been resumed (the value
  is the surface that did it). **`created_at`** is when the id was minted while
  **`started_at`** is refreshed on every (re)assignment, so a large gap between
  them says the same thing independently. Checked 2026-08-09 against 28 live
  tasks: the 26 with `resumed_by` set all showed week-to-month gaps, the 2
  without it showed 0.00 days.
- **The conversation** — a coding agent keeps session history in some form,
  and its first entry dates the conversation, which is a different date from
  either of the others. Where that history lives, and whether reading it is
  permitted, is up to your runtime and your instructions, not this skill.
- **The process** — every OS exposes a process start time, each by its own
  mechanism.

**The server has a lifetime of its own, independent of all three.** Its board
is in memory: a restart drops every retained message and every subscription,
then re-seeds only `chat.<short-id>` for live assignments. The exception is a
topic someone kept with `board retain` (see "Keeping a topic" in `references/topics.md`): its messages
come back, and so do its members (see "Joined topics" in `references/topics.md`), but not its plain
subscribers. Anything else you
subscribed to is gone, and `agent subscriptions` reports the reduced set without
comment. It is detectable without any capability: a board `seq` carries the
server's boot time in its high bits (`seq >> 20` is Unix ms), so a fresh `seq`
whose prefix differs from one you saw earlier means the server restarted in
between.

**What follows from being a later incarnation** is the part that matters. Your
ticket, and possibly your runner, are not the ones the earlier incarnation
used. Do not reason from "what I set up last time": re-read state from disk or
from the server instead of assuming that a subscription, a file, or a process
you remember arranging is still there. The conversation you can see is evidence
about what was *said*, not about what is currently *true*.
