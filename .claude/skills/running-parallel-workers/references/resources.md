# Resources: checks, machines, the watcher, usage limits

Read with the "Resources" section of SKILL.md.

## Heavy checks

The conventions name two sets of checks: those a branch must pass before it
lands, and those a child runs while it works — the same set, or a cheaper one
(a subset, the fast stages only, whatever the repository's checks allow). When
the expensive checks are what the machine cannot keep up with, cut how often they
run before cutting anything else:

- **The landing checks run once, on the branch about to land** — by you, or by
  the child on its final tip, as the conventions assign — and children run the
  cheaper set while they work. This only pays where the cheaper set catches most
  of what the landing checks would: if it misses much, the landing checks fail
  more often and the rework comes at the end.
- **Several ready branches can share one run.** Combine them, run the landing
  checks once, land them together. A failure then has to be split by branch —
  rerun per branch, or bisect — so batch only while failures are rare.
- **Another machine can take the run.** If the harness has another runner with
  room, run the check there (`harness-cli exec` in a task on it, or a task spawned
  there for the purpose — `supervising-workers`). Which machines are available is
  the owner's environment: it goes in your parent-state file, not the repository.
- **CI can take it** in a pull-request repository, at the cost of CI time and a
  push.

Checks that are light, or run only in CI, need none of the below.

## The lock

Heavy checks run one at a time per machine:

```
python scripts/heavy_lock.py <absolute lock path> -- <command>
```

The same lock path in every worktree on that machine. It works on every OS and
lets the watcher count who is waiting. On Linux `flock <lock> <command>`
serialises the same way, but the watcher cannot see its queue.

## Memory and temp space

- A child's tests can be killed when several sessions' heavy runs overlap on one
  machine. Tell children to wait for available memory above a threshold, then run
  under the lock, only the checks their change needs.
- Build caches and test temp files of killed processes stay in the temp directory
  (tmpfs on many Linux hosts) and can fill a quota so builds fail. Have each child
  delete its own old scratch after checking what is in it.

## Children on several machines

The lock, the memory threshold and the watcher's memory alert are per machine —
the watcher reads the memory of the machine it runs on. Watch the other machines
by their own means, and give each child the limits of the machine it is on.

## The watcher

```
python scripts/watch.py --children <id prefixes> [--lock <path>] [--interval 60]
```

Run it in the background. It exits — waking you — on the first of: all children
idle for `--idle-min` minutes, one child idle for `--long-min`, more than
`--queue` processes waiting on the lock, memory below `--mem-mib`, a child
`failed`. A failed child keeps alerting; drop it from `--children` before
restarting. The watcher itself can be killed for memory; check memory, restart
it on the next child message, tell the owner in a line.

## Usage limits

An owner on a usage-limited plan may pause work on purpose to make the limit
last.

- Never cut the reads that back a claim — the search before saying something is
  absent, the full read before an edit, checking a child's diff. Skimming there
  costs more in rework.
- Cut repeats: the same file twice in a turn, a whole file where a section does.
- Hold back on subagents for your own checks; send readings to the other model's
  quota.
- Offer to run fewer children when asked to make the limit last.
- Do not book a deliberate pause as "waiting for the owner" in your records.
