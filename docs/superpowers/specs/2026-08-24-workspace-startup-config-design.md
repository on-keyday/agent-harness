# A client restores its workspace on start, on reconnect, and on demand

Date: 2026-08-24

## Problem

Every `harness-tui` start, and every reconnect after a drop, repeats the same
manual sequence: pass the connection flags, bring the long-lived task back if it
died, re-establish that task's port forwards one spec at a time, and reopen the
grid over the sessions being watched.

- **The forwards are not optional work after a reconnect — they are always
  gone.** A forward's lifetime is the lifetime of the client connection holding
  its control stream. For `-L` and raw forwards the control stream's EOF is what
  deregisters the forward (`cli/forward_endpoint.go:19-32`). For `-R` the runner
  is the listener, but each accepted connection is dialled **on the client
  side**, and closing the control stream is what makes the server send
  `ClosePortForward` to the runner (`cli/port_forward.go:504-521`). A client that
  goes away takes every forward with it.

- **The task itself can be gone after a reconnect, and that is exactly when it
  is wanted back.** Replaying the WAL on server start rebuilds Queued tasks and
  marks interrupted Running tasks as **Failed** (`server/server.go:446-448`). So
  the reconnect that follows a server restart finds the task terminal. A plain
  network blip does not: the task stays alive and no resume is warranted.

- **There is nowhere to write any of this down.** This repository has no config
  file mechanism. `grep -rn "XDG_CONFIG\|UserConfigDir" --include=*.go .` returns
  no hits; the only XDG use is `XDG_CACHE_HOME` for an agent's board cursor
  (`cli/agent/util.go:9`). Every client-side startup value is a flag, so the
  connection values are retyped and the rest is redone by hand.

- **The values that would be written down are not typeable by hand.** A task id
  is 32 hex characters, and a forward spec is something the operator produced
  interactively by pressing `p` in the TUI. A config the operator must author in
  an editor moves the work rather than removing it.

### The server cannot do any of this for us

A declaration held server-side could never enact itself: both forward directions
terminate in a client process (the two citations above), and a resume needs a
client to ask for it. Whatever holds the declaration, the actor is a client.
That is why this is a client-side file and not a protocol change. This design
adds no `.bgn` field, no server state, and no new RPC.

## Scope

IN:

- `.harness/config`, a line-oriented file read by `harness-tui` and
  `harness-cli`.
- A **workspace**: a named set of connection values, a grid selection, and — per
  task — whether to resume it, which runner may take it, and which forwards to
  establish.
- One apply routine in the TUI, reached from three paths: start, reconnect, and
  the `workspace apply` command.
- Writing the file from the TUI and the CLI (`workspace save`), so no id or
  forward spec is ever hand-typed.
- `workspace ls` / `workspace show`.

OUT, each with its reason:

