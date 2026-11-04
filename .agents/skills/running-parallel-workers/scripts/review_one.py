"""Run one reading on a reviewer session whose worktree is on THIS machine.

  review_one.py --reviewer ID --worktree DIR [--dir RECORDS] NAME PREPARED_DIR

RECORDS defaults to $PARALLEL_DIR, else the recommended records directory
(parallel_common.py records-dir).

  1. waits until any earlier review left in DIR/review/ has its findings.md and
     the reviewer is idle, and archives it;
  2. refuses to go on unless review/ is then empty;
  3. copies PREPARED_DIR in as review/NAME and checks TASK.md arrived;
  4. sends /clear, then the request, by keystrokes;
  5. waits for findings.md and an idle reviewer, and archives it to
     RECORDS/readback/NAME.md (the whole directory to RECORDS/readback/runs/NAME).

Keystrokes, not agentboard: this is for a reviewer whose runtime has no
agentboard hook (another vendor's CLI). It does NOT check that /clear took
effect - snapshot the reviewer's screen when a run looks wrong.
PREPARED_DIR holds TASK.md - the brief, which also says what the reviewer may
read beyond this directory - and the input. references/readback-request.md is
one brief, for user-facing text; its "N. **quote:**" items are counted.
"""
import argparse
import os
import re
import shutil
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parallel_common as pc  # noqa: E402

ITEM = re.compile(r"^[0-9]+\. \*\*quote", re.M)


def idle_ms(rid):
    t = next((r for r in pc.tasks() if r.get("id") == rid), None)
    if t and str(t.get("activity") or "").startswith("idle"):
        return int(t.get("output_idle_ms") or 0)
    return -1


def send(rid, text, escapes=False):
    pc.harness("session", "send", *(["-e"] if escapes else []), rid, text)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--reviewer", required=True, help="the reviewer's task id (32 hex)")
    ap.add_argument("--worktree", required=True, help="the reviewer's worktree directory")
    ap.add_argument("--dir", help="the records directory (default: $PARALLEL_DIR, else the recommended place)")
    ap.add_argument("--timeout", type=int, default=1500, help="seconds to wait for each review")
    ap.add_argument("--idle-ms", type=int, default=20000, help="quiet time that counts as done")
    ap.add_argument("name")
    ap.add_argument("prepared")
    a = ap.parse_args()
    a.dir = pc.resolve_records_dir(a.dir)

    review = os.path.join(a.worktree, "review")
    out = os.path.join(a.dir, "readback")
    os.makedirs(review, exist_ok=True)
    os.makedirs(os.path.join(out, "runs"), exist_ok=True)
    if not os.path.isfile(os.path.join(a.prepared, "TASK.md")):
        sys.exit("%s has no TASK.md" % a.prepared)

    def stamp():
        return time.strftime("%H:%M:%S")

    def wait_done(name, start):
        findings = os.path.join(review, name, "findings.md")
        while not (os.path.isfile(findings) and os.path.getsize(findings) > 0
                   and idle_ms(a.reviewer) >= a.idle_ms):
            time.sleep(15)
            if time.monotonic() - start > a.timeout:
                print("%s %s did not finish" % (stamp(), name))
                sys.exit(1)

    def archive(name):
        """Archive review/NAME; returns a short description of findings.md."""
        dst = os.path.join(out, name + ".md")
        shutil.copyfile(os.path.join(review, name, "findings.md"), dst)
        shutil.move(os.path.join(review, name), os.path.join(out, "runs", name))
        with open(dst, encoding="utf-8") as f:
            text = f.read()
        n = len(ITEM.findall(text))
        return "%d items" % n if n else "%d lines" % len(text.splitlines())

    start = time.monotonic()
    for name in sorted(os.listdir(review)):
        if os.path.isdir(os.path.join(review, name)):
            wait_done(name, start)
            print("%s %s archived (%s)" % (stamp(), name, archive(name)))
    if os.listdir(review):
        print("%s review/ not empty" % stamp())
        return 1

    shutil.copytree(a.prepared, os.path.join(review, a.name))
    if not os.path.isfile(os.path.join(review, a.name, "TASK.md")):
        print("%s copy failed: no review/%s/TASK.md" % (stamp(), a.name))
        return 1
    send(a.reviewer, "/clear")
    time.sleep(2)
    send(a.reviewer, r"\r", escapes=True)
    time.sleep(4)
    send(a.reviewer, "Read review/%s/TASK.md and carry out the task it describes." % a.name)
    time.sleep(2)
    send(a.reviewer, r"\r", escapes=True)
    start = time.monotonic()
    print("%s %s started" % (stamp(), a.name))
    time.sleep(30)
    wait_done(a.name, start)
    print("%s %s done (%s)" % (stamp(), a.name, archive(a.name)))
    return 0


if __name__ == "__main__":
    pc.utf8_stdio()
    sys.exit(main())
