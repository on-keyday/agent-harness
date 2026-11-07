"""Draw the codebase tour's drawn item: a directory, then a line in it, that
earlier pages have not shown the owner.

  tour_draw.py --tour-dir D [--repo R] [--commit C] [--coverage]

Shown lines. Every page (<nnn>-<date>.md) and walk (walks/*.md) in the tour
directory is read. A location (path:line, or a code-host link ending in
#L<line>) followed by a fenced code block marks the block's lines as shown: each
quoted line is matched by content to the nearest line of that file at C, so
lines that moved since the page was written still count. A quoted line may
carry a trailing annotation; it matches when it starts with the file's line. A
location with no block marks only its own line or range. "..." lines in a block
are elisions and mark nothing. A path names a tracked file when it ends in the
file's path relative to the repository.

Counted lines. A line with no letter or digit (blank, "}", "});") is neither
counted nor drawn. Files that are not text, and files matched by the owner's
exclusions, are left out. The exclusions are a fenced block with the info
string `draw-exclude` in <tour-dir>/settings.md, one pattern per line: a
directory prefix ("docs/") or an fnmatch pattern on the path ("*.pb.go"); "#"
starts a comment. The line "@generated" leaves out every file whose first 512
bytes say "Code generated" and "DO NOT EDIT" (Go's marker for generated code).
The block must exist, even if empty: the exclusions are the owner's setting,
never a default.

The draw. Directories are those that directly hold a counted file ("." for
the repository root). Step 1 picks a directory, weighted by the fraction of its
counted lines not yet shown, so a fully shown directory is never drawn and size
does not tilt the draw. Step 2 picks one unshown counted line in it, uniformly.
Both steps take their randomness from sha256 of C's full hash, so the same
commit and the same pages always give the same draw, and anyone can rerun it.

--commit defaults to HEAD; pass the commit the page describes.
--coverage also prints every directory's shown/counted lines.
"""
import argparse
import fnmatch
import glob
import hashlib
import os
import re
import subprocess
import sys

FENCE = re.compile(r"^\s*(`{3,})(.*)$")
LOCATION = re.compile(
    r"(?:/blob/[0-9a-fA-F]+/(?P<lpath>[^\s#)]+)#L(?P<lstart>\d+)(?:-L(?P<lend>\d+))?)"
    r"|(?P<path>[\w./~+@-]+\.[\w-]+):(?P<start>\d+)(?:[-–](?P<end>\d+))?"
)
ELISION = re.compile(r"^\s*(\.\.\.|…)")
SEARCH_WINDOW = 400
GENERATED = "@generated"


def git(repo, *args, data=None):
    return subprocess.run(["git", "-C", repo, *args], input=data, capture_output=True,
                          check=True).stdout


def counted(line):
    return any(c.isalnum() for c in line)


def norm(line):
    return " ".join(line.split())


def read_exclusions(tour_dir):
    path = os.path.join(tour_dir, "settings.md")
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except FileNotFoundError:
        sys.exit(f"{path} not found: the tour directory has no settings yet")
    m = re.search(r"^\s*(`{3,})draw-exclude[ \t]*\n(.*?)^\s*\1", text, re.M | re.S)
    if not m:
        sys.exit(f"{path} has no ```draw-exclude block: ask the owner what the draw "
                 "leaves out (an empty block means nothing)")
    pats = []
    for raw in m.group(2).splitlines():
        p = raw.split("#", 1)[0].strip()
        if p:
            pats.append(p)
    return pats


def excluded(path, pats):
    for p in pats:
        if p == GENERATED:
            continue
        if p.endswith("/") and path.startswith(p):
            return True
        if fnmatch.fnmatch(path, p):
            return True
    return False


def load_files(repo, commit, pats):
    """{path: [line, ...]} for every counted text file at commit."""
    names = git(repo, "ls-tree", "-r", "-z", "--name-only", commit).decode().split("\0")
    names = [n for n in names if n and not excluded(n, pats)]
    req = "".join(f"{commit}:{n}\n" for n in names).encode()
    out = git(repo, "cat-file", "--batch", data=req)
    files, pos = {}, 0
    for n in names:
        nl = out.index(b"\n", pos)
        header = out[pos:nl].split()
        size = int(header[2]) if len(header) == 3 and header[1] == b"blob" else 0
        body = out[nl + 1:nl + 1 + size]
        pos = nl + 1 + size + 1
        if b"\0" in body:
            continue
        if GENERATED in pats and b"Code generated" in body[:512] and b"DO NOT EDIT" in body[:512]:
            continue
        try:
            files[n] = body.decode("utf-8").splitlines()
        except UnicodeDecodeError:
            continue
    return files


def resolve(token, files):
    """The tracked file a path token names: the longest suffix of it at a '/'
    boundary that is a tracked path."""
    token = token.strip("`'\"()")
    parts = token.split("/")
    for i in range(len(parts)):
        cand = "/".join(parts[i:])
        if cand in files:
            return cand
    return None


