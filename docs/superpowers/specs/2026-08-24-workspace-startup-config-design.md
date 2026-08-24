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

Per task block, in order:

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

### Idempotency

Each apply first tears down the forwards a previous apply of the same workspace
started, then re-establishes them. `PortForwardSession` (`tui/portforward.go:98`)
gains a field marking a session as workspace-owned; it is client-local
bookkeeping for teardown and is not reported anywhere, because the server-side
registry has no notion of a workspace and inventing one would mean a wire field.
Forwards the operator started by hand are never torn down by an apply.

### Failure

- A parse error is reported on stderr and exits 2, before the TUI enters the alt
  screen. It is an authoring mistake and is fixed immediately.
- An apply failure — a local port already bound, a task no longer present, no
  runner available — appends one line per item to `a.cmdresult` and the client
  keeps running. The environment being wrong must not cost the operator the
  client.
- Each line names its subject and what happened, e.g.
  `workspace default: -L 3000:127.0.0.1:3000 on 3f2a9c… failed: bind: address
  already in use`, never a bare count.

## Writing a workspace

`workspace save <name>` writes what the client currently has:

- `server-cid`, `ws-path`, `repo` from the live connection.
- `grid` from the open grid's mode, anchor and ids, rendered back into the
  `grid` command's argument grammar.
- One task block per task that currently has workspace-eligible state: the task
  being followed or attached, and every task holding an entry in
  `a.activeForwards`.
- `forward` lines from `a.activeForwards`.
- `resume` and `runner` default to `continue` / `assigned` for a saved block, and
  are the two values an operator is expected to hand-edit afterwards.

**DECIDED (2026-08-24)** — only this client's own forwards are saved.
`forward ls` reports the server-side registry, which includes forwards other
clients established; a workspace describes what this client sets up, so saving
the registry would produce a file that duplicates another operator's forwards on
the next apply.

**DECIDED (2026-08-24)** — raw (`t`) forwards are not saved. They do not join
`activeForwards` and bind nothing locally (`tui/app.go:106-112`), so there is no
`-L`/`-R` spec that reproduces one; a raw forward's client endpoint is a TUI
pane.

**DECIDED (2026-08-24)** — saving replaces only the target workspace's own
lines. Every other line in the file, comments included, is preserved
byte-for-byte. A round-trip test pins parse → render → parse.

The CLI counterpart is `harness-cli workspace save <name> --task <id>`, which
takes the forwards from `forward ls --task <id>` and writes `resume = continue`,
`runner = assigned`. Without it, a forward established from the CLI could only
be captured by recreating it in the TUI.

## Surfaces

| surface | verdict |
| --- | --- |
| `harness-tui` flags | `--config`, `--workspace` |
| `harness-cli` global flags | `--config`, `--workspace`; they resolve `server-cid` / `ws-path` / `repo` only |
| `harness-cli workspace` | new verb family: `save`, `apply`, `ls`, `show` |
| TUI command line | new verb family `workspace`: `save`, `apply`, `ls`, `show` |
| TUI keybindings | omitted — re-applying is rare and the command line reaches it |
| TUI popups / pickers | n/a — no new modal |
| WebUI controls / command input / wasm bridge | omitted — see Scope |
| server / runner / `.bgn` / WAL | n/a — no wire or server-state change |

**DECIDED (2026-08-24)** — `harness-cli` reads only `server-cid`, `ws-path` and
`repo` from a workspace. `grid`, and the per-task `resume` / `runner` /
`forward`, are read by the TUI's apply routine and by `harness-cli workspace
apply`; other `harness-cli` subcommands ignore them. This is a difference in
which consumer reads which key, not an option accepted and dropped: no
`harness-cli` subcommand advertises a flag whose value the workspace silently
overrides.

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
- Applying twice tears down the first apply's forwards and leaves a hand-started
  forward alone.

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
