#!/usr/bin/env python3
"""Land the current branch's HEAD on main, the whole procedure in one command.

    python3 scripts/land.py [--dry-run]

Run it from the worktree (or checkout) whose HEAD is the work to land. In order:

1. `git fetch origin`; refuse unless origin/main is an ancestor of HEAD (a
   fast-forward). Rebase onto origin/main first otherwise — never force.
2. `scripts/wire-skew-check.sh <origin/main>`: a wire (.bgn) change must
   degrade recoverably against the revision being replaced. It exits 0 at
   once when no wire file changed.
3. `git push origin HEAD:main`.
4. Fast-forward the MAIN checkout's `main` to HEAD (the checkout every runner
   slot and `harness-cli` on PATH run from).
5. `make build` in that checkout: bin/ is what runners execute, and nothing
   else refreshes it.
6. Print which running parts the landed paths touch (server, runners, WebUI),
   since only those need a restart.

Each step that fails stops the run with nothing after it done, and says which
step and why. LAND_SKEW_CMD / LAND_BUILD_CMD replace steps 2 and 5 (tests).
"""
from __future__ import annotations

import argparse
import os
import shlex
import subprocess
import sys
from pathlib import Path


class LandError(Exception):
    pass


def git(cwd: Path, *args: str) -> str:
    p = subprocess.run(["git", "-C", str(cwd), *args], capture_output=True, text=True)
    if p.returncode != 0:
        raise LandError(f"git {' '.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
    return p.stdout.strip()


def main_checkout(cwd: Path) -> Path:
    """The repository's main worktree: the first entry of `git worktree list`."""
    for line in git(cwd, "worktree", "list", "--porcelain").splitlines():
        if line.startswith("worktree "):
            return Path(line[len("worktree "):])
    raise LandError("git worktree list printed no worktree")


# Path prefixes that decide what a landing needs restarted. Anything else (CLI,
# TUI, docs, scripts) takes effect with the rebuilt bin/ alone.
RESTART_HINTS = (
    (("server/",), "the server (server/ changed)"),
    (("runner/", "cmd/agent-runner/"), "the runner slots (runner/ changed)"),
    (("webui/", "cmd/harness-webui-wasm/"), "the server or a WebUI reload (webui/ changed; embedded in the server)"),
)


def restart_hints(paths: list[str]) -> list[str]:
    return [what for prefixes, what in RESTART_HINTS
            if any(p.startswith(prefixes) for p in paths)]


def run_step(name: str, argv: list[str], cwd: Path) -> None:
    print(f"land: {name}: {' '.join(shlex.quote(a) for a in argv)}", flush=True)
    if subprocess.run(argv, cwd=str(cwd)).returncode != 0:
        raise LandError(f"{name} failed (see its output above); nothing after it was done")


def land(cwd: Path, *, dry_run: bool = False) -> int:
    root = Path(git(cwd, "rev-parse", "--show-toplevel"))
    head = git(root, "rev-parse", "HEAD")
    git(root, "fetch", "-q", "origin")
    base = git(root, "rev-parse", "origin/main")
    if subprocess.run(["git", "-C", str(root), "merge-base", "--is-ancestor", base, head]).returncode != 0:
        raise LandError(f"origin/main ({base[:12]}) is not an ancestor of HEAD ({head[:12]}): "
                        "rebase onto origin/main first; this never force-pushes")
    main = main_checkout(root)
    branch = git(main, "rev-parse", "--abbrev-ref", "HEAD")
    if branch != "main":
        raise LandError(f"the main checkout {main} is on {branch!r}, not main; it cannot be fast-forwarded")
    paths = git(root, "diff", "--name-only", f"{base}..{head}").splitlines() if base != head else []
    count = git(root, "rev-list", "--count", f"{base}..{head}")
    print(f"land: {count} commit(s), {base[:12]}..{head[:12]}, {len(paths)} path(s)")

    skew = shlex.split(os.environ.get("LAND_SKEW_CMD", "")) or [str(root / "scripts" / "wire-skew-check.sh"), base]
    build = shlex.split(os.environ.get("LAND_BUILD_CMD", "")) or ["make", "build"]
    if dry_run:
        print("land: --dry-run: would run the skew check, push, fast-forward "
              f"{main} and build there")
        return 0
    if base != head:
        run_step("wire skew check", skew, root)
        run_step("push", ["git", "push", "origin", f"{head}:refs/heads/main"], root)
    else:
        print("land: origin/main is already HEAD; nothing to push")
    run_step("fast-forward main checkout", ["git", "merge", "--ff-only", "-q", head], main)
    run_step("build", build, main)
    hints = restart_hints(paths)
    print(f"land: landed {head[:12]} and built {main}/bin")
    for h in hints:
        print(f"land: restart {h}")
    if not hints and paths:
        print("land: no running part changed; nothing to restart")
    return 0


def cli(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dry-run", action="store_true", help="check and print the plan; change nothing")
    args = ap.parse_args(argv)
    try:
        return land(Path.cwd(), dry_run=args.dry_run)
    except LandError as e:
        print(f"land: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(cli(sys.argv[1:]))
