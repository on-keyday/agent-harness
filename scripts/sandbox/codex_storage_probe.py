#!/usr/bin/env python3
"""Check the codex storage behaviour the sandbox history window depends on.

Runs inside the sandbox image from check_codex_release.smoke_bundle, in a
disposable CODEX_HOME, with no credentials, no network and no model turn. It
talks to `codex app-server` over stdio JSON-RPC with synthetic rollouts in both
on-disk formats (legacy and paginated). Prints one JSON object and exits 1 if
any check is false. See
docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md §7, §10.

Checks:
  list_fresh_db        a fresh state DB lists both formats at sessions/YYYY/MM/DD/,
                       with and without a cwd filter, and the filter excludes
                       another cwd's rollout
  resume_fresh_db      thread/resume succeeds for both formats
  list_after_add       a rollout written after startup appears in the next listing
  hardlink_inode_kept  a hardlinked rollout keeps its inode through list and resume
  features_default_off local_thread_store_compression and
                       background_paginated_rollout_migration are disabled
  reindex_registers_linked_rollout
                       a rollout added while no server runs, to a DB that already
                       has a row for its cwd, is in the DB after the wrapper's
                       reindex request (history_window.reindex_params) and is then
                       listed from the DB alone. This is what the host's
                       `resume --last` needs after a window run (spec §7b).
Observation (not a check): non_date_depth, a rollout outside the date
directories, measured as listed / readable (spec §7a).
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import time

DAY = "2026/10/25"
STAMP = "2026-10-25T12:00:00Z"
FEATURES = ("local_thread_store_compression", "background_paginated_rollout_migration")


def write_rollout(sessions: Path, rel: str, uid: str, cwd: Path, paginated: bool) -> Path:
    """Three lines: session_meta, the user message, its event (the shape codex writes)."""
    meta = {"id": uid, "timestamp": STAMP, "cwd": str(cwd), "originator": "codex_cli_rs",
            "cli_version": "0.164.0", "source": "cli", "model_provider": "openai"}
    rows = [{"type": "session_meta", "payload": meta},
            {"type": "response_item", "payload": {"type": "message", "role": "user",
                                                  "content": [{"type": "input_text", "text": "storage probe"}]}},
            {"type": "event_msg", "payload": {"type": "user_message", "message": "storage probe",
                                              "images": [], "local_images": []}}]
    if paginated:
        meta["history_mode"] = "paginated"
        rows = [dict(row, ordinal=i) for i, row in enumerate(rows)]
    path = sessions / rel / f"rollout-2026-10-25T12-00-00-{uid}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps({"timestamp": STAMP, **row}) + "\n" for row in rows))
    return path


class AppServer:
    def __init__(self, codex: str, home: Path, cwd: Path, log: Path):
        env = {k: v for k, v in os.environ.items() if not k.startswith(("HARNESS_", "CODEX_"))}
        env.update(HOME=str(home), CODEX_HOME=str(home))
        self.log = open(log, "w")
        self.proc = subprocess.Popen([codex, "app-server", "--listen", "stdio://"], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=self.log, text=True, cwd=cwd, env=env, bufsize=1)
        self.next_id = 0

    def send(self, message: dict) -> None:
        self.proc.stdin.write(json.dumps(message) + "\n")
        self.proc.stdin.flush()

    def rpc(self, method: str, params: dict, timeout: float = 15) -> dict:
        self.next_id += 1
        self.send({"id": self.next_id, "method": method, "params": params})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if select.select([self.proc.stdout], [], [], 0.5)[0]:
                line = self.proc.stdout.readline()
                if not line:
                    break
                reply = json.loads(line)
                if reply.get("id") == self.next_id:
                    return reply
        return {"error": {"message": f"no reply to {method}"}}

    def ids(self, params: dict) -> set[str]:
        reply = self.rpc("thread/list", params)
        return {thread["id"] for thread in reply.get("result", {}).get("data", [])}

    def close(self) -> None:
        self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self.log.close()


def features_off(codex: str, home: Path) -> bool:
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(home))
    out = subprocess.run([codex, "features", "list"], env=env, capture_output=True, text=True, timeout=30).stdout
    rows = {line.split()[0]: line.split()[-1] for line in out.splitlines() if line.split()}
    return all(rows.get(name) == "false" for name in FEATURES)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--codex", required=True)
    args = parser.parse_args(argv)
    root = Path(tempfile.mkdtemp(prefix="codex-storage-"))
    home, work, other = root / "codex-home", root / "work", root / "other"
    for path in (home, work, other):
        path.mkdir()
    sessions = home / "sessions"
    legacy, paginated, foreign, nested, late = (f"019a15a0-0000-7000-8000-0000000000{n}" for n in (11, 12, 13, 14, 15))
    legacy_path = write_rollout(sessions, DAY, legacy, work, paginated=False)
    write_rollout(sessions, DAY, paginated, work, paginated=True)
    write_rollout(sessions, DAY, foreign, other, paginated=False)
    write_rollout(sessions, "harness/x/" + DAY, nested, work, paginated=False)
    link = root / "linked-rollout.jsonl"
    os.link(legacy_path, link)
    inode = legacy_path.stat().st_ino

    checks: dict[str, bool] = {}
    observations: dict[str, object] = {}
    server = AppServer(args.codex, home, work, root / "app-server.stderr")
    try:
        init = server.rpc("initialize", {"clientInfo": {"name": "storage_probe", "version": "0"}})
        server.send({"method": "initialized"})
        observations["initialize"] = "ok" if "result" in init else init.get("error")
        everything = server.ids({"limit": 50})
        in_work = server.ids({"limit": 50, "cwd": str(work)})
        checks["list_fresh_db"] = {legacy, paginated} <= everything and {legacy, paginated} <= in_work and foreign not in in_work
        resumed = {uid: server.rpc("thread/resume", {"threadId": uid}) for uid in (legacy, paginated)}
        checks["resume_fresh_db"] = all("result" in reply for reply in resumed.values())
        observations["resume_errors"] = {uid: reply["error"] for uid, reply in resumed.items() if "error" in reply}
        write_rollout(sessions, DAY, late, work, paginated=True)
        checks["list_after_add"] = late in server.ids({"limit": 50, "cwd": str(work)})
        checks["hardlink_inode_kept"] = legacy_path.stat().st_ino == inode and os.path.samefile(link, legacy_path)
        read = server.rpc("thread/read", {"threadId": nested, "includeTurns": True})
        observations["non_date_depth"] = {"listed": nested in everything, "readable": "result" in read}
    finally:
        server.close()

    # The host DB now has rows for `work`. A rollout that appears while no
    # server runs is what a window run leaves behind on the host.
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from history_window import reindex_params
    linked = "019a15a0-0000-7000-8000-000000000016"
    write_rollout(sessions, DAY, linked, work, paginated=True)
    db_only = {"limit": 100, "cwd": str(work), "useStateDbOnly": True, "sourceKinds": reindex_params(str(work))["sourceKinds"]}
    server = AppServer(args.codex, home, Path("/"), root / "app-server-2.stderr")
    try:
        server.rpc("initialize", {"clientInfo": {"name": "storage_probe", "version": "0"}})
        server.send({"method": "initialized"})
        observations["db_only_before_reindex"] = linked in server.ids(db_only)
        repaired = server.rpc("thread/list", reindex_params(str(work)))
        observations["reindex_reply"] = "result" if "result" in repaired else repaired.get("error")
    finally:
        server.close()
    server = AppServer(args.codex, home, Path("/"), root / "app-server-3.stderr")
    try:
        server.rpc("initialize", {"clientInfo": {"name": "storage_probe", "version": "0"}})
        server.send({"method": "initialized"})
        checks["reindex_registers_linked_rollout"] = linked in server.ids(db_only)
    finally:
        server.close()
    checks["features_default_off"] = features_off(args.codex, home)
    ok = all(checks.values())
    print(json.dumps({"storage": "ok" if ok else "fail", "checks": checks, "observations": observations}))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
