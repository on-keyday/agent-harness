"""Owner questions of the parallel work, one file per question.

Open questions are in <dir>/questions/, answered ones in <dir>/questions/answered/.
Each question has a number that is never reused (Q<n>); its options are Q<n>-1,
Q<n>-2, ... The file format is in references/questions.md.

  q.py [--dir D]                    list the open questions; those that block
                                    someone (a "blocks:" line) come first
  q.py [--dir D] all                list every question
  q.py [--dir D] <n>                show question n in full (Q12, q12 and 12 all work)
  q.py [--dir D] new                print the next free number
  q.py [--dir D] register DRAFT     give DRAFT (options written Q?-1, Q?-2, ...) the
                                    next number and the current time, file it as open
  q.py [--dir D] answer <n> TEXT    record TEXT as the answer with the current time,
                                    and file the question as answered
  q.py [--dir D] answer-recommended <n>...
                                    answer each question with its one option marked
                                    "(recommended)"; a question without exactly one
                                    such option is left open and reported

--dir defaults to $PARALLEL_DIR, else the recommended records directory
(parallel_common.py records-dir). The directory must be set up by
`parallel_common.py init`.
Times in the files are local time, YYYY-MM-DD HH:MM.
"""
import argparse
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parallel_common as pc  # noqa: E402

NAME = re.compile(r"^q(\d+)\.md$")


def now():
    return time.strftime("%Y-%m-%d %H:%M")


def numbered(qdir, adir):
    """{number: path} over open and answered questions."""
    out = {}
    for d in (qdir, adir):
        for n in os.listdir(d):
            m = NAME.match(n)
            if m:
                out[int(m.group(1))] = os.path.join(d, n)
    return out


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def write(path, text):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


def field(text, key):
    m = re.search(r"^%s: (.*)$" % re.escape(key), text, re.M)
    return m.group(1).strip() if m else ""


def number(arg):
    m = re.fullmatch(r"[Qq]?(\d+)", arg)
    if not m:
        sys.exit("not a question number: %s" % arg)
    return int(m.group(1))


def answer(qdir, adir, n, text):
    """File open question n as answered with TEXT; False when it is not open."""
    src = os.path.join(qdir, "q%d.md" % n)
    if not os.path.isfile(src):
        return False
    ans = "answer: %s %s" % (now(), text)
    t = re.sub(r"^status: open$", "status: answered", read(src), count=1, flags=re.M)
    t, done = re.subn(r"^answer:.*$", lambda m: ans, t, count=1, flags=re.M)
    if not done:
        t = t.rstrip("\n") + "\n\n" + ans + "\n"
    write(os.path.join(adir, "q%d.md" % n), t)
    os.remove(src)
    return True


def recommended(text, n):
    """The option lines of question n marked "(recommended)"."""
    pat = re.compile(r"^\s*[-*]\s*(Q%d-\d+)\b(.*\(recommended\).*)$" % n, re.M)
    return [(m.group(1), m.group(2).strip()) for m in pat.finditer(text)]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dir", help="the records directory (default: $PARALLEL_DIR, else the recommended place)")
    ap.add_argument("cmd", nargs="?", default="")
    ap.add_argument("rest", nargs="*")
    a = ap.parse_args()
    a.dir = pc.resolve_records_dir(a.dir)
    qdir = os.path.join(a.dir, "questions")
    adir = os.path.join(qdir, "answered")
    os.makedirs(adir, exist_ok=True)
    qs = numbered(qdir, adir)
    nxt = max(qs, default=0) + 1

    if a.cmd in ("", "all"):
        rows = []
        for n in sorted(qs):
            t = read(qs[n])
            st = field(t, "status")
            if a.cmd == "all" or st == "open":
                blocks = field(t, "blocks") if st == "open" else ""
                rows.append((0 if blocks else 1, n, st, field(t, "plan"), field(t, "title"), blocks))
        for _, n, st, plan, title, blocks in sorted(rows):
            line = "Q%-4s %-8s %-28s %s" % (n, st, plan, title)
            print(line + ("   [blocks: %s]" % blocks if blocks else ""))
    elif a.cmd == "new":
        print(nxt)
    elif a.cmd == "register":
        if len(a.rest) != 1:
            sys.exit("register takes one draft file")
        t = read(a.rest[0])
        if not (re.search(r"^id:", t, re.M) and re.search(r"^asked:", t, re.M)
                and re.search(r"^status: open$", t, re.M)):
            sys.exit("draft needs 'id:', 'asked:' and 'status: open' lines (references/questions.md)")
        t = re.sub(r"^id:.*$", "id: %d" % nxt, t, count=1, flags=re.M)
        t = re.sub(r"^asked:.*$", lambda m: "asked: " + now(), t, count=1, flags=re.M)
        t = t.replace("Q?-", "Q%d-" % nxt)
        path = os.path.join(qdir, "q%d.md" % nxt)
        write(path, t)
        print("Q%d %s" % (nxt, path))
    elif a.cmd == "answer":
        if len(a.rest) < 2:
            sys.exit("answer takes a number and the answer text")
        n = number(a.rest[0])
        if not answer(qdir, adir, n, " ".join(a.rest[1:])):
            sys.exit("no open question Q%d" % n)
    elif a.cmd == "answer-recommended":
        if not a.rest:
            sys.exit("answer-recommended takes one or more question numbers")
        rc = 0
        for arg in a.rest:
            n = number(arg)
            src = os.path.join(qdir, "q%d.md" % n)
            if not os.path.isfile(src):
                print("Q%d: not open" % n, file=sys.stderr)
                rc = 1
                continue
            opts = recommended(read(src), n)
            if len(opts) != 1:
                print("Q%d: %d options marked (recommended); answer it by hand" % (n, len(opts)),
                      file=sys.stderr)
                rc = 1
                continue
            opt, rest = opts[0]
            answer(qdir, adir, n, "%s %s (the recommendation, accepted as proposed)" % (opt, rest))
            print("Q%d: %s" % (n, opt))
        return rc
    else:
        n = number(a.cmd)
        if n not in qs:
            sys.exit("no question Q%d" % n)
        sys.stdout.write(read(qs[n]))
    return 0


if __name__ == "__main__":
    pc.utf8_stdio()
    sys.exit(main())
