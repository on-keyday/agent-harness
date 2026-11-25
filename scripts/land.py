#!/usr/bin/env python3
"""Land the current branch's HEAD on main, the whole procedure in one command.

    python3 scripts/land.py [--dry-run]

Run it from the worktree (or checkout) whose HEAD is the work to land. In order:

1. `git fetch origin`; refuse unless origin/main is an ancestor of HEAD (a
   fast-forward). Rebase onto origin/main first otherwise — never force.
2. `scripts/check_local_identifiers.py <origin/main>..HEAD`: no added line may
   carry this machine's user / host / addresses or a private LAN address (the
   repository is public). Then `scripts/wire-skew-check.sh <origin/main>`: a wire (.bgn) change must
   degrade recoverably against the revision being replaced. It exits 0 at
   once when no wire file changed. Then `make wasm-check`: every package
   under cli/ must still build for GOOS=js. Step 5's `make build` builds only
   the WebUI's wasm binary, which does not import all of them (cli/agent), so
   a !js-only symbol used there passed landing and failed CI's wasm-check.
3. `git push origin HEAD:main`.
4. Fast-forward the MAIN checkout's `main` to HEAD (the checkout every runner
   slot and `harness-cli` on PATH run from).
5. `make build` in that checkout: bin/ is what runners execute, and nothing
   else refreshes it.
6. Print which running parts the landed paths touch: the daemons need a
   restart, the WebUI a browser reload.

Each step that fails stops the run with nothing after it done, and says which
step and why. LAND_IDENT_CMD / LAND_SKEW_CMD / LAND_WASM_CMD / LAND_BUILD_CMD replace those
steps (tests).
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


def go_deps(root: Path, cmd: str, env: dict[str, str] | None = None) -> set[str] | None:
    """Import paths of every package `./cmd/<cmd>` builds, or None if unknown."""
    p = subprocess.run(["go", "list", "-deps", f"./cmd/{cmd}"], cwd=str(root),
                       capture_output=True, text=True,
                       env={**os.environ, **env} if env else None)
    return set(p.stdout.split()) if p.returncode == 0 else None


def restart_hints(paths: list[str], module: str, server_deps: set[str] | None,
                  runner_deps: set[str] | None, wasm_deps: set[str] | None = None) -> list[str]:
    """Which running parts the landed paths reach.

    Decided by which binary BUILDS a changed Go package, not by its directory
    name: runner/protocol, appwire and pubsub are in both daemons, agentboard
    only in the server. A dependency bump (go.mod / go.sum) can change either.
    Shipped skill text under runner/agentskills is read from disk on every
    task (--agentskills-dir), so it needs nothing.

    webui/ is NOT a server restart, although the server imports the package
    (its //go:embed copy): a server run with --webui-dir serves webui/ from
    disk, and `make build` has already rewritten main.wasm there, so a browser
    reload shows the change. The same holds for a package the WebUI's wasm
    builds (wasm_deps, GOOS=js). Only a server serving its embedded copy needs
    the restart, and the hint names that case rather than prescribing it --
    printing "restart the server" for every WebUI change got it relayed as a
    need, and a restart costs live tasks.
    """
    if server_deps is None or runner_deps is None:
        return ["could not compute which daemons the change reaches (go list failed); check by hand"]
    server = runner = webui = False
    for p in paths:
        if p in ("go.mod", "go.sum"):
            server = runner = True
        elif p.startswith("webui/"):
            webui = True
        elif p.endswith(".go"):
            pkg = module + "/" + p.rsplit("/", 1)[0] if "/" in p else module
            server |= pkg in server_deps
            runner |= pkg in runner_deps
            webui |= wasm_deps is not None and pkg in wasm_deps
    out = []
    if server:
        out.append("restart the server (a package it builds changed)")
    if runner:
        out.append("restart the runner slots (a package agent-runner builds changed)")
    if webui:
        out.append("reload the WebUI in the browser (a --webui-dir server serves webui/ from disk; "
                   "only a server serving its embedded copy needs a restart for it)")
    return out


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
    ident = shlex.split(os.environ.get("LAND_IDENT_CMD", "")) or [
        sys.executable, str(root / "scripts" / "check_local_identifiers.py"), f"{base}..{head}"]
    wasm = shlex.split(os.environ.get("LAND_WASM_CMD", "")) or ["make", "wasm-check"]
    build = shlex.split(os.environ.get("LAND_BUILD_CMD", "")) or ["make", "build"]
    if dry_run:
        print("land: --dry-run: would run the identifier, skew and wasm checks, push, fast-forward "
              f"{main} and build there")
        return 0
    if base != head:
        run_step("local identifier check", ident, root)
        run_step("wire skew check", skew, root)
        run_step("wasm check", wasm, root)
        run_step("push", ["git", "push", "origin", f"{head}:refs/heads/main"], root)
    else:
        print("land: origin/main is already HEAD; nothing to push")
    run_step("fast-forward main checkout", ["git", "merge", "--ff-only", "-q", head], main)
    run_step("build", build, main)
    module = next((l.split()[1] for l in (root / "go.mod").read_text().splitlines()
                   if l.startswith("module ")), "") if (root / "go.mod").is_file() else ""
    hints = restart_hints(paths, module, go_deps(root, "harness-server"), go_deps(root, "agent-runner"),
                          go_deps(root, "harness-webui-wasm", {"GOOS": "js", "GOARCH": "wasm"})) if paths else []
    print(f"land: landed {head[:12]} and built {main}/bin")
    for h in hints:
        print(f"land: {h}")
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
