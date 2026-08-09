# Per-spawn `--caps` in the TUI, subtractive cap lists, and the removal of `caps --on-resume`

Date: 2026-08-10

## Problem

### 1. The TUI's capability mask is a mode, not an argument

`App.sessionCaps` (`tui/app.go`) is initialised to `Capability_All` and is
changed only by the `caps` command. Every spawn and resume site reads it —
thirteen of them, covering the submit popup, `submit` / `interactive` /
`session new`, the X11 and detached variants, and the `r`/`R`/`u`/`U`
task-list resumes.

So restricting one session means entering a mode, spawning, and leaving it
again. Forgetting either edge fails in both directions: leave it narrow and
the next unrelated spawn is silently restricted; leave it wide and a spawn
you believed was confined is not. The mask is displayed only when `caps` is
typed, so neither state is visible while working.

The CLI has no such problem — `harness-cli submit --caps …` names the mask
on the invocation that uses it (`cmd/harness-cli/main.go:100`,
`cmd/harness-cli/session.go:201`).

### 2. `caps --on-resume on` rewrote a live task's caps by accident

`App.applyCapsOnResume`, set by `caps --on-resume on|off`, passed
`resumeCapsOverride=true` plus the *current* `sessionCaps` on every resume,
so the server re-granted rather than keeping the task's persisted caps.

Being both sticky and invisible, it did what the mode footgun predicts: a
`cmd.exe` session on this deployment carries
`caps: exec_attach,info_global` today not by intent but because it was
resumed while the toggle happened to be on. Nothing reported the change —
there is no "caps changed on resume: all → exec_attach,info_global" line
anywhere — so it was found later by reading `harness-cli ls` for an
unrelated reason. It was left in place because the narrowing was harmless.

### 3. A mask cannot be written as "everything except X"

`ParseCaps` (`cli/caps.go`) ORs a comma-separated list of names. Expressing
"all but spawn" means enumerating the other eleven capabilities by hand and
re-editing that list whenever the enum grows.

## Design

### 1. Subtractive terms in `ParseCaps`

A term may carry a leading `-` and is then cleared from the result:

```
all,-spawn                 every capability except spawn
all,-spawn,-runner_admin   two exclusions
spawn,file_read            unchanged — existing lists parse as before
```

Parsing is two-pass: every positive term is OR'd into the base, then every
negative term is cleared. The result therefore does not depend on term
order, and `-spawn,all` means the same as `all,-spawn`. A left-to-right fold
would make those two inputs differ while looking interchangeable.

**A list of only negatives is an error.** `-spawn` alone has no visible
base — a reader cannot tell whether it starts from `all` or from `none` —
and the message names the fix (`all,-spawn`). Requiring a positive term also
means a `--caps` value never begins with `-`, so it cannot be mistaken for
the next flag.

This lands in `ParseCaps` alone, which is the single parser behind both the
CLI flag and the TUI command, so the syntax appears in both without further
work. Rendering is unaffected: `CapsLabel` expands a bitmask back to explicit
names, so `all,-spawn` is stored and displayed as the enumerated set. The
short form is an input convenience only, never a stored representation.

### 2. `--caps` on `submit`, `interactive`, `session new`

Each parser gains an optional `--caps`, carried on the action as
`Caps *protocol.Capability`.

**The pointer is load-bearing.** `Capability_None` is a real grantable value
— a shell session with no capabilities is an ordinary thing to want, and
tasks with `caps: none` exist on this deployment — so a zero mask cannot
double as "flag absent". `capsFlag` (`tui/cmdline.go`) records whether `Set`
was ever called and returns `nil` otherwise.

`App.resolveSpawnCaps(explicit, resuming)` decides both values passed to the
Do* helpers:

| `--caps` given | resuming | mask | `resumeCapsOverride` |
|---|---|---|---|
| no  | no  | `sessionCaps` | false |
| no  | yes | `sessionCaps` | false |
| yes | no  | the given mask | false |
| yes | yes | the given mask | **true** |

**An explicit `--caps` on a resume implies the override.** With the override
off the server keeps the resumed task's persisted caps, which means the mask
the operator just typed is discarded entirely. The alternatives are to honour
it or to reject the combination; treating the two flags as orthogonal is not
a third option, it is a flag whose value silently does nothing — the same
shape as `board purge <topic> --seq N`, where `flag.Parse` stopped at the
positional and the ignored `--seq` purged a whole topic.

The session default deliberately does **not** override on resume. That keeps
the property the removed toggle destroyed: an unqualified resume can neither
widen nor narrow a task. Only a mask the operator typed on that invocation
can change it. This also makes the TUI agree with the CLI, whose `--caps`
already documents "With --resume, --caps re-grants caps to the task".

The key-driven paths (submit popup, `r`/`R`/`u`/`U`) have no place to put a
flag and keep `sessionCaps` with override false.

### 3. `caps --on-resume` is removed

With per-invocation `--caps` implying re-grant, the toggle's only remaining
job was "re-grant on resume without typing it each time" — which is exactly
the sticky-invisible mode that caused the incident. No batch use case
survives: revoking a task's capabilities is `--caps none` on the resume that
does it.

`App.applyCapsOnResume` and `CapsAction.OnResume` are gone. The parser
rejects `caps --on-resume` with a message naming the replacement rather than
falling through to `ParseCaps` and reporting `--on-resume` as an unknown
capability name.

### 4. WebUI: unchanged, deliberately

`webui/static/main.js` already selects caps per spawn — `spawnCaps` is a row
of toggles rendered next to the submit control, not a mode you enter and
leave — so the per-invocation half of this change already exists there.

Its `#caps-on-resume` checkbox is **not** the same footgun and stays. In the
WebUI the mask is always explicitly present on screen, so "an explicit mask
implies re-grant" would degenerate into "every resume re-grants", removing
the safe default. The checkbox is the operator's deliberate ask, it is
visible next to the caps it applies to, and `sessionReq` already forces
`resumeCapsOverride` false when there is no `resumeTaskId`.

## Testing

- `cli`: subtractive terms — order independence (`all,-spawn` ≡ `-spawn,all`),
  subtraction from an explicit list, negatives-only rejected, unknown name
  rejected whether or not it is prefixed.
- `tui`: `--caps` absent leaves `Caps` nil on all three actions; `--caps none`
  survives as a value distinct from unset; `--caps all,-spawn` clears the bit;
  an unknown name fails the whole command rather than spawning with a default;
  `caps --on-resume` errors and the message points at `--caps`.

`make check` / `make vet` / `make test` all green.

## Relationship to the operator surface

`--caps` was already on the CLI. This makes the TUI equivalent and removes a
control that existed in neither the CLI nor the WebUI, so the three surfaces
now describe the same model: a mask belongs to a spawn, and a resume keeps
what the task has unless that spawn says otherwise.
