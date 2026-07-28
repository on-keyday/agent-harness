---
name: dummy-harness
description: Use when a change needs checking against a running harness rather than a unit test — interactive wake, TUI/WebUI behaviour across a reconnect, "does the operator actually see this". Covers scripts/dummy-harness.sh, the environment traps that make a dummy instance fail in misleading ways, and the two-instance topology needed to test a client across a server restart. Repo-local: this skill is about THIS repository's binaries and scripts.
---

# Standing up a throwaway harness to verify against

This skill is **not** embedded into other repositories' agents — it is about
this repository's own binaries (`github.com/on-keyday/agent-harness`). For
driving a live session once you have one (snapshot / send / exec), see
`session-debugging`, which is embedded and applies anywhere.

## Why bother

Some properties only exist live and cannot be unit-tested honestly:

- an agent's terminal UI actually submitting an injected keystroke as a turn
  (a cooperating fake proves the plumbing, never the agent);
- what a client does when its server restarts;
- whether an operator can actually see a thing in the log pane.

The alternative — checking against the live fleet — is worse than useless for
verifying a change: its server runs a different build on a different host, so a
green result there says nothing about your code.

## The script

```
scripts/dummy-harness.sh up [--agent claude|fake] [--model NAME] [--detach] [--name N]
scripts/dummy-harness.sh env  [--name N]     # eval this
scripts/dummy-harness.sh down [--name N]
```

Loopback only, ephemeral port, fresh PSK, temp data dir; `down` removes all of
it. `--agent claude` runs a real claude (default model is the cheapest one) —
use it when the property under test involves a real agent's behaviour.
`--agent fake` emits claude's `stream-json` shapes on a timer, for checks that
must not depend on a model or the network.

Always drive it through `eval "$(scripts/dummy-harness.sh env)"` rather than
reading the state file yourself: the `env` output starts with `unset` lines that
matter (below).

## Four traps, in the order you will hit them

**1. Your shell's inherited `HARNESS_AUTH_TICKET` is for the LIVE server.**
Every task shell has one. `harness-cli` prefers a ticket over a PSK, so pointing
it at a dummy server fails with `psk auth: psk: server rejected: BadTicket` —
which reads like a PSK mistake and sends you to check the PSK. The `env`
subcommand emits the `unset`s for this reason; if you construct the environment
by hand, scrub `HARNESS_*` yourself.

**2. `CLAUDE_CODE_*` / `CLAUDECODE` / `AI_AGENT` are inherited too.** A claude
spawned with those set decides it is a child session and silently disables
transcript saving, so `--continue` / `--resume` later find nothing. The script
scrubs them; the symptom if you don't is a banner reading
`⚠ Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker`.

**3. `agent-runner`'s `--max-tasks` defaults to 1.** An interactive session
holds its slot for its entire life, so the moment you open one, every follow-up
task sits `Queued` forever. The symptom is silence — no error anywhere, the task
simply never runs, and `logs` returns nothing. `ls` shows `tasks=1/1` and
`Busy`; check that before debugging anything else. The script passes
`--max-tasks 4`.

**4. A server restart marks in-flight tasks `Failed` with
`err="runner_disconnected"`.** So output stopping after a restart is the task
dying, not the client freezing. Confirm with `ls` before concluding you have
found a client bug — and note that a task which dies on restart cannot be used
to test "does the pane keep updating"; submit a fresh task after the reconnect
for that.

## Testing a client across a server restart

The window you are watching through must not live on the server you are about to
bounce. Two instances:

```bash
scripts/dummy-harness.sh up --detach --name host   --agent fake
scripts/dummy-harness.sh up --detach --name target --agent fake
```

Open an interactive `bash` session on **host** (every instance ships a `bash`
profile), start the client inside it pointed at **target**, then restart
target's server. The host session — and your `snapshot`/`send` access to it —
is untouched.

Inside that host session the shell still carries host's own `HARNESS_*`, so
unset them there before running a client against target:

```bash
session exec "$HSID" "unset HARNESS_AUTH_TICKET HARNESS_TASK_ID HARNESS_SERVER_CID; export HARNESS_PSK='<target psk>'; echo ready"
```

Restarting target's server means killing its pid and relaunching
`harness-server` with the *same* `--listen` port, PSK and `--data-dir`, so the
runner and the client reconnect to what they think is the same server.

## Driving `harness-tui` specifically

General TUI technique is in `session-debugging` — in particular, read the
`reverse` span out of `snapshot --style` instead of guessing where focus is.
Harness-specific facts:

- Focus order is `runners, tasks, logs, notify, cmdresult, cmdline` (6 panes).
  Tab is `+1`, Shift-Tab (`\x1b[Z`) is `-1`.
- **Initial focus is `tasks`.** A reflexive Tab on startup moves focus *off* the
  panel you want, and Enter then lands on the command line instead of following
  a task.
- Enter on the `tasks` panel follows the selected row; the log pane switches
  from `(no task selected)` to live output. That transition is the assertion —
  poll for it rather than sleeping.
- `/` in the log pane filters by substring. To count occurrences of something in
  a log too long to read (checking for duplicated history, say), have the task
  print a unique sentinel and filter for it: the pane then shows one line per
  occurrence.

## The bash profile is your agent-side hands

Every instance gets a `bash` profile alongside the agent one. A oneshot task
submitted to it runs its prompt as a shell command **with that task's
`HARNESS_*` set**, which is the only way to exercise agent-side commands
(`harness-cli agent send`, `agent inbox`, …) without minting a ticket by hand:

```bash
harness-cli --server-cid "$CID" submit --repo "$REPO" --agent bash \
  --task "harness-cli agent send --topic chat.<first8-of-target-task> --data hello"
```

That is also how you deliver an agentboard message to a session you are
watching, since the wake fires on delivery.
