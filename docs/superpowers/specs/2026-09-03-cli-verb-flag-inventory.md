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

**Counted in the Go surfaces: 68 FlagSets, 293 flag registrations.** The
hand-scanned parsers add 17 more in the TUI and 25 in the WebUI.

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