def page_quotes(text, files):
    """Yield (path, start, end, quoted_lines or None) for each location."""
    pending = None  # (path, start, end) waiting for a block
    in_block, fence, block = False, "", []
    for line in text.splitlines():
        m = FENCE.match(line)
        if in_block:
            if m and len(m.group(1)) >= len(fence) and not m.group(2).strip():
                in_block = False
                if pending:
                    yield (*pending, block)
                    pending = None
            else:
                block.append(line)
            continue
        if m:
            in_block, fence, block = True, m.group(1), []
            continue
        if line.lstrip().startswith("#"):
            if pending:
                yield (*pending, None)
                pending = None
            continue
        for lm in LOCATION.finditer(line):
            path = resolve(lm.group("lpath") or lm.group("path"), files)
            if not path:
                continue
            start = int(lm.group("lstart") or lm.group("start"))
            end = int(lm.group("lend") or lm.group("end") or start)
            if pending:
                yield (*pending, None)
            pending = (path, start, max(start, end))
    if pending:
        yield (*pending, None)


def mark(shown, files, index, path, start, end, quoted):
    lines = files[path]
    if quoted is None:
        for i in range(start - 1, min(end, len(lines))):
            if counted(lines[i]):
                shown[path].add(i)
        return
    for offset, q in enumerate(quoted):
        if ELISION.match(q) or not counted(q):
            continue
        nq = norm(q)
        anchor = start - 1 + offset
        hits = index[path].get(nq)
        if not hits:
            lo, hi = max(0, anchor - SEARCH_WINDOW), min(len(lines), anchor + SEARCH_WINDOW)
            hits = [i for i in range(lo, hi)
                    if counted(lines[i]) and len(norm(lines[i])) >= 12
                    and nq.startswith(norm(lines[i]))]
        if hits:
            shown[path].add(min(hits, key=lambda i: abs(i - anchor)))


def fraction(seed, label):
    h = hashlib.sha256(f"{seed}:{label}".encode()).hexdigest()
    return int(h, 16) / float(1 << 256)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--tour-dir", required=True)
    ap.add_argument("--repo", default=".")
    ap.add_argument("--commit", default="HEAD")
    ap.add_argument("--coverage", action="store_true")
    a = ap.parse_args(argv)

    sha = git(a.repo, "rev-parse", "--verify", a.commit + "^{commit}").decode().strip()
    pats = read_exclusions(a.tour_dir)
    files = load_files(a.repo, sha, pats)
    index = {}
    for p, lines in files.items():
        idx = {}
        for i, l in enumerate(lines):
            if counted(l):
                idx.setdefault(norm(l), []).append(i)
        index[p] = idx

    pages = sorted(glob.glob(os.path.join(a.tour_dir, "[0-9][0-9][0-9]-*.md")))
    pages += sorted(glob.glob(os.path.join(a.tour_dir, "walks", "*.md")))
    shown = {p: set() for p in files}
    for page in pages:
        with open(page, encoding="utf-8") as f:
            text = f.read()
        for path, start, end, quoted in page_quotes(text, files):
            mark(shown, files, index, path, start, end, quoted)

    dirs = {}
    for p, lines in files.items():
        d = os.path.dirname(p) or "."
        total = sum(1 for l in lines if counted(l))
        if total:
            t, s = dirs.get(d, (0, 0))
            dirs[d] = (t + total, s + len(shown[p]))

    all_t = sum(t for t, _ in dirs.values())
    all_s = sum(s for _, s in dirs.values())
    print(f"commit     {sha}")
    print(f"pages      {len(pages)} read; exclusions {pats or 'none'}")
    print(f"counted    {all_t} lines in {len(dirs)} directories; "
          f"shown {all_s} ({100.0 * all_s / max(all_t, 1):.1f}%)")
    if a.coverage:
        for d in sorted(dirs):
            t, s = dirs[d]
            print(f"  {d:<50} {s:>6}/{t:<6} {100.0 * s / t:5.1f}%")

    cands = sorted(d for d, (t, s) in dirs.items() if s < t)
    if not cands:
        print("every counted line has been shown; nothing to draw")
        return 0
    weights = [(dirs[d][0] - dirs[d][1]) / dirs[d][0] for d in cands]
    r = fraction(sha, "directory") * sum(weights)
    for d, w in zip(cands, weights):
        r -= w
        if r < 0:
            break
    unshown = [(p, i) for p in sorted(files) if (os.path.dirname(p) or ".") == d
               for i, l in enumerate(files[p]) if counted(l) and i not in shown[p]]
    p, i = unshown[int(fraction(sha, "line") * len(unshown))]
    t, s = dirs[d]
    print(f"directory  {d}  (shown {s}/{t}, {100.0 * s / t:.1f}%; "
          f"drawn among {len(cands)} directories)")
    print(f"line       {p}:{i + 1}: {files[p][i].strip()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
