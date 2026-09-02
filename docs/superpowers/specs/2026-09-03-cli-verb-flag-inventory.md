# Flag inventory: every option, on every surface, counted

Date: 2026-09-03
Companion to `2026-09-03-cli-verb-ssot-design.md`. That spec assumed the
per-flag differences would surface one phase at a time through the differential
test. This is the count done up front instead.

## Method, and what it had to work around

Go surfaces (`cmd/harness-cli`, `cli`, `cli/agent`, `tui`) were enumerated by
AST walk, not by grep. Three things a naive sweep gets wrong:

- **Indirect registration.** `addSelectorFlags(fs)` and `addAgentArgFlags(fs)`
  (`cmd/harness-cli/main.go:114`, :123) and `gitCommonFlags(fs, submodule)`
  (`git.go:33`) register flags on a FlagSet they are handed. A `fs.X(` sweep
  reports `submit` as having eight flags when it has thirteen.
- **Shadowing.** `case "forward":` contains nested blocks that each declare
  their own `fs`. Collecting by variable name merges four verbs' flags into
  one row.
- **Alias grouping.** `follow` and `f` are one flag; `enter` and `e` are two.
  The difference is whether the registrations share a target variable, which
  is visible in the AST as the assignment `x := fs.Bool(...)` followed by
  `fs.BoolVar(x, ...)`.

Hand-scanned parsers (the TUI's `parseSetCaps`, `parseWorkspace`, `parseForward`,
`parseExecRun`, `parseServer`, `parseSetParent`, and all of `main.js`) have no
FlagSet to walk and were enumerated by reading their comparison chains.

