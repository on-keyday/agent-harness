"""Carry chosen keys of a container's config COPY back to the host file.

    config_sync.py --copy ROOT/.claude/settings.json --host ~/.claude/settings.json \
        --state ROOT/sync-0.json --keys model,modelSettings

Under credential injection claude's settings.json is a per-run copy, so that
nothing the container writes there (hooks, above all) reaches the host's
claude. That also discarded what the operator expects to persist: on the host,
`/model` and `/effort` save the new default at once (measured 2026-10-29,
claude 2.1.312: `/model sonnet` changed `model`, `/effort low` changed
`modelSettings`, and nothing else). The reaper runs this every 5 seconds while
the container lives and once after, so those keys reach the host within
seconds, as on the host itself.

Only a key the CONTAINER changed is written: the state file holds what the
copy held when it was made (or last synced), and a key equal to that is left
alone, so a change made on the host meanwhile is not overwritten. The host
file is rewritten atomically with its mode kept.
"""

import argparse
import json
import os
from pathlib import Path
import sys
import tempfile

MISSING = object()


def _load(path):
    try:
        return json.loads(Path(path).read_text())
    except FileNotFoundError:
        return {}


def init(copy, state, keys):
    """Record the copy's values of keys as the baseline."""
    data = _load(copy)
    Path(state).write_text(json.dumps({k: data[k] for k in keys if k in data}))


def sync(copy, host, state, keys, log=sys.stderr):
    """Write each key the copy changed since the baseline into host."""
    try:
        cur = _load(copy)
    except ValueError:
        return False  # mid-write by the agent; the next round reads it whole
    base = _load(state)
    changed = [k for k in keys if cur.get(k, MISSING) != base.get(k, MISSING)]
    if not changed:
        return False
    try:
        h = _load(host)
    except ValueError as e:
        print(f"config_sync: {host} unreadable ({e}); not syncing {changed}", file=log)
        return False
    for k in changed:
        if k in cur:
            h[k] = cur[k]
            base[k] = cur[k]
        else:
            h.pop(k, None)
            base.pop(k, None)
    hp = Path(host)
    mode = hp.stat().st_mode & 0o777 if hp.exists() else 0o600
    fd, tmp = tempfile.mkstemp(dir=hp.parent, prefix=hp.name + ".sync.")
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(h, f, indent=2)
            f.write("\n")
        os.chmod(tmp, mode)
        os.replace(tmp, hp)
    except BaseException:
        os.unlink(tmp)
        raise
    Path(state).write_text(json.dumps(base))
    print(f"config_sync: {host}: {', '.join(changed)} from the container", file=log)
    return True


def main(argv=None):
    ap = argparse.ArgumentParser(prog="config_sync.py")
    ap.add_argument("--copy", required=True)
    ap.add_argument("--host", required=True)
    ap.add_argument("--state", required=True)
    ap.add_argument("--keys", required=True)
    ap.add_argument("--init", action="store_true", help="record the baseline and stop")
    a = ap.parse_args(argv)
    keys = [k for k in a.keys.split(",") if k]
    if a.init:
        init(a.copy, a.state, keys)
    else:
        sync(a.copy, a.host, a.state, keys)
    return 0


if __name__ == "__main__":
    sys.exit(main())
