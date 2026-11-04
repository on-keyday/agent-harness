"""Append agentboard messages to the parent's message log, verbatim, once each.

  log_messages.py --log FILE [--names "8hex=name ..."]
      Every message in your inbox the log lacks. For a UserPromptSubmit hook:
      prints nothing and always exits 0, because a hook's output would land in
      the parent's context.
  log_messages.py --log FILE [--names ...] SEQ...
      These messages, read with `agent read`. By hand: reports skips and
      failures on stderr, exits 1 if any seq could not be logged.

A message is in the log when a heading contains "(seq <N>,". Both modes skip
those, so the hook and a hand run never log a message twice.
`agent inbox --json` without --user-prompt-submit-hook moves no delivery mark,
so running this from a hook takes nothing away from the agent's own delivery.
"""
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parallel_common as pc  # noqa: E402


def render_body(r):
    if r.get("payload_omitted"):
        how = r.get("read_with") or "harness-cli agent read <seq>"
        return "(body not delivered: over 64 KiB. Read it with: %s)" % how
    p = r.get("payload")
    if p is not None:
        if isinstance(p, dict):
            rest = {k: v for k, v in p.items() if k != "kind"}
            if set(rest) - {"from"} == {"message"}:
                return str(rest["message"])
            return json.dumps(rest, ensure_ascii=False)
        return json.dumps(p, ensure_ascii=False)
    return r.get("payload_text") or r.get("payload_b64") or ""


def append(log, names, r, how):
    """0 = logged, 2 = already in the log, 1 = not a message record."""
    seq = r.get("seq")
    if seq is None:
        return 1
    with open(log, encoding="utf-8") as f:
        if "(seq %s," % seq in f.read():
            return 2
    sender = ((r.get("from") or {}).get("task_id") or "")[:8]
    p = r.get("payload")
    kind = p.get("kind", "-") if isinstance(p, dict) else "-"
    head = "## %s %s -> %s (seq %s, in_reply_to %s, %s)" % (
        time.strftime("%Y-%m-%d %H:%M %Z"), names.get(sender, sender),
        r.get("topic", "-"), seq, r.get("in_reply_to", 0), how)
    with open(log, "a", encoding="utf-8", newline="\n") as f:
        f.write("%s\n\nkind `%s`:\n\n````text\n%s\n````\n\n" % (head, kind, render_body(r)))
    return 0


def main(a):
    hook = not a.seq
    names = dict(kv.split("=", 1) for kv in a.names.split() if "=" in kv)
    try:
        open(a.log, "a", encoding="utf-8").close()
        lock = pc.FileLock(a.log + ".lock", timeout=10)
        lock.__enter__()
    except (OSError, TimeoutError) as e:
        if hook:
            return 0
        print("cannot lock the log: %s" % e, file=sys.stderr)
        return 1
    try:
        if hook:
            r = pc.harness("agent", "inbox", "--json")
            for rec in pc.json_lines(r.stdout):
                append(a.log, names, rec, "logged on delivery")
            return 0
        rc = 0
        for s in a.seq:
            r = pc.harness("agent", "read", s)
            recs = list(pc.json_lines(r.stdout)) if r.returncode == 0 else []
            if not recs:
                sys.stderr.write(r.stderr)
                print("read failed: %s (rotated out, expired, or a topic you do not subscribe to)" % s,
                      file=sys.stderr)
                rc = 1
                continue
            got = append(a.log, names, recs[0], "logged by hand")
            if got == 2:
                print("already logged: %s" % s, file=sys.stderr)
            elif got != 0:
                print("not a message record: %s" % s, file=sys.stderr)
                rc = 1
        return rc
    finally:
        lock.release()


if __name__ == "__main__":
    pc.utf8_stdio()
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", required=True, help="the message log file")
    ap.add_argument("--names", default="", help='"<first-8-hex>=<name> ..." to print names, not ids')
    ap.add_argument("seq", nargs="*")
    args = ap.parse_args()
    if args.seq:
        sys.exit(main(args))
    # The hook form never breaks the parent's turn: silent, exit 0, whatever happens.
    try:
        sys.stderr = open(os.devnull, "w")
        main(args)
    except BaseException:
        pass
    sys.exit(0)