- **Non-literal FlagSet names.** Two sites build the name —
  `flag.NewFlagSet("session stream "+verb, …)` (`session.go:238`) and
  `flag.NewFlagSet("git "+act.Sub, …)` (`tui/cmdline.go:1515`). The first pass
  of this census required a string literal and dropped both **silently**, which
  is the same shape of miss it exists to find. Caught by comparing the walk's
  count (68) against a plain `grep -c NewFlagSet` (70); they recovered
  `session stream interrupt`/`finish`'s `--flush-ms` and TUI `git
  status`/`subrepos`'s `--subrepo`.

**Counted in the Go surfaces: 70 FlagSets, 287 flag registrations.** The
hand-scanned parsers add 17 more in the TUI and 25 in the WebUI.

Two counting conventions, because both were ambiguous on the first pass:

- **`(none)` is a measured zero, not an absence of data.** `board topics`,
  `session ls`, `file ls`, `forward kill` and others genuinely register no
  flags. Appendix A lists them rather than omitting them, so a path that was
  never examined cannot hide among the ones that have nothing to declare.
- **One parser can serve two paths.** `agent subscribe` and `agent
  unsubscribe` both run `subscribeOrUnsub` (`cli/agent/subscribe.go:95-102`),
  so one FlagSet row covers two verbs. The census printed eleven rows for
  twelve `agent` sub-verbs; nothing was missed, but the row was labelled with
  only one of the two names.

## Divergences

`=` joins names that share one target (a real alias). Positional counts are
given where they differ.

| # | Verb | CLI | TUI | WebUI |
| --- | --- | --- | --- | --- |
| 1 | `submit` / `interactive` / `session new` — agent arg forwarding | `agent-arg`=`claude-arg` | **`claude-arg` only** | `--agent` only, no forwarding |
| 2 | `submit` — runner pinning | `runner` `host` `ip` | **absent** | dropdowns |
| 3 | `submit` — prompt | `--task` | positional | positional |
| 4 | `session new` | + `repo` `rows` `cols` | **absent** | buttons |
| 5 | `file pull` | `recursive`=`r` `force`=`f` `offset` `length`, 3 pos | `recursive`=`r` `force`=`f` `offset`=`o` `length`=`n`, 3 pos | **`recursive`=`r` only, 2 pos** |
| 6 | `file push` | `recursive`=`r` `force`=`f` `parents`=`p`, 3 pos | same | **no flags, 2 pos** |
| 7 | `git diff` / `show` / `file` | + `max-bytes` | **absent** | accepted, then **discarded** |
| 8 | `git <any sub-verb>` | per-sub-verb flag sets | per-sub-verb flag sets | **one shared loop: all seven flags on every sub-verb** |
| 9 | `exec ls` | `task` `json` | `task` only | `task` only |
| 10 | `forward tap` | `dir` `max-bytes` `hex` `text` `raw` `json` | `dir` `max-bytes` | none (toggles the row's panel) |
| 11 | `forward ls` | `task` `json` | none | none |
| 12 | `notify` | `--title` `--level` | **bare leading level word, positional title/text** | absent |
| 13 | `ssh-gateway` | `--listen` `--host-key` `--authorized-keys` | **`start` / `stop` + positional bind** | absent |

### The ones that are not merely missing

**#1 is a rename the TUI never received.** `cmd/harness-cli/main.go:114-118`
registers `agent-arg` as primary with `claude-arg` as "deprecated alias for
--agent-arg". The TUI registers only `claude-arg`, three times
(`tui/cmdline.go:592`, :626, :804), and the comment at :644 still describes it
as "mirroring the same idiom used by cmd/harness-cli for --claude-arg" — frozen
at the pre-rename state. An operator who learned `--agent-arg` from the CLI
gets "flag provided but not defined" in the TUI. This is the same failure the
`-d` comment at :793-795 records, for a different flag, unnoticed.

**#5 and #6 are arity differences, not flag differences.** The WebUI's `file
push` takes `<task-id> <dst>` because the local source comes from a file
picker, and `file pull` takes `<task-id> <src>` because the browser downloads
rather than writing a path. A declaration with one `Args` list per verb cannot
express that; see **Consequences**.

**#7 and #8 point opposite ways.** The TUI silently lacks `--max-bytes`, so a
large diff cannot be capped there. The WebUI accepts it and throws it away —
`main.js:6293`, "caps are the runner's job" — and accepts every git flag on
every sub-verb from one shared argument loop, so `git <id> status --rev X`
parses in the WebUI and is refused by the CLI.

**#12 and #13 are different grammars wearing one name.** `notify` and
`ssh-gateway` do not differ by a flag; they parse differently. Under D16 they
would keep one `Path` while meaning two things, which is the D17 problem in the
flag layer.

## The counter-example

`grid` is parsed by `cli.ParseGridArgs` (`cli/gridargs.go`) on both surfaces
that have it, and `tui/cmdline.go:1610-1612` states the reason this design
exists:

> The grammar itself lives in `cli.ParseGridArgs`. The workspace config accepts
> the same argument string and cannot import this package, so a copy here would
> be a mirror with no way to fail loudly when the grammar grows.

`grid` has zero divergences. So do `caps set`, `caps set-parent`, `session
await-idle`, `server dial-runner`, `prune`, `file delete` and `file mkdir` —
every one of them either small or already routed through a shared parser.

## A duplicate that already exists

`tui/cmdline.go:1394 parsePermutedFlags` is a line-for-line copy of
`cli.ParsePermuted` (`cli/permute.go:23`). Its comment gives a reason that has
expired:

> This is the same loop as parsePermuted in **cmd/harness-cli/session.go**; it
> is duplicated rather than imported because tui must not depend on cmd/.

The canonical copy now lives in package `cli`, which `tui/cmdline.go` already
imports (`cli.ParseCaps` at :509, `cli.ParseScope` at :524). The import
obstacle is gone and the comment did not follow. It is called from five sites,
all in `parseGit` (:1426, :1446, :1473, :1492, :1518) — so the TUI's `git`
family tolerates flags after positionals and no other TUI family does.

This also refines the design spec's claim that `cli.ParsePermuted` has zero
callers under `tui/`: literally true, and misleading. The TUI has its own copy
and applies it to one family out of twelve.

## Consequences for the design

1. **`Arg` needs `Surfaces` too, not just `Flag` (D19).** `file push` and `file
   pull` differ in positional count because a browser has no local path. Either
   the arity is per-surface, or those are separate `Path`s. **Decision: per-
   surface arity, via the same `Surfaces` + `SurfaceReason` pair `Flag`
   carries.** Separate paths would mean `file push` naming two different verbs,
   which is the confusion D16 and D17 exist to remove.

2. **#12 and #13 are the flag-layer form of D17.** `notify` and `ssh-gateway`
   keep one name and parse differently. **Decision: they are declared as
   distinct `Path`s — `ssh-gateway` (CLI) and `ssh-gateway start` / `stop`
   (TUI) — and `notify`'s TUI form adopts the CLI's flags in Phase 5**, since
   the bare-word level exists only because the TUI parser could not read a
   flag, which stops being true once it is declaration-driven.

3. **#8 must not be preserved.** The WebUI accepting every git flag on every
   sub-verb is not a feature to declare; it is the absence of per-sub-verb
   parsing. Phase 2 makes the WebUI reject what the CLI rejects.

4. **#7's discarded `--max-bytes` is a silent no-op flag.** Accepting an option
   and ignoring it is worse than refusing it. Phase 2 either honours it on the
   WebUI or removes it there.

5. **Phase 4's risk section understated the problem.** It named `repo` / `host`
   / `agent` resolution order. #1 through #4 show the whole selector and
   forwarding surface is missing from the TUI, so Phase 4 is a larger behaviour
   reconciliation than "compare three resolution orders".

## Appendix A — every CLI path and its flags

The CLI is the reference surface (D14, D16), so this is the list the
declaration is built from. `=` joins names sharing one target. `(none)` means
the path registers no flags — measured, not skipped.

| Path | Flags |
| --- | --- |
| `submit` | `repo` `task` `resume` `resume-conversation` `caps` `scope` `agent` `scope-for` `runner` `host` `ip` `agent-arg`=`claude-arg` |
| `ls` | `json` `tree` |
| `conns` | `json` `follow`=`f` |
| `caps` | `json` |
| `caps set` | `caps` `scope` `scope-for` `cascade` `keep-conns` |
| `caps set-parent` | `parent` `none` `swap` |
| `whoami` | `json` |
| `version` | `json` |
| `skill` | `--list`/`-l`/`ls` (if-chain, `main.go:289`) |
| `cancel` | (none) |
| `notify` | `title` `level` |
| `prune` | `before` `force`=`f` |
| `prune-local` | `repo` `before` `force`=`f` |
| `logs` | `follow`=`f` |
| `watch` | (none) |
| `notify-watch` | (none) |
| `interactive` | `repo` `resume` `resume-conversation` `caps` `scope` `agent` `scope-for` `runner` `host` `ip` `agent-arg`=`claude-arg` |
| `file push` | `recursive`=`r` `force`=`f` `parents`=`p` |
| `file pull` | `recursive`=`r` `force`=`f` `offset` `length` |
| `file ls` | (none) |
| `file mkdir` | `parents`=`p` |
| `file edit` | (none) |
| `file new` | (none) |
| `file delete` | `recursive`=`r` `force`=`f` |
| `git log` | `max` `subrepo` |
| `git diff` | `staged`=`cached` `max-bytes` `subrepo` `submodule` |
| `git show` | `max-bytes` `subrepo` `submodule` |
| `git status` | `subrepo` |
| `git file` | `staged` `rev` `max-bytes` `subrepo` |
| `git subrepos` | `subrepo` |
| `exec` (run) | `--shell` `--sshd-parent` (hand-scanned, `exec.go:93-95`) |
| `exec ls` | `task` `json` |
| `exec kill` | (none) |
| `workspace save` | `task` `resume` `runner` `repo` |
| `workspace ls` / `rm` / `show` | (none) |
| `forward` (open) | `L` `R` `W` `http-method` `http-path` `http-body` `http-header` |
| `forward ls` | `task` `json` |
| `forward tap` | `dir` `max-bytes` `hex` `text` `raw` `json` |
| `forward kill` | (none) |
| `ssh-gateway` | `listen` `host-key` `authorized-keys` |
| `session new` | `repo` `runner` `host` `ip` `resume` `resume-conversation` `caps` `scope` `scope-for` `agent` `agent-arg`=`claude-arg` `detach`=`d` `stream` `x11` `x11-display` `rows` `cols` |
| `session attach` | `view` |
| `session snapshot` | `rows` `cols` `settle-ms` `style` `color` `without-synth` `raw` `json` `ansi` `detect` `detect-agent` |
| `session send` | `enter` `e` `flush-ms` `quiet` `snapshot` `rows` `cols` `settle-ms` `resize` `style` `color` `json` `ansi` `without-synth` `detect` `detect-agent` |
| `session exec` | `timeout` `json` `exit-only` `raw` |
| `session ls` | (none) |
| `session kill` | (none) |
| `session await-idle` | `threshold-ms` `notify` `topic` |
| `session resize` | `size` `wait-ms` `quiet` |
| `session stream attach` | (none) |
| `session stream turn` | `flush-ms` |
| `session stream approve` | `allow` `deny` `message` `suggestion` `flush-ms` |
| `session stream interrupt` / `finish` | `flush-ms` (one FlagSet, dynamic name) |
| `session stream requests` / `snapshot` | unbuilt — naming one reports that, by design (`session.go:117-119`) |
| `server dial-runner` | `via` |
| `board topics` | (none) |
| `board read` | `in-reply-to` `json` |
| `board subscribers` | (none) — optional positional topic |
| `board retract` | `seq` |
| `board purge` | `seq` |
| `agent send` | `server-cid` `topic` `data` `in-reply-to` `reply-to` `no-retire-on-reply` |
| `agent wait` | `server-cid` `topic` `since` `in-reply-to` `timeout` |
| `agent inbox` | `server-cid` `since` `json` `user-prompt-submit-hook` `in-reply-to` |
| `agent subscribe` / `unsubscribe` | `server-cid` `topic` `self` (one parser, `subscribeOrUnsub`) |
| `agent dispatch` | `server-cid` `topic` `reply-to` `data` `timeout` |
| `agent topics` | `server-cid` |
| `agent subscriptions` | `server-cid` |
| `agent purge` | `server-cid` `topic` `self` `seq` |
| `agent retained` | `server-cid` `topic` `self` |
| `agent read` | `server-cid` |
| `agent retract` | `server-cid` |

`--server-cid` on all twelve `agent` paths is the global flag re-registered
per sub-verb, not a per-verb option; in the declaration it belongs to the
global set that `usage()` already documents separately.

## Appendix B — positional arity

Appendix A counts flags. Positionals are never declared, only consumed, so
they were extracted from three shapes: arity checks (`len(pos) != 3`,
`fs.NArg() > 1`), indexed reads (`pos[0]`, `fs.Arg(0)`), and reads that happen
**before** the FlagSet is built (`args[1]`).

That third shape is why the first pass reported nothing for several verbs:
`forward tap` takes its id at `args[1]` and only then constructs its FlagSet.
An extractor that starts at the declaration sees a verb with no positionals.

`(no evidence)` in the census conflates four different states, and separating
them is the point of this appendix:

| State | Meaning | Example |
| --- | --- | --- |
| variadic, unchecked | `fs.Args()` passed through whole | `prune` — 0..N task ids |
| pre-parse | positional read before the FlagSet exists | `forward tap`, `workspace save`, `forward` |
| none, extras ignored | positionals never read; surplus silently dropped | `forward ls`, `exec ls` |
| none | the verb genuinely takes none | `ls`, `conns`, `whoami`, `version`, `submit` |

### Arity per CLI path

`+text` marks free-form trailing words (the `Trailing` verbs).

| Path | Positionals | Evidence |
| --- | --- | --- |
| `submit` / `interactive` / `session new` | 0 | flags only |
| `ls` / `conns` / `caps` / `whoami` / `version` | 0 | — |
| `caps set` / `caps set-parent` | 1 task-id | `len!=1`, `[0]` |
| `notify` | 1 + text | `len==0` guard, then joined |
| `prune` / `prune-local` | 0..N task-ids | `fs.Args()` passthrough |
| `logs` | 1 task-id | `len==0`, `[0]` |
| `watch` / `notify-watch` | 0 | — |
| `file push` / `file pull` | 3 | `len!=3`, `[0]`..`[2]` |
| `file mkdir` / `file delete` | 2 | `len!=2`, `[0]`,`[1]` |
| `file ls` | 1..2 | task-id + optional dir |
| `git <sub>` | 1 task-id + 1 sub, then per-sub | `pre` reads, then `[0]`/`[1]` |
| `git log` / `show` / `file` | +0..1 rev | `len>1` refused, `len==1` |
| `git diff` | +0..2 revs | `[0]`, `[1]` |
| `git status` / `subrepos` | +0 | `len>0` refused |
| `exec` (run) | 1 task-id + argv after `--` | hand-scanned |
| `exec ls` | 0, extras ignored | positionals never read |
| `exec kill` | 1..N exec-ids | — |
| `forward` (open) | 1 task-id | `pre:args[0]` |
| `forward ls` | 0, extras ignored | positionals never read |
| `forward tap` | 1 forward-id | `pre:args[1]` |
| `forward kill` | 1..N forward-ids | — |
| `ssh-gateway` | 0 | flags only |
| `session attach` / `ls` / `kill` / `await-idle` / `snapshot` / `resize` | 1 task-id | `len!=1` / `len<1`, `[0]` |
| `session send` / `session exec` | 1 task-id + text | `NArg<2`, `Arg(0)` |
| `session stream turn` | 1 task-id + text | `NArg<2`, `Arg(0)` |
| `session stream approve` | 2 (task-id, verdict) | `len!=2`, `[0]`,`[1]` |
| `session stream attach` / `interrupt` / `finish` | 1 task-id | `len!=1`, `[0]` |
| `server dial-runner` | 1 runner-cid | `len!=1`, `[0]` |
| `workspace save` | 1 name | `pre:args[1]` |
| `workspace ls` / `rm` / `show` | 0..1 name | — |
| `board topics` | 0 | — |
| `board read` / `retract` / `purge` | 1 topic | `len==0`, `[0]` |
| `board subscribers` | 0..1 topic | optional |
| `agent read` / `agent retract` | 1 topic | `len!=1`, `[0]` |
| `agent send` / `agent dispatch` | 0..N payload words (**trailing text**) | `resolvePayload` joins `fs.Args()` |
| `agent wait` / `inbox` / `subscribe` / `unsubscribe` / `topics` / `subscriptions` / `purge` / `retained` | 0 | topic is a flag |

Rows whose Evidence column is `—` were read from the source rather than
derived by the extractor, and are the ones to re-check when their family is
migrated: `exec kill`, `forward kill`, `file ls`, `workspace ls`/`rm`/`show`,
`board topics`/`subscribers`, `watch`, `notify-watch`, and the nine flag-only
`agent` paths.

### Divergences in arity

| Path | CLI | TUI | WebUI |
| --- | --- | --- | --- |
| `file push` | 3 | 3 | **2** — the source is a file picker, not a path |
| `file pull` | 3 | 3 | **2** — the browser downloads rather than writing a path |
| `submit` | 0 (`--task`) | **1 + text** (prompt positional) | **1 + text** |
| `notify` | 1 + text | **1 level word + 1 + text** | absent |

### Two things this pass found

**`forward ls` and `exec ls` accept surplus positionals and drop them.**
Neither reads `fs.Args()` after parsing, so `forward ls garbage` runs the
unfiltered listing and says nothing about `garbage`. Not a live incident, but
it is the same shape as an ignored flag.

**The `agent` family has two trailing-text verbs nobody had listed.**
`agent send` and `agent dispatch` take their payload as joined positional
words when `--data` is absent (`cli/agent/payload.go:43-48`, called from
`send.go:46` and `dispatch.go:93`). That makes six `Trailing` verbs, not the
four on `cli/flagorder_test.go`'s allowlist.

They are missing from it because the guard cannot see them: `scanFlagSetUse`
looks for `fs.Args()` in the block that declares the FlagSet, and here the
read happens one call deeper, inside `resolvePayload(fs, …)`. The verb reads
as "defines flags, stdlib parse, no positionals" — harmless by the rule — so
it is neither reported as an offender nor required to be on the allowlist.

The same file has a second blind spot, identical to the one this census had:
`flagSetDecl` requires a literal FlagSet name (`flagorder_test.go:132-135`),
so `flag.NewFlagSet("session stream "+verb, …)` is skipped. That verb calls
`ParsePermuted` anyway, so it is unexamined rather than broken.

Both matter because D6 derives `Trailing` from that allowlist. Taken as-is it
would have declared `agent send` permutable, and permuting it breaks any
payload whose first word starts with `-`.

**`forward tap` and `workspace save` require the positional BEFORE the
flags** — the inverse of what `ParsePermuted` guarantees everywhere else.
`forward tap --dir both 5` fails with `bad forward id "--dir"` because
`args[1]` is read before the FlagSet exists. **Decision: this constraint
disappears rather than being declared.** It is an artifact of needing the id
in order to build the FlagSet, which a declaration-driven parse removes; both
verbs become ordinary permuted parses, which only widens what is accepted.