- **WebUI (buttons, command input, wasm bridge).** A browser has no file to
  read, which is the whole reason this design is client-local rather than
  server-held. Independently, a browser cannot listen, so `-L` has no
  equivalent there: the WebUI's analogue is a preview pin
  (`cli/preview_forward_wasm.go:50`), which binds nothing locally, and `-R`
  (dial-back to the viewer's own host) means nothing on a phone. A workspace
  applied in the WebUI would therefore mean something different from the same
  file applied in the TUI.
- **`harness-cli forward` reading a workspace's `forward` lines.** That verb
  takes its specs as arguments; mixing an implicit source in makes "which one
  took effect" unanswerable.
- **Server, runner, and `.bgn`.** Unchanged.

## The file

### Location

Resolution order, first hit wins:

1. `--config <path>` — an explicit path. A missing file here is an error.
2. `HARNESS_CONFIG` — same, an error if the named file is missing.
3. `./.harness/config` — the default. A missing file here is silent: running
   without a workspace is the normal case.

**DECIDED (2026-08-24)** — the default location is the current directory only.
The search does not walk up to parent directories. An operator who runs a client
from somewhere else names the file with `--config`.

**DECIDED (2026-08-24)** — the config is not read at all when
`HARNESS_AUTH_TICKET` is set. That variable is how an in-task agent is
identified; `cliopts.ResolveAuthTicket` requires it and accepts no flag
fallback (`cli/cliopts/cliopts.go:28-41`). The reason is a specific leak path:
`scripts/sandbox/agent-in-podman.sh:385` forwards environment into the container
**by prefix** (`case "$name" in HARNESS_*) CLI+=( --env "$name" ) ;;`), so a
`HARNESS_CONFIG` present in a runner's environment rides into every sandboxed
agent, where that path either does not exist or names a different file. The
observable failure without this rule: an agent's `harness-cli` invocation fails
on a config it was never meant to read, or silently picks up an operator's
connection values.

`.harness/` is added to `.gitignore`. It sits with `harness-data/`, `bin/`,
`.harness-worktrees/` and `.playwright-mcp/` — this repository keeps
machine-local state in gitignored directories at the repository root, and the
values here include a LAN hostname in `server-cid`.

### Grammar

```
# comment; blank lines ignored

[workspace <name>]
key = value

[workspace <name> task <32-hex-task-id>]
key = value
```

A section header is split on whitespace and matched by token, so the binding of
a task's keys to a workspace is written in the header rather than implied by
position in the file.

Keys under `[workspace <name>]`:

| key | value |
| --- | --- |
| `server-cid` | as `--server-cid` |
| `ws-path` | as `--ws-path` |
| `repo` | as `--repo` |
| `grid` | the argument string of the `grid` command, verbatim; empty means the unnarrowed grid |
| `ssh-gateway` | the gateway's listen address; empty means its default bind. An absent key means "do not touch the gateway" |

Keys under `[workspace <name> task <id>]`:

| key | value |
| --- | --- |
| `resume` | `no` (default) \| `continue` \| `fresh` |
| `runner` | `assigned` (default) \| `any` |
| `forward` | the `-L …` or `-R …` argument of `harness-cli forward`, verbatim; repeatable |

`resume` and `runner` are the two axes of the existing keys: `r`/`R` are
assigned-runner continue/fresh and `u`/`U` are the any-runner pair
(`tui/keys.go:78-81`), so the four combinations of these two values are the four
keys.

Example:

```
# .harness/config

[workspace default]
server-cid = ws:HOSTNAME:8539-*
repo       = /abs/path/to/repo
grid       = --under 3f2a9c...

[workspace default task 3f2a9c...]
resume  = continue
runner  = assigned
forward = -L 3000:127.0.0.1:3000
forward = -R 8080:127.0.0.1:8080
```

**DECIDED (2026-08-24)** — an unknown key, an unknown section header, or a bad
enum value is a parse error, not a skipped line. The observable failure this
prevents: `fowardd = -L 3000:…` establishing nothing while the file looks
correct.

**DECIDED (2026-08-24)** — no PSK key. The secret keeps its two existing homes
(`HARNESS_PSK`, `HARNESS_PSK_FILE`, `cli/psk.go:21-26`) and gains no third.

**DECIDED (2026-08-24)** — line-oriented, not TOML or JSON. `go.mod` has no
TOML or YAML parser and adding a dependency needs the operator's approval; JSON
carries no comments and would force quoting on values like
`-L 3000:127.0.0.1:3000`. The line format also lets `workspace save` replace one
workspace's lines and leave every other line, comment included, byte-identical.

**DECIDED (2026-08-24)** — `forward` and `grid` values are passed to the
existing parsers rather than re-parsed here: `cli.ParseForwardSpec` /
`cli.ParseRemoteForwardSpec` for a forward, `parseGrid` (`tui/cmdline.go:1316`)
for the grid string. The config layer splits the value with `shlex` and hands it
over. One grammar, one parser.

## Applying a workspace

### Trigger

Application runs on `SnapshotMsg` (`tui/client.go:391-401`), because the decision
of what a task needs depends on that task's status, which the snapshot carries.
`SubscribedMsg.Resubscribed` already distinguishes a first join from a rejoin
(`tui/app.go:610-623`), so no new connection-state tracking is needed.

**DECIDED (2026-08-24)** — start, reconnect, and `workspace apply` run the
identical routine, including `resume`. Two facts settle this. A network blip
leaves the task alive, and the resume branch only fires for a terminal task
(`resumeReattachAction`, `tui/taskaction.go:31-57`), so a blip cannot spawn
anything. A server restart leaves the task Failed (`server/server.go:446-448`),
which is precisely when it should come back. Making reconnect the exception
would have skipped the resume in the one case that most needs it.

### Steps

Per task block, in order — but see the Amendment: "in order" was written as if
the steps were sequential, and the TUI's command runner is concurrent.

1. **`resume`** — when `resume` is not `no` and the task is terminal, call
   `DoStartDetachedSession` with `resumeTaskID` set to the block's task id and
   `resumeConversation = (resume == continue)` (`tui/interactive.go:269-283`).
   `runner = assigned` pins the selector to the task's runner; `any` leaves it
   unpinned. This path starts the session and closes its local stream — its own
   comment records that nobody attaches to it — so nothing takes over the
   operator's screen.
2. **`forward`** — start each spec against that task, through the existing
   `DoStartPortForward` / `DoStartRemoteForward` on the long-lived `a.client`
   (`tui/portforward.go:260`, `:285`).

Then once, after every block:

3. **`grid`** — `openGrid(mode, anchor, ids)` from the parsed `grid` value
   (`tui/app.go:2457`). Grid panes attach in `AttachMode_Cowrite`
   (`tui/pane_streamer.go:225`), which the schema defines as non-takeover,
   coexisting with one control and any number of viewers
   (`runner/protocol/message.bgn:954-958`). Opening the grid therefore takes
   control away from no one — not from another client, and not from the
   operator's own WebUI on another device.

There is no attach step and no attach key in the file. Two observable failures
rule one out. A control attach splices the operator's terminal to one session's
PTY, so a workspace naming two tasks would have the second block's attach
replace the first block's handover — the file would describe a state the client
has no way to be in. And `AttachMode_Control` is takeover among controls
(`runner/protocol/message.bgn:954-958`), so an automatic control attach on every
reconnect reclaims a session another client had taken: a phone that picked the
session up during the outage loses it the moment the TUI's network returns.

### Idempotency: apply reconciles, it does not restart

**DECIDED (2026-08-24)** — an apply compares the declared forwards against the
workspace-owned forwards already running and acts only on the difference:

- a declared spec already running as a workspace-owned forward is left alone;
- a declared spec with nothing running is started;
- a workspace-owned forward whose spec is no longer declared is stopped.

Not "tear everything down, then re-establish". The intended recovery from a port
conflict is to free the port and run `workspace apply`, and a
teardown-then-rebind would drop the forwards that were working in order to
retry the one that was not — breaking live connections through them, and racing
this client's own closing listener for the port it is about to rebind.

On a reconnect the running set is empty (the connection that held every control
stream is gone), so the same rule reduces to starting all of them.

`PortForwardSession` (`tui/portforward.go:98`) gains a field marking a session as
workspace-owned. It is client-local bookkeeping for reconciliation and is not
reported anywhere: the server-side registry has no notion of a workspace and
inventing one would mean a wire field. Forwards the operator started by hand are
not workspace-owned and are therefore never stopped by an apply.

### Failure

A parse error is reported on stderr and exits 2, before the TUI enters the alt
screen. It is an authoring mistake and is fixed immediately.

Every other failure — a local port already bound, a task no longer present, no
runner available — appends one line to `a.cmdresult` and the apply continues to
the next item. The environment being wrong must not cost the operator the
client. Each line names its subject and what happened, e.g.
`workspace default: -L 3000:127.0.0.1:3000 on 3f2a9c… failed: listen tcp
127.0.0.1:3000: bind: address already in use`, never a bare count.

**DECIDED (2026-08-24)** — one `cli.RunForward` call per declared spec, never
one call with a slice of them. `RunForward` aborts every spec in the call when
any one of them fails to listen (`cli/port_forward.go:215-225`), so a conflict on
3000 would take 8080 down with it. `DoStartPortForward` already passes
`[]cli.ForwardSpec{sp}`, so following it gives per-spec independence.

**DECIDED (2026-08-24)** — a failed forward is not retried automatically. When
the port is held by another program the retries cannot succeed, and
`a.cmdresult` is a 200-line ring that evicts oldest-first, so a retry loop
would push out the error the operator still needs to read. Recovery is: free the
port, run `workspace apply`, which by the reconciliation rule starts only the
missing forward.

**DECIDED (2026-08-24)** — a bind conflict does not fall back to another local
port. `3000` is written because something expects 3000; binding 3001 silently
would leave the operator with a forward that is running and useless.

## Writing a workspace

`workspace save <name>` writes what the client currently has:

- `server-cid`, `ws-path`, `repo` from the live connection.
- `grid` from the open grid's mode, anchor and ids, rendered back into the
  `grid` command's argument grammar.
- One task block per task that currently has workspace-eligible state: the task
  being followed or attached, and every task the registry reports a savable
  forward for.
- `forward` lines from the server-side registry, for those tasks.
- `resume` and `runner` default to `continue` / `assigned` for a saved block, and
  are the two values an operator is expected to hand-edit afterwards.

**DECIDED (2026-08-24)** — one rule, both save paths: write every forward the
registry reports for the task whose `client_endpoint` is `os_socket`, and skip
the `in_process` ones. The property that decides this is a field on the wire,
not a client's private bookkeeping. `PortForwardInfo.client_endpoint`
(`runner/protocol/message.bgn:1757`) separates a forward whose client side is an
OS socket — the listener of a `-L`, the dial of a `-R` — from one that lives
inside a client process: a raw `t` pane, a WebUI preview pin, a `-W` stdio
splice. The schema states that an `in_process` forward's client-side address
pair is empty, which is exactly why no `-L`/`-R` line can describe one.

An earlier draft had the TUI save only the forwards it had started itself,
because the registry also holds forwards other clients established. That was
wrong twice over. There are no other operators on this deployment, so the
forward the ownership rule actually dropped was the operator's own, established
from a `harness-cli forward` in another terminal. And what cannot be written
down is an endpoint kind, not an owner — the ownership rule both dropped
savable forwards and would have kept unsavable ones had a raw pane been the
client's own, which it always is.

**DECIDED (2026-08-24)** — the `-L`/`-R` value is rendered by one function,
`cli.PortForwardConfigSpec(fi) (string, bool)`, used by both save paths and
pinned by a test that feeds its output back through `cli.ParseForwardSpec` /
`cli.ParseRemoteForwardSpec`. The `bool` reports whether the forward is savable
at all, so the `os_socket` test lives with the renderer rather than being
repeated at each call site. The existing `cli.PortForwardSpecString`
(`cli/port_forward_list.go:113`) is NOT that function: it renders
`127.0.0.1:3000 -> 127.0.0.1:3000` for a human reading `forward ls`, and writing
that into a config would produce a line no parser accepts.

**DECIDED (2026-08-24)** — saving replaces only the target workspace's own
lines. Every other line in the file, comments included, is preserved
byte-for-byte. A round-trip test pins parse → render → parse.

The CLI counterpart is `harness-cli workspace save <name> --task <id>`. It reads
the same registry through the same renderer and writes `resume = continue`,
`runner = assigned`. It exists because a workspace has to be authorable from a
machine whose operator is not sitting in the TUI.

Reading the registry rather than a client's own list is also what makes the TUI
save reachable at all from a short-lived process: `harness-cli` holds no
forwards of its own, so a rule phrased in terms of "this client's forwards"
would have had no CLI implementation.

## Surfaces

| surface | verdict |
| --- | --- |
| `harness-tui` flags | `--config`, `--workspace` |
| `harness-cli` global flags | `--config`, `--workspace`; they resolve `server-cid` / `ws-path` / `repo` only |
| `harness-cli workspace` | new verb family: `save`, `ls`, `show` — no `apply`, see below |
| TUI command line | new verb family `workspace`: `save`, `apply`, `detach`, `ls`, `show`, `rm` |
| TUI keybindings | omitted — re-applying is rare and the command line reaches it |
| TUI popups / pickers | n/a — no new modal |
| WebUI controls / command input / wasm bridge | omitted — see Scope |
| server / runner / `.bgn` / WAL | n/a — no wire or server-state change |

**DECIDED (2026-08-24)** — `harness-cli` reads only `server-cid`, `ws-path` and
`repo` from a workspace. `grid` and the per-task `resume` / `runner` /
`forward` are read by the TUI's apply routine alone. This is a difference in
which consumer reads which key, not an option accepted and dropped: no
`harness-cli` subcommand advertises a flag whose value the workspace silently
overrides.

**DECIDED (2026-08-24)** — there is no `harness-cli workspace apply`. A forward
lives exactly as long as the client holding its control stream, so a
short-lived CLI process could only establish one by staying in the foreground
for its lifetime — which is what `harness-cli forward` already is. An apply
whose forwards vanish on exit would not be the same operation the TUI's apply
performs, and giving one verb two lifetimes across two surfaces is how "which
one took effect" becomes unanswerable. The observable gap this leaves: a
headless machine with no TUI cannot restore a workspace's forwards. If that
turns out to matter, the answer is a foreground `harness-cli workspace up` that
holds them and says so in its name, not an `apply` that quietly differs.

The word **workspace** is used rather than *profile* because *profile* already
names an agent preset in this repository — the `--agent` flag's help text is
"agent profile name" (`cmd/harness-cli/main.go:124`), the WAL field is
`agent_profile` (`server/wal.go`), and `scripts/agent_presets.py` holds
`KNOWN_AGENT_PRESETS`. `grep -rn workspace --include=*.go .` returns no hits.

## Tests

- Parse → render → parse round-trips, including a file with comments and two
  workspaces, where saving one leaves the other's lines byte-identical.
- An unknown key, an unknown header and a bad enum each produce an error naming
  the line.
- With `HARNESS_AUTH_TICKET` set, a config present in cwd is not read.
- A `forward` value reaches `cli.ParseForwardSpec` and a `grid` value reaches
  `parseGrid` — asserted by feeding a spec only those parsers accept.
- `resume`/`runner` map to the four `r`/`R`/`u`/`U` combinations.
- Reconciliation: a second apply leaves an already-running workspace-owned
  forward untouched, starts one whose first attempt failed, stops one whose spec
  was removed from the file, and does not touch a hand-started forward.
- A spec that fails to listen does not prevent the remaining specs, the resume
  step, or the grid step from running.
- A save writes an `os_socket` forward, skips an `in_process` one, and every
  value it writes parses back through the parser that owns that value's grammar.

## Consequences the operator should expect

- **A session ended on purpose comes back.** Typing `exit` in a session exits 0,
  which `TaskStore.Finish` maps to Succeeded (`server/taskstore.go:542`), and the
  next apply resumes it. Distinguishing "ended deliberately" from "died" was
  considered and rejected: `Finish` deliberately overwrites a Cancelled status
  with what the agent actually did (`runner/session.go:893-899`), so the status
  cannot carry that distinction. The rule is instead that a workspace declares
  the task should be alive and every apply reconciles toward it. The remedies are
  `resume = no` or removing the task block.
- **Two clients applying the same workspace both bind the local ports.** The
  second one's `-L` fails to bind and says so per line. Nothing prevents this;
  local ports are a property of the machine, not of the workspace.
- **A save can capture a forward another process on the same machine is
  holding.** The registry is the source, so a `harness-cli forward -L 3000:…`
  running in another terminal is written into the file; the next apply then
  tries to bind 3000 while that process still holds it, and reports the ordinary
  bind conflict. This is the price of reading the registry, and the alternative
  — saving only what the saving client itself started — costs more: it silently
  drops the operator's own CLI-established forwards and has no implementation at
  all in `harness-cli`, which holds none. The remedy is the same as any other
  conflict: stop the other forward, or delete the line.

## Amendment (2026-08-24, after the first end-to-end run)

Two defects that no unit test could have shown, both found by driving a real
TUI against a dummy harness and restarting the server under it. Each is a place
where the "Steps" section above described an ORDER the implementation had no way
to keep.

**1. The steps are not sequential, and one pair has to be.** The TUI issues work
as `tea.Cmd`s and `tea.Batch` runs them CONCURRENTLY. A forward issued in the
same pass as the resume that brings its task back therefore races that resume
and loses: the live run printed `forward: register 127.0.0.1:34990: forward: no
such task (id unknown or task not running)`.

**DECIDED** — a task being resumed gets no forwards in that pass. The
`SessionStartedMsg` that reports the resume re-arms the apply, and the snapshot
that follows starts them with the task alive. It terminates because the task is
then not in a terminal state, so the next pass resumes nothing. Only a task the
installed workspace names re-arms, so an operator's own `session new -d` does
not trigger an apply.

**2. The reconnect wins a race the feature depends on losing.** After a server
restart the client reconnects before the RUNNER does, so the apply runs against
a server that has no runner yet: `interactive no_runner_for_repo: no idle runner
for repo ""`. The apply is one pass per subscribe, so nothing retried, and the
case this design exists for — a server restart leaves the task Failed, bring it
back — failed by default.

**DECIDED** — a failed workspace resume arms ONE re-apply for the next runner
event. Armed by a failure, never standing: re-applying on every runner event
would reopen the grid overlay under the operator. The measured gap was about ten
seconds.

**3. An apply with nothing to do now says so.** Reconciling to a no-op is the
healthy steady state and the expected answer to a `workspace apply` typed after
fixing a conflict that turned out not to need fixing; silence read as a command
that had not run.

## Amendment (2026-08-24, second round) — a save asks instead of inferring

The first implementation recorded "the task the logs pane is following, plus any
task with a forward". With four Detached sessions open it wrote ONE block, and
nothing in its report said why. The operator's question was 「1タスクしかsaveさ
れないしそれが選ばれた基準も?」 and both halves were fair: the followed task is
whatever row Enter was last pressed on, and a Detached session with no forward —
the exact thing `resume` exists for — qualified under neither clause.

Widening the rule to "every live session" was the obvious repair and is still
wrong, for a reason the operator named next: 「いくつもdetachしてても再開時に確実
にやりたいのは違うって場合もある」. **"Detached right now" and "what I want brought
back" are different sets, and only one of them is knowable from the server.** A
better guess is still a guess.

**DECIDED (2026-08-24)** — `workspace save <name>` opens a picker. It lists every
live interactive session, every task the named workspace already declares
(listed even when it is no longer running, so dropping one is a decision rather
than a consequence of it being down at that moment), and any task that only has
a forward. Per row: include/exclude, and the `resume` / `runner` values cycled in
place. `--all` keeps the non-interactive form.

`resume` and `runner` are cycled rather than typed for item 34a's reason — both
are small closed enums, so the surface should offer their states. It also removes
the last routine reason to hand-edit the file, which was the other half of the
report: 「そもそも編集しづらい」.

**DECIDED (2026-08-24)** — a save MERGES rather than replaces. Two defects made
this necessary, both demonstrated before the fix: writing one task's block
deleted every sibling block in that workspace, and every save reset `resume` /
`runner` to the defaults — the two values this spec's own text calls the ones an
operator hand-edits. `workspace.Merge` replaces the `forward` lines of a task the
save OBSERVED, keeps every other block verbatim, and never overwrites an existing
block's policy. The picker's listed set is what counts as observed, so an
unticked row drops its block while a task the picker never listed survives.

**DECIDED (2026-08-24)** — `harness-cli workspace save`'s `--task` is optional.
It was required, which is why the CLI wrote one block; `cli.PortForwardList` with
an empty filter returns every forward, so the bare form records the same set the
TUI would. `--task` narrows it, and is the only way to clear a task's forwards
from the CLI, since the registry reports presence and never absence.

## Amendment (2026-08-24, third round) — three things the file could say and the tools could not

Asked what else was only reachable by hand-editing, three answers, all defects
of the same shape: state the file can hold that no verb could produce.

**1. The grid could never be saved at all.** The save recorded it only when
`a.grid.IsOpen()`, and the grid is a full-screen overlay that intercepts every
key (`tui/app.go:1270` returns unconditionally), so the command line is
unreachable while it is up and the gate was ALWAYS false by the time a save ran.
Dead code from the operator's side, not merely a missed case.

**DECIDED** — the App records the grid selection at `openGrid`, the one entry
point, and keeps it after the overlay closes. The picker carries a grid row
cycling **keep / this session's selection / none**, so the value is visible and
both settable and removable. `none` is a real state, so the merge is told not to
carry the file's old value forward when the row was decided.

**2. Forwards were real-time only.** The picker showed a task's forwards but had
no way to add one: the list came from the registry, so "record `-L 3000` for
next time" required the forward to be running now — impossible for a task that
is not.

**DECIDED** — `f` on a picker row edits that task's `forward` lines, pre-filled
with what it currently holds. Every value goes through
`workspace.ParseForwardValue`, so the picker cannot write a spec the command line
would reject, and a rejected edit leaves the row as it was rather than
half-written.

**3. There was no way to delete a workspace.** `save` / `apply` / `ls` / `show`
could create and rewrite one but never drop it, so removing a workspace meant
opening an editor — the thing these verbs exist to avoid.

**DECIDED** — `workspace rm <name>` on both surfaces, backed by `File.Remove`,
which deletes that workspace's line span and nothing else: other workspaces and
every comment survive, the same rule `Set` follows. In the TUI, removing the
INSTALLED workspace also uninstalls it — leaving it installed would keep
re-applying something the file no longer describes, and the next reconnect would
look like the delete had not happened.

## Amendment (2026-08-29, fourth round) — the gateway is workspace state, and an install can be taken back

Two gaps, raised together by the operator, and the second is the older one.

**1. `ssh-gateway` joins the workspace-level keys.**

The ssh-gateway design listed workspace config as **Intentionally omitted**, on
the grounds that "a gateway is not per-task and has one address, and `--listen`
in the alias the operator already wrote is the same information". The first half
is true and is why this is a workspace-level key rather than a task one. The
second half does not hold: `--listen` lives in the operator's `~/.ssh/config` on
the CLIENT side and says where to CONNECT; nothing on the harness side says the
listener should exist. A workspace restores forwards on reconnect and left the
door they arrive through to be started by hand every time — the same asymmetry
`grid` was fixed for in the third round.

**DECIDED** — `ssh-gateway = <addr>` under `[workspace <name>]`, with the same
presence/emptiness split `grid` needs: an absent key means "do not touch the
gateway", `ssh-gateway =` means "bind wherever the gateway defaults to", and the
empty string is therefore an instruction rather than the absence of one. The
picker gains a row cycling **keep / running now / none**, on `s`, beside the
grid row and by its rules — including that `set` is not offered when no gateway
is running.

**DECIDED** — apply RECONCILES the gateway, as it does forwards: absent key →
nothing; declared and none running → start; declared and running elsewhere →
stop, then start; declared and already there → nothing. The stop-then-start
cannot be one batch. `Cancel()` only signals, and the listener holds its port
until the serve goroutine returns and sends `SSHGatewayStoppedMsg`, so a start
issued beside the stop would race it for the port and lose. The restart is
parked and fired by that handler — the same deferral the first amendment
introduced for a resuming task's forwards, arrived at for the same reason.

**DECIDED** — `cli/workspace` does NOT import `cli/sshgw`. That package is
`//go:build !js` and this one compiles for `js/wasm`, so the import would break
the wasm build. The config therefore holds the address as a string and validates
only that it is well formed (`net.SplitHostPort` plus a numeric port); the
default-bind resolution and the loopback / authorized-keys refusal stay in the
layers that can see `sshgw`. What a config can check is the shape; what only the
runtime can check stays at runtime.

**2. `workspace detach` — an installed workspace can be uninstalled.**

`apply` installed a workspace and nothing took it back off. Once installed it
re-applied on every reconnect, and the only exit was `workspace rm`, which
uninstalls as a SIDE EFFECT of deleting the file's block (third round) — so
"stop doing this to me" and "delete what I wrote down" were the same keystroke.

**DECIDED** — `workspace detach` clears the install and stops there: forwards,
sessions and the gateway keep running. Detach's job is to stop MANAGING. An
operator typing it after a reconnect-triggered apply should not lose the tunnels
they are working through, and tearing down by default would make the safe verb
the dangerous one.

**DECIDED** — `workspace detach --stop` also stops what the workspace started:
the forwards marked workspace-owned (that flag is the whole selector — a forward
the operator started by hand is not this workspace's to stop) and the gateway,
but only if the workspace DECLARED one.

**DECIDED** — sessions are never touched, by either form, and the result line
says so rather than leaving it to be discovered. A resume has no inverse: the
session exists, and ending it is a different and much larger action than undoing
an apply. Detach takes no workspace name for the same reason `rm` requires one —
there is only ever one installed workspace, and accepting a name would invite
`detach other` to read as "detach that one instead of mine".

### What the end-to-end run confirmed

With `.harness/config` supplying the connection and `bin/harness-tui --workspace
default` given no `--server-cid`: the task was resumed detached (`ls` showed
`Detached`, `resumed_by=tui`, `cowrite=0 viewer=0` — nothing attached) and its
forward appeared in `forward ls`. Restarting the target server and waiting
produced, in the TUI's own result pane, the failed resume, the retry after the
runner returned, and then the forward — with a new origin CID, so it was
re-established rather than a survivor. Occupying one declared port left the
other forward running and the resume unaffected, one line naming the port and
`bind: address already in use`. Freeing it and running `workspace apply` started
only the missing forward; the working one kept its forward id, so its
connections were never broken.
