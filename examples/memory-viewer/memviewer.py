#!/usr/bin/env python3
"""Browse and audit Claude Code's per-project auto-memory.

    python3 memviewer.py                 # write out.html (self-contained)
    python3 memviewer.py --serve         # http://127.0.0.1:8765, re-read per request
                                         #   /              the shell (~25kB)
                                         #   /api/payload   the corpus, as JSON
                                         #   /out.html      the self-contained page
    python3 memviewer.py --check         # the maintenance findings, as text

Reads ~/.claude/projects/*/memory/*.md plus each directory's MEMORY.md index.
Standard library only, by design: this runs on a box where installing a
package is a decision, not a detail.

The audit half is the point. A memory index is loaded into every session, so
it grows until something forces a prune, and the questions that decide what to
prune are not answerable by reading one file at a time: which memory does
nothing link to, which [[link]] points at a memory that no longer exists,
which index line has lost its file, which two descriptions are saying the same
thing, and how close the index is to the point where it stops being read — which
is two limits, bytes and lines, whichever comes first.
"""

from __future__ import annotations

import argparse
import errno
import subprocess
import html
import json
import os
import re
import sys
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

PROJECTS = Path.home() / ".claude" / "projects"

# The size at which the harness stops reading the index, from its own warning
# ("approaching the 24.4KB read limit"). Shown as a bar because the number on
# its own says nothing about how much room is left.
INDEX_LIMIT_BYTES = 24_400
INDEX_TARGET_BYTES = 17_100  # what the same warning asks you to compact to

# The OTHER half of the same limit, and it was missing here until 2026-09-14.
# code.claude.com/docs/en/memory: "The first 200 lines of MEMORY.md, or the first
# 25KB, whichever comes first, are loaded at the start of every conversation."
# Measuring only bytes reports headroom that does not exist: compacting an index
# into many short lines SPENDS lines while freeing bytes, so the line axis can
# bite first and a byte-only reading calls that comfortable.
# No warning text states a line target the way it does for bytes, so this target
# is DERIVED at the byte target's own ratio (17_100/24_400 = 0.70), not observed.
INDEX_LIMIT_LINES = 200
INDEX_TARGET_LINES = 140

# Two descriptions this similar are probably one memory. Jaccard over words,
# which is crude and is meant to be: it produces CANDIDATES for a human to
# look at, and a cleverer score would only make the false positives harder to
# dismiss.
SIMILAR_THRESHOLD = 0.45

STOPWORDS = set(
    """a an and are as at be but by for from has have in is it its not of on or
    that the this to was were will with you your when where which what who why
    how if then than so no nor do does did done can could should would may
    might must been being had having i me my we our they them their he she""".split()
)


# --------------------------------------------------------------------------
# parsing
# --------------------------------------------------------------------------


@dataclass
class Memory:
    path: Path
    stem: str
    name: str
    description: str
    mtype: str
    body: str
    links: list[str]
    size: int
    mtime: float
    body_html: str = ""
    inbound: list[str] = field(default_factory=list)
    # Subdirectory under memory/, e.g. "old". Empty for a live memory.
    # A subdirectory is STORAGE: nothing reads a memory unless MEMORY.md
    # points at it or a link leads there, so filing one away is exactly
    # "keep the file, drop the index line" — and the checks have to stop
    # calling that a defect.
    area: str = ""


def _unquote(v: str) -> str:
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        v = v[1:-1]
        v = v.replace('\\"', '"').replace("\\'", "'")
    return v


def parse_frontmatter(text: str) -> tuple[dict, str]:
    """Split `---`-delimited frontmatter from the body.

    A hand-rolled reader rather than a YAML dependency: the shape here is
    fixed and two levels deep (top-level scalars plus one `metadata:` block),
    and every file in the corpus is written by the same producer. If that ever
    stops being true this is the thing that has to change, which is why it
    returns what it found rather than raising.
    """
    if not text.startswith("---"):
        return {}, text
    end = text.find("\n---", 3)
    if end == -1:
        return {}, text
    head = text[3:end]
    body = text[end + 4 :].lstrip("\n")

    meta: dict = {}
    section = None
    for line in head.split("\n"):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indented = line[:1] in (" ", "\t")
        if ":" not in line:
            continue
        key, _, val = line.partition(":")
        key = key.strip()
        val = val.strip()
        if indented and section is not None:
            meta.setdefault(section, {})[key] = _unquote(val)
        elif val == "":
            section = key
            meta.setdefault(key, {})
        else:
            section = None
            meta[key] = _unquote(val)
    return meta, body


LINK_RE = re.compile(r"\[\[([^\]]+)\]\]")


def load_memory(path: Path, area: str = "") -> Memory:
    text = path.read_text(encoding="utf-8", errors="replace")
    meta, body = parse_frontmatter(text)
    metadata = meta.get("metadata") or {}
    if not isinstance(metadata, dict):
        metadata = {}
    st = path.stat()
    return Memory(
        path=path,
        area=area,
        stem=path.stem,
        name=str(meta.get("name") or path.stem),
        description=str(meta.get("description") or ""),
        mtype=str(metadata.get("type") or "?"),
        body=body,
        links=LINK_RE.findall(body),
        size=st.st_size,
        mtime=st.st_mtime,
    )


INDEX_LINE_RE = re.compile(r"^\s*-\s*\[(?P<title>[^\]]*)\]\((?P<file>[^)]+)\)\s*(?:[—-]\s*(?P<hook>.*))?$")


def parse_index(path: Path) -> list[dict]:
    if not path.exists():
        return []
    out = []
    for n, line in enumerate(path.read_text(encoding="utf-8", errors="replace").split("\n"), 1):
        m = INDEX_LINE_RE.match(line)
        if m:
            # raw is kept because the index is budgeted by BYTE, and the
            # byte cost of a line is the line, not its parts.
            out.append({"line": n, "title": m["title"], "file": m["file"],
                        "hook": (m["hook"] or "").strip(), "raw": line})
    return out


# --------------------------------------------------------------------------
# markdown -> html
#
# Only what the corpus actually uses. A general renderer would be a
# dependency-shaped amount of code for output nobody would read differently.
# --------------------------------------------------------------------------

_CODE_SLOT = "\x00CODE%d\x00"


def _inline(text: str) -> str:
    spans: list[str] = []

    def stash(m: re.Match) -> str:
        spans.append(m.group(1))
        return _CODE_SLOT % (len(spans) - 1)

    text = re.sub(r"`([^`]+)`", stash, text)
    text = html.escape(text, quote=False)
    text = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", text)
    # [[wiki]] before the ordinary link form, or the brackets fight.
    text = LINK_RE.sub(
        lambda m: f'<a class="wl" href="#" data-goto="{html.escape(m.group(1), quote=True)}">[[{html.escape(m.group(1))}]]</a>',
        text,
    )
    text = re.sub(
        r"\[([^\]]+)\]\(([^)]+)\)",
        lambda m: f'<a href="{html.escape(m.group(2), quote=True)}">{m.group(1)}</a>',
        text,
    )
    for i, s in enumerate(spans):
        text = text.replace(_CODE_SLOT % i, f"<code>{html.escape(s)}</code>")
    return text


def render_markdown(md: str) -> str:
    lines = md.split("\n")
    out: list[str] = []
    i = 0
    list_open: str | None = None

    def close_list() -> None:
        nonlocal list_open
        if list_open:
            out.append(f"</{list_open}>")
            list_open = None

    while i < len(lines):
        line = lines[i]

        if line.startswith("```"):
            close_list()
            lang = line[3:].strip()
            i += 1
            buf: list[str] = []
            while i < len(lines) and not lines[i].startswith("```"):
                buf.append(lines[i])
                i += 1
            i += 1
            cls = f' class="lang-{html.escape(lang, quote=True)}"' if lang else ""
            out.append(f"<pre{cls}><code>{html.escape(chr(10).join(buf))}</code></pre>")
            continue

        # a table: a header row followed by a |---|---| separator
        if line.lstrip().startswith("|") and i + 1 < len(lines) and re.match(r"^\s*\|[\s:|-]+\|\s*$", lines[i + 1]):
            close_list()
            def cells(row: str) -> list[str]:
                return [c.strip() for c in row.strip().strip("|").split("|")]
            head = cells(line)
            i += 2
            rows = []
            while i < len(lines) and lines[i].lstrip().startswith("|"):
                rows.append(cells(lines[i]))
                i += 1
            out.append("<table><thead><tr>" + "".join(f"<th>{_inline(c)}</th>" for c in head) + "</tr></thead><tbody>")
            for r in rows:
                out.append("<tr>" + "".join(f"<td>{_inline(c)}</td>" for c in r) + "</tr>")
            out.append("</tbody></table>")
            continue

        if re.match(r"^#{1,6}\s", line):
            close_list()
            level = len(line) - len(line.lstrip("#"))
            out.append(f"<h{min(level+2,6)}>{_inline(line[level:].strip())}</h{min(level+2,6)}>")
            i += 1
            continue

        if re.match(r"^\s*(-{3,}|\*{3,})\s*$", line):
            close_list()
            out.append("<hr>")
            i += 1
            continue

        if line.startswith(">"):
            close_list()
            buf = []
            while i < len(lines) and lines[i].startswith(">"):
                buf.append(lines[i].lstrip("> ").rstrip())
                i += 1
            out.append(f"<blockquote>{_inline(' '.join(buf))}</blockquote>")
            continue

        m = re.match(r"^(\s*)([-*]|\d+\.)\s+(.*)$", line)
        if m:
            want = "ol" if m.group(2)[0].isdigit() else "ul"
            if list_open != want:
                close_list()
                out.append(f"<{want}>")
                list_open = want
            # A hard-wrapped bullet is ONE item. Taking only the first line
            # dropped every continuation into a following <p>, which read as a
            # bullet that stops mid-sentence beside an unattached paragraph.
            parts = [m.group(3)]
            i += 1
            while (
                i < len(lines)
                and lines[i].strip()
                and not re.match(
                    r"^(#{1,6}\s|```|>|\s*([-*]|\d+\.)\s|\s*(-{3,}|\*{3,})\s*$)", lines[i]
                )
                and not lines[i].lstrip().startswith("|")
            ):
                parts.append(lines[i].strip())
                i += 1
            out.append(f"<li>{_inline(' '.join(parts))}</li>")
            continue

        if not line.strip():
            close_list()
            i += 1
            continue

        close_list()
        # Consume the line that got us here FIRST, then take continuations.
        # Testing the guard before consuming anything let a `|` line with no
        # separator row under it match no branch and advance nothing: the
        # renderer hung on the first table-looking paragraph in the corpus.
        buf = [lines[i]]
        i += 1
        while (
            i < len(lines)
            and lines[i].strip()
            and not re.match(r"^(#{1,6}\s|```|>|\s*[-*]\s|\s*\d+\.\s)", lines[i])
            and not lines[i].lstrip().startswith("|")
        ):
            buf.append(lines[i])
            i += 1
        out.append(f"<p>{_inline(' '.join(buf))}</p>")

    close_list()
    return "\n".join(out)


# --------------------------------------------------------------------------
# scanning + analysis
# --------------------------------------------------------------------------


def project_label(d: Path) -> str:
    """A readable name for a `-home-<user>-workspace-<project>` directory.

    The encoding replaces path separators with `-`, and the directory names
    contain `-` too, so it cannot be decoded back into a path — a first attempt
    rendered `agent-harness` as `<workspace>/agent/harness`. Everything lives
    under one workspace root, so the tail after it is both unambiguous and what
    a human calls the project.
    """
    s = d.name.lstrip("-")
    marker = "workspace-"
    i = s.find(marker)
    return s[i + len(marker) :] if i != -1 else s


def scan() -> list[dict]:
    projects = []
    if not PROJECTS.is_dir():
        return projects
    for pdir in sorted(PROJECTS.iterdir()):
        mdir = pdir / "memory"
        if not mdir.is_dir():
            continue
        files = sorted(p for p in mdir.glob("*.md") if p.name != "MEMORY.md")
        # One level down is an archive: memory/old/, or whatever it is called.
        # Read it, because a live memory can still link into it and a dangling
        # link is the wrong answer for a file that is right there.
        # A subdirectory is one of two things, told apart by whether it holds
        # its own INDEX.md:
        #   with one    — a TOPIC: a second-level index, pointed at from
        #                 MEMORY.md by a line naming the directory. Correct
        #                 when the file has a row in that INDEX.md.
        #   without one — an ARCHIVE (old/): correct when something links to
        #                 it, since nothing else will lead there.
        sub, sub_index, sub_raw = [], {}, {}
        for d in sorted(x for x in mdir.iterdir() if x.is_dir()):
            idx = d / "INDEX.md"
            if idx.exists():
                sub_index[d.name] = parse_index(idx)
                sub_raw[d.name] = idx.read_text(encoding="utf-8", errors="replace")
            for f in sorted(d.glob("*.md")):
                if f.name not in ("MEMORY.md", "INDEX.md"):
                    sub.append((d.name, f))
        if not files and not sub:
            continue
        mems = [load_memory(p) for p in files] + [load_memory(p, area) for area, p in sub]
        index_path = mdir / "MEMORY.md"
        index_raw = (index_path.read_text(encoding="utf-8", errors="replace")
                     if index_path.exists() else "")
        projects.append(
            {
                "key": pdir.name,
                "label": project_label(pdir),
                "short": pdir.name.rsplit("-", 1)[-1] or pdir.name,
                "dir": str(mdir),
                "memories": mems,
                "index": parse_index(index_path),
                "sub_index": sub_index,
                "sub_index_raw": sub_raw,
                "index_bytes": index_path.stat().st_size if index_path.exists() else 0,
                # The other half of the load limit (INDEX_LIMIT_LINES). splitlines()
                # so a trailing newline does not read as one more line than the file
                # presents — the same count wc -l gives.
                "index_lines": len(index_raw.splitlines()),
                "index_raw": index_raw,
            }
        )
    projects.sort(key=lambda p: -len(p["memories"]))
    return projects


def words(s: str) -> set[str]:
    return {w for w in re.findall(r"[a-z0-9_]+", s.lower()) if w not in STOPWORDS and len(w) > 2}


def analyze(proj: dict) -> dict:
    mems: list[Memory] = proj["memories"]
    by_name = {m.name: m for m in mems}
    by_stem = {m.stem: m for m in mems}

    def resolve(target: str) -> Memory | None:
        # `[[x.md]]` is written occasionally; the file is the same one.
        t = target[:-3] if target.endswith(".md") else target
        return by_name.get(t) or by_stem.get(t)

    # Two naming conventions live side by side (feedback_jargon_masks_confusion
    # and jargon-masks-confusion), and every link that crosses them dangles.
    # Resolving through the normalization would HIDE that, so the check keeps
    # failing and offers the match as a suggestion instead: a list of broken
    # links is a chore, a list with the intended target beside each is a patch.
    def norm(x: str) -> str:
        return re.sub(r"[-_]+", "_", x.lower().removesuffix(".md"))

    # Suggest the STEM, not the name: some memories carry a whole sentence in
    # `name:`, and a suggestion you cannot paste into [[...]] is not one. Both
    # forms resolve, so the stem is always a working link.
    by_norm: dict[str, str] = {}
    for m in mems:
        by_norm.setdefault(norm(m.name), m.stem)
        by_norm.setdefault(norm(m.stem), m.stem)

    # The other half of the same drift: the link also drops the type prefix, so
    # [[jargon-masks-confusion]] is written for feedback_jargon_masks_confusion
    # and normalizing alone still misses. Match on the tail after a prefix, but
    # only when exactly ONE memory ends that way — a suggestion that might be
    # the wrong file is worse than none, because it will be pasted unchecked.
    def suggest(target: str) -> str:
        nt = norm(target)
        if nt in by_norm:
            return by_norm[nt]
        hits = {stem for k, stem in by_norm.items() if k.endswith("_" + nt)}
        return hits.pop() if len(hits) == 1 else ""

    # inbound links
    for m in mems:
        for t in m.links:
            tgt = resolve(t)
            if tgt is not None and m.name not in tgt.inbound:
                tgt.inbound.append(m.name)

    dangling = []
    for m in mems:
        for t in sorted(set(m.links)):
            if resolve(t) is None:
                dangling.append({"from": m.name, "target": t, "suggest": suggest(t)})

    # Orphan = nothing links here. Archived files are judged by `unreachable`
    # instead, which also accounts for the index line.
    orphans = [m.name for m in mems if not m.inbound and not m.area]

    # Compare on the path AS THE INDEX WOULD WRITE IT: a file under memory/old/
    # is linked "old/x.md", so matching on the bare name would call an indexed
    # archive file both indexed and missing at once.
    def rel(m: "Memory") -> str:
        return f"{m.area}/{m.path.name}" if m.area else m.path.name

    indexed_files = {row["file"] for row in proj["index"]}
    live = [m for m in mems if not m.area]
    archived = [m for m in mems if m.area]


    # A file under a TOPIC directory is indexed by that directory's own
    # INDEX.md, not by MEMORY.md — that is the point of the second level.
    sub_index = proj.get("sub_index") or {}
    topical = {a: {r["file"] for r in rows} for a, rows in sub_index.items()}

    def indexed(m: "Memory") -> bool:
        if m.area in topical:
            # Either spelling: the row may name the file bare (it sits beside
            # the INDEX) or prefixed, and neither is worth calling drift.
            return m.path.name in topical[m.area] or rel(m) in topical[m.area]
        return rel(m) in indexed_files

    # Only a LIVE file missing its line is drift. An archived one has no line
    # on purpose — that is the whole mechanism.
    missing_line = sorted(rel(m) for m in live if not indexed(m))
    missing_line += sorted(rel(m) for m in mems if m.area in topical and not indexed(m))
    missing_file = sorted(indexed_files - {rel(m) for m in mems})

    # What goes wrong for an ARCHIVE is different: filing something away and
    # leaving nothing that leads back to it. No index line and no inbound link
    # is not storage, it is deletion that still costs disk.
    unreachable = sorted(m.name for m in archived
                         if m.area not in topical and not m.inbound and not indexed(m))

    similar = []
    descs = [(m, words(m.description)) for m in mems if m.description]
    for a in range(len(descs)):
        ma, wa = descs[a]
        if not wa:
            continue
        for b in range(a + 1, len(descs)):
            mb, wb = descs[b]
            if not wb:
                continue
            inter = len(wa & wb)
            if not inter:
                continue
            score = inter / len(wa | wb)
            if score >= SIMILAR_THRESHOLD:
                similar.append({"a": ma.name, "b": mb.name, "score": round(score, 2)})
    similar.sort(key=lambda s: -s["score"])

    return {
        "unreachable": unreachable,
        "orphans": orphans,
        "dangling": dangling,
        "missing_line": missing_line,
        "missing_file": missing_file,
        "similar": similar[:40],
    }


def cross_project(projects: list[dict]) -> dict:
    """Every project's memories as one pseudo-project.

    The per-project view cannot answer the question this exists for: the same
    lesson gets re-learned repo after repo under a different name, and nothing
    put those side by side. Measured 2026-09-16: 351 memories over 15 projects,
    175 of them type `feedback` — and pairs like `feedback_build_verification`
    (kcdn) and `feedback_verify_with_make_targets_not_adhoc` (harness) say one
    thing twice. Counting by FILENAME prefix gives 139 and undercounts by 36:
    the type lives in the frontmatter, and not every file is named for it.

    Only the fields the list and the detail pane read are carried, and the
    per-project ones are present but EMPTY. An index budget, orphans, dangling
    links and `similar` all resolve strictly within one project — a link in one
    repo never points into another — so a union value for any of them would be
    a number that means nothing. The page hides those panels here rather than
    computing something that looks defensible. They are kept as empty keys so a
    guard that someone forgets later renders nothing instead of throwing.
    """
    mems = []
    for p in projects:
        topics = p["subIndex"] or {}
        for m in p["memories"]:
            mems.append(dict(m, project=p["label"],
                             topical=bool(m["area"]) and m["area"] in topics))
    return {
        "key": "*",
        "label": "全プロジェクト",
        "cross": True,
        "dir": str(PROJECTS),
        "memories": mems,
        "index": [],
        "subIndex": {},
        "subIndexRaw": {},
        "indexRaw": "",
        "indexBytes": 0,
        "indexLines": 0,
        "checks": {"unreachable": [], "orphans": [], "dangling": [],
                   "missing_line": [], "missing_file": [], "similar": []},
    }


def build_payload() -> dict:
    projects = scan()
    out = []
    for p in projects:
        checks = analyze(p)
        # The hook is markdown like everything else — bold, code spans and the
        # occasional [[link]] — so it goes through the same inline renderer
        # rather than being escaped into literal asterisks. The byte count
        # beside it is of the RAW line, which is what the budget counts.
        # Enrich the rows IN PLACE so the per-memory lookup and the whole-file
        # view render the same hook — two renderings of one line is how they
        # end up disagreeing.
        # Enrich EVERY index row once, both levels, and let both the per-memory
        # lookup and the whole-file view read the same objects. Enriching one
        # level and not the other is how the second level came out rendering
        # its hook as `undefined`.
        p["index"] = [dict(row, hookHtml=_inline(row["hook"])) for row in p["index"]]
        p["sub_index"] = {a: [dict(r, hookHtml=_inline(r["hook"])) for r in rows]
                          for a, rows in (p.get("sub_index") or {}).items()}
        by_file = {row["file"]: row for row in p["index"]}
        # A filed memory's hook lives in its own directory's INDEX.md, so the
        # lookup has to span both levels or everything below the top looks
        # hookless.
        for a, rows in p["sub_index"].items():
            for row in rows:
                by_file.setdefault(f"{a}/{row['file'].split('/')[-1]}", row)
        for m in p["memories"]:
            m.body_html = render_markdown(m.body)
        out.append(
            {
                "key": p["key"],
                "label": p["label"],
                "dir": p["dir"],
                "indexBytes": p["index_bytes"],
                "indexLines": p["index_lines"],
                # The whole file, because the per-line view has to show the
                # lines that are NOT index rows too: a heading or a stray costs
                # the same always-loaded bytes as a memory does.
                "indexRaw": p["index_raw"],
                "subIndex": p["sub_index"],
                "subIndexRaw": p["sub_index_raw"],
                "checks": checks,
                "memories": [
                    {
                        "name": m.name,
                        "file": m.path.name,
                        "type": m.mtype,
                        "description": m.description,
                        "size": m.size,
                        "mtime": m.mtime,
                        "links": sorted(set(m.links)),
                        "inbound": sorted(m.inbound),
                        "html": m.body_html,
                        # What MEMORY.md says about this memory. It is the only
                        # part loaded into every session, so a viewer that shows
                        # the body and the frontmatter description but not this
                        # is hiding the one line that actually does the work.
                        "area": m.area,
                        "index": by_file.get(f"{m.area}/{m.path.name}" if m.area else m.path.name),
                    }
                    for m in p["memories"]
                ],
                "index": p["index"],
            }
        )
    # APPENDED, never prepended: the page opens on D.projects[0], so putting
    # this first would silently change which view the tool starts in. With one
    # project there is nothing to cross, so it is not offered.
    if len(out) > 1:
        out.append(cross_project(out))
    return {
        "send": send_targets(),
        "generated": time.time(),
        "indexLimit": INDEX_LIMIT_BYTES,
        "indexTarget": INDEX_TARGET_BYTES,
        "indexLimitLines": INDEX_LIMIT_LINES,
        "indexTargetLines": INDEX_TARGET_LINES,
        "projects": out,
    }


# --------------------------------------------------------------------------
# page
# --------------------------------------------------------------------------

CSS = """
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;background:#1e1e1e;color:#d4d4d4;font:14px/1.6 system-ui,-apple-system,"Noto Sans JP",sans-serif}
a{color:#6bb3f7}
code{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:.88em;background:#2a2a2a;padding:.1em .35em;border-radius:3px}
pre{background:#161616;border:1px solid #333;border-radius:5px;padding:.7rem;overflow-x:auto}
pre code{background:none;padding:0}
header{position:sticky;top:0;z-index:5;background:#1e1e1e;border-bottom:1px solid #333;padding:.6rem .9rem;display:flex;flex-wrap:wrap;gap:.5rem;align-items:center}
header h1{font-size:1rem;margin:0 .4rem 0 0;font-weight:600}
select,input{background:#252526;color:#d4d4d4;border:1px solid #444;border-radius:4px;padding:.3rem .5rem;font:inherit}
input[type=search]{min-width:14rem;flex:1}
.chip{background:#2a2a2a;border:1px solid #444;color:#bbb;border-radius:999px;padding:.2rem .7rem;cursor:pointer;font:inherit}
.chip.on{background:#2d5;color:#000;border-color:#2d5;font-weight:600}
main{display:grid;grid-template-columns:minmax(20rem,26rem) 1fr;gap:1px;background:#333;min-height:calc(100vh - 3.2rem)}
.pane{background:#1e1e1e;padding:.8rem .9rem;overflow-y:auto;max-height:calc(100vh - 3.2rem)}
h2.sec{font-size:.78rem;text-transform:uppercase;letter-spacing:.08em;color:#8a8a8a;margin:1.2rem 0 .4rem}
h2.sec:first-child{margin-top:0}
.row{padding:.45rem .55rem;border-radius:5px;cursor:pointer;border:1px solid transparent}
.row:hover{background:#252526}
.row.sel{background:#263140;border-color:#3a5a80}
.row .nm{font-family:ui-monospace,monospace;font-size:.84rem;color:#d4d4d4;word-break:break-all}
.row .ds{color:#9a9a9a;font-size:.8rem;margin-top:.15rem;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.meta{color:#7a7a7a;font-size:.74rem;margin-top:.2rem}
.t{display:inline-block;padding:0 .4rem;border-radius:3px;font-size:.7rem;margin-right:.35rem}
.t-feedback{background:#3d2a1a;color:#f0a060}
.t-project{background:#1a3d2a;color:#5cc98a}
.t-reference{background:#1a3d5c;color:#5aabf7}
.t-user{background:#3d1a3d;color:#c080f0}
.t-\\? {background:#333;color:#999}
.warn{border:1px solid #4a3520;background:#241c12;border-radius:6px;padding:.5rem .65rem;margin-bottom:.5rem}
.warn h3{margin:0 0 .3rem;font-size:.82rem;color:#f0a060}
.warn ul{margin:.2rem 0 0;padding-left:1.1rem}
.warn li{font-size:.8rem;color:#c8c8c8;margin:.1rem 0}
.bar{height:.55rem;background:#2a2a2a;border-radius:3px;overflow:hidden;margin:.35rem 0 .2rem}
.bar span{display:block;height:100%;background:#2d5}
.bar span.hot{background:#f0a060}
.bar span.over{background:#f48771}
.detail h2{margin:.2rem 0 .1rem;font-size:1.05rem;font-family:ui-monospace,monospace;word-break:break-all}
.detail .ds{color:#9a9a9a;margin:.3rem 0 .8rem}
.ixrow{border:1px solid #2f4030;background:#18211a;border-radius:6px;padding:.5rem .65rem;margin:.5rem 0 .8rem}
.ixrow.ixmissing{border-color:#4a3520;background:#241c12}
.ixline{font-size:.88rem;line-height:1.5;margin-top:.2rem}
.ixtitle{font-weight:600;color:#d4d4d4}
.ixnone{color:#c08a4a}
.arch{color:#8aa0c0;border:1px solid #35455a;border-radius:3px;padding:0 .3rem}
.arearow{display:flex;flex-wrap:wrap;gap:.35rem;padding:.4rem .8rem;border-bottom:1px solid #333;background:#1b1b1b}
.arearow:empty{display:none}
.ar{color:#8aa0c0;margin-right:.4rem}

/* The ego graph. Fixed intrinsic width in a scrolling box rather than a
   viewBox scaled to the pane: at 390px a 720-wide viewBox would render the
   labels at ~6px, which is a picture of text rather than text. */
.egowrap{overflow-x:auto;margin:.6rem 0;border:1px solid #2f2f2f;border-radius:6px;background:#181818}
.ego{display:block;width:820px}
.ego .n{cursor:pointer}
.ego circle{fill:#252526;stroke:#4a4a4a}
.ego .n:hover circle{stroke:#6bb3f7}
.ego .me circle{fill:#263140;stroke:#3a5a80}
.ego text{font:11px ui-monospace,Menlo,Consolas,monospace;fill:#b8b8b8}
.ego .me text{fill:#d4d4d4}
.ego .out{stroke:#46603f}
.ego .in{stroke:#3c5068}
.ego .both{stroke:#6a5630}
.ego .dang{stroke:#6a4a20;stroke-dasharray:3 2}
.ego .n.dang circle{fill:#241c12;stroke:#6a4a20;stroke-dasharray:3 2}
.ego .n.dang text{fill:#c08a4a}
.egocap{color:#7a7a7a;font-size:.74rem;padding:.25rem .5rem}

/* The map takes both panes: half a screen is not enough for 162 nodes, and on
   a phone the panes stack anyway. */
#mapview{position:relative;height:calc(100vh - 3.2rem);background:#161616;overflow:hidden}
#mapview[hidden]{display:none}
#mapsvg{width:100%;height:100%;display:block;cursor:grab;touch-action:none}
#mapsvg.drag{cursor:grabbing}
#mapsvg line{stroke:#3a3a3a;stroke-width:.8}
#mapsvg line.mut{stroke:#6a5630;stroke-width:1.4}
#mapsvg line.hout{stroke:#46603f;stroke-width:1.8}
#mapsvg line.hin{stroke:#3c5068;stroke-width:1.8}
#mapsvg g.dim circle{opacity:.25}
#mapsvg circle{stroke:#161616;stroke-width:1}
#mapsvg text{font:9px ui-monospace,Menlo,Consolas,monospace;fill:#9a9a9a;pointer-events:none}
.maphud{position:absolute;right:.6rem;bottom:.6rem;background:#202020cc;border:1px solid #333;
  border-radius:5px;padding:.3rem .55rem;font-size:.75rem;color:#9a9a9a}
.mapbar{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap;padding:.4rem .8rem;
  border-bottom:1px solid #333;background:#1b1b1b}
.mapbar[hidden]{display:none}
/* The area filter is a chip strip rather than a select: which areas exist, and
   which one is being looked at, are both things you should see without opening
   anything. It was a select first and the operator could not find it. Same
   shape as the list's own area chips, so there is one thing to learn. */
.mapareas{display:flex;flex-wrap:wrap;gap:.35rem;flex-basis:100%}
.mapareas:empty{display:none}

.send{margin:.8rem 0;border:1px solid #333;border-radius:6px;padding:.4rem .6rem;background:#202020}
.send summary{cursor:pointer;color:#9a9a9a;font-size:.86rem}
.sendform{display:flex;flex-direction:column;gap:.4rem;margin-top:.5rem}
.sendform select,.sendform textarea{background:#252526;color:#d4d4d4;border:1px solid #3a3a3a;
  border-radius:4px;padding:.35rem .5rem;font:inherit;font-size:.86rem;max-width:100%}
.sendform label{font-size:.82rem;color:#b0b0b0}
.warn-inline{color:#d1a054}
.warn h3 a{color:inherit;text-decoration:none;border-bottom:1px dotted #6a5a3a}
.ixsort{display:flex;gap:.4rem;align-items:center;margin:.5rem 0}
.idxtbl{border-collapse:collapse;width:100%;font-size:.84rem}
.idxtbl td{border-bottom:1px solid #2a2a2a;padding:.25rem .4rem;vertical-align:top}
.idxtbl td.n{color:#6a6a6a;text-align:right;width:3.2rem;font-family:ui-monospace,monospace}
.idxtbl td.b{color:#8aa0c0;text-align:right;width:3.5rem;font-family:ui-monospace,monospace}
.idxtbl tr.notrow td{background:#232323}
.rawidx{background:#1a1a1a;border:1px solid #333;border-radius:6px;padding:.7rem .8rem;
  overflow-x:auto;font-size:.82rem;line-height:1.5;white-space:pre;color:#c8c8c8;margin:.4rem 0}
.sendform button{background:#0e639c;color:#fff;border:0;border-radius:4px;padding:.35rem .9rem;cursor:pointer}
.sendform button:disabled{opacity:.5;cursor:default}
.detail table{border-collapse:collapse;margin:.6rem 0;font-size:.86rem;display:block;overflow-x:auto}
.detail th,.detail td{border:1px solid #3a3a3a;padding:.3rem .55rem;text-align:left;vertical-align:top}
.detail th{background:#252526;color:#bbb;font-weight:600}
.detail blockquote{border-left:3px solid #444;margin:.6rem 0;padding:.1rem .8rem;color:#b0b0b0}
.links{display:flex;flex-wrap:wrap;gap:.35rem;margin:.5rem 0}
.links a{background:#252526;border:1px solid #3a3a3a;border-radius:4px;padding:.15rem .5rem;font-size:.78rem;text-decoration:none;font-family:ui-monospace,monospace}
.empty{color:#7a7a7a;padding:2rem 0;text-align:center}
#back{position:fixed;right:.9rem;bottom:.9rem;z-index:5;background:#252526;color:#d4d4d4;
  border:1px solid #4a4a4a;border-radius:999px;padding:.5rem .9rem;font-size:.85rem;cursor:pointer;
  box-shadow:0 2px 8px #0008}
@media (max-width:800px){
  main{grid-template-columns:1fr}
  .pane{max-height:none}
  .pane.detail{border-top:1px solid #333}
  /* The cross-project view puts one chip per project here, and at 390px
     fifteen of them wrapped to eight rows and pushed the first memory off the
     screen — a filter strip that costs half the viewport before you have read
     anything. Capped and scrolled instead. Harmless for the per-project area
     strip, which is a handful of short chips and stays under the cap. */
  .arearow{max-height:6.5rem;overflow-y:auto}
}
@media (min-width:801px){ #back{display:none} }
"""

JS = r"""
// The data arrives one of two ways and nothing below cares which: baked into
// the page by `-o`, or fetched from the server, which re-reads the corpus on
// every request. Both are the same build_payload() output, so there is one
// shape to render and no second code path to keep in step.
let D = null;
let P = null, sel = null, q = "", types = new Set(), view = "all";

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
// Decimal KB on purpose: the number this is compared against comes from the
// harness's own warning ("approaching the 24.4KB read limit"), and a bar whose
// ceiling reads 23.8K next to a warning that says 24.4KB is a bar you have to
// stop and reconcile before you can use it.
const kb = (n) => (n/1000).toFixed(1) + "kB";
// The index line is budgeted in BYTES against the read limit, and these lines
// are mostly Japanese: counting characters would understate a 3-byte-per-glyph
// line by a factor of three.
const bytes = (s) => new TextEncoder().encode(s).length + "B";
// Returns the COMPLETE phrase, not a stem a caller has to finish: one call site
// appended 前 and the other did not, so the same value rendered as "today前".
const ago = (t) => {
  const d = (Date.now()/1000 - t) / 86400;
  if (d < 1) return "\u4eca\u65e5";
  return Math.floor(d) + "\u65e5\u524d";
};

// The areas are per project, so the row is built from the data rather than
// written into the page: a directory added tomorrow has to show up without an
// edit here, which is the whole point of filing things by moving them.
//
// Across projects that row means nothing — an area is a directory inside ONE
// project's memory/, and two projects' `old/` are unrelated. The axis that
// does mean something there is which project a memory came from, so the same
// strip carries that instead.
function areaChips() {
  if (P.cross) { projChips(); return; }
  const areas = [...new Set(P.memories.map(m => m.area).filter(Boolean))].sort();
  let h = "";
  if (areas.length) {
    h += `<button class="chip${area===""?" on":""}" data-area="">全部</button>`
      + `<button class="chip${area==="_top"?" on":""}" data-area="_top">上位のみ</button>`
      + areas.map(a => {
          const n = P.memories.filter(m => m.area === a).length;
          const kind = P.subIndex && P.subIndex[a] ? "第2階層" : "退避";
          return `<button class="chip${area===a?" on":""}" data-area="${esc(a)}"
                    title="${kind}">${esc(a)}/ ${n}</button>`;
        }).join("");
  }
  $("areas").innerHTML = h;
}

// Biggest first, because the question being asked is "who else wrote one of
// these" and the three projects holding 76/39/20 of the feedback memories are
// where the answer is.
function projChips() {
  const n = new Map();
  for (const m of P.memories) n.set(m.project, (n.get(m.project) || 0) + 1);
  const names = [...n.keys()].sort((a, b) => n.get(b) - n.get(a));
  $("areas").innerHTML =
    `<button class="chip${projFilter===""?" on":""}" data-proj="">全部</button>`
    + names.map(x => `<button class="chip${projFilter===x?" on":""}" data-proj="${esc(x)}"
         >${esc(x)}/ ${n.get(x)}</button>`).join("");
}

function projOptions() {
  $("proj").innerHTML = D.projects
    .map((p,i) => `<option value="${i}">${esc(p.label)} (${p.memories.length})</option>`).join("");
}

function matches(m) {
  if (P.cross) {
    if (projFilter && m.project !== projFilter) return false;
  } else if (area === "_top" ? m.area : area && m.area !== area) return false;
  if (types.size && !types.has(m.type)) return false;
  if (!q) return true;
  const hay = (m.name + " " + m.description + " " + m.html).toLowerCase();
  return q.toLowerCase().split(/\s+/).every(t => hay.includes(t));
}

function warnBlock() {
  // Nothing here survives the union. The index budget is one file's, and
  // orphans / dangling / unreachable / similar are all computed against ONE
  // project's link graph — a [[link]] never crosses a project, so a union
  // reading of any of them would be a number with no referent.
  if (P.cross) return "";
  const c = P.checks, n = c.unreachable.length + c.orphans.length + c.dangling.length + c.missing_line.length
          + c.missing_file.length + c.similar.length;
  // TWO limits truncate the index on load — bytes and LINES — and the bar shows
  // whichever is closer to its own ceiling, because that is the one that will
  // actually bite. Showing bytes alone reported headroom that did not exist:
  // compacting entries into more, shorter lines frees bytes and SPENDS lines.
  const bFrac = P.indexBytes / D.indexLimit, lFrac = P.indexLines / D.indexLimitLines;
  const byLines = lFrac > bFrac;
  const pct = Math.min(100, Math.max(bFrac, lFrac) * 100);
  const cls = (P.indexBytes >= D.indexLimit || P.indexLines >= D.indexLimitLines) ? "over"
            : (P.indexBytes >= D.indexTarget || P.indexLines >= D.indexTargetLines) ? "hot" : "";
  let h = `<div class="warn"><h3><a href="#" id="open-index">MEMORY.md ${kb(P.indexBytes)} / ${kb(D.indexLimit)} · ${P.indexLines} / ${D.indexLimitLines} 行</a></h3>
    <div class="bar"><span class="${cls}" style="width:${pct}%"></span></div>
    <div class="meta">${byLines ? "行数" : "バイト"}が先に尽きます ·
      目標 ${kb(D.indexTarget)} / ${D.indexTargetLines} 行</div></div>`;
  // The tail has to be REACHABLE, not just counted. A card that says "…他 21"
  // and offers no way to see them is a list with 21 items you cannot act on,
  // which is the same defect as a silent cut with a number painted on it.
  const item = (label, list, fmt, key) => {
    if (!list.length) return "";
    const all = warnOpen.has(key), shown = all ? list : list.slice(0, 25);
    return `<div class="warn"><h3>${label} (${list.length})</h3>
      <ul>${shown.map(fmt).join("")}</ul>${
      list.length > 25
        ? `<button class="chip" data-warnmore="${esc(key)}">${
            all ? "先頭 25 件だけ表示" : `残り ${list.length - 25} 件を表示`}</button>`
        : ""}</div>`;
  };
  h += item("孤立 — 被リンク 0", c.orphans, x => `<li><a href="#" data-goto="${esc(x)}">${esc(x)}</a></li>`, "orphans");
  h += item("dangling — リンク先が無い", c.dangling,
    x => `<li><a href="#" data-goto="${esc(x.from)}">${esc(x.from)}</a> → <code>[[${esc(x.target)}]]</code>${
      x.suggest ? ` → <a href="#" data-goto="${esc(x.suggest)}">${esc(x.suggest)}</a>?` : ""}</li>`, "dangling");
  h += item("退避したが到達不能 — 索引行もリンクも無い", c.unreachable,
    x => `<li><a href="#" data-goto="${esc(x)}">${esc(x)}</a></li>`, "unreachable");
  h += item("index に行が無い", c.missing_line, x => `<li><code>${esc(x)}</code></li>`, "missing_line");
  h += item("index の行にファイルが無い", c.missing_file, x => `<li><code>${esc(x)}</code></li>`, "missing_file");
  h += item("description が似ている", c.similar,
    x => `<li>${x.score} <a href="#" data-goto="${esc(x.a)}">${esc(x.a)}</a> ↔ <a href="#" data-goto="${esc(x.b)}">${esc(x.b)}</a></li>`, "similar");
  return n ? h : h + `<div class="meta">検出なし</div>`;
}

function sortFor(list) {
  if (view === "big") return [...list].sort((a,b) => b.size - a.size);
  if (view === "old") return [...list].sort((a,b) => a.mtime - b.mtime);
  if (view === "linked") return [...list].sort((a,b) => b.inbound.length - a.inbound.length);
  return [...list].sort((a,b) => a.name.localeCompare(b.name));
}

function renderList() {
  const list = sortFor(P.memories.filter(matches));
  $("count").textContent = `${list.length} / ${P.memories.length}`;
  $("list").innerHTML = list.length ? list.map(m => `
    <div class="row ${sel && sel.name===m.name ? "sel":""}" data-goto="${esc(m.name)}">
      <div class="nm">${esc(m.name)}</div>
      <div class="ds">${esc(m.description)}</div>
      <div class="meta"><span class="t t-${esc(m.type)}">${esc(m.type)}</span>${
        P.cross ? `<span class="ar">${esc(m.project)}/</span>` : ""}${
        m.area ? `<span class="ar">${esc(m.area)}/</span>` : ""}${kb(m.size)} ·
        ${ago(m.mtime)} · →${m.links.length} ←${m.inbound.length}</div>
    </div>`).join("") : `<div class="empty">一致なし</div>`;
}

function renderDetail() {
  if (!sel) { $("detail").innerHTML = `<div class="empty">左から選んでください</div>`; return; }
  const m = sel;
  const linkRow = (label, arr) => arr.length
    ? `<div class="meta">${label}</div><div class="links">${arr.map(x =>
        `<a href="#" data-goto="${esc(x)}">${esc(x)}</a>`).join("")}</div>` : "";
  // MEMORY.md's own line comes FIRST, because it is the only part of this
  // memory that is loaded into every session — the body is read only if
  // something makes you open the file. Showing the frontmatter description
  // where the reader expects "what this says" would show the half nobody sees.
  const ix = m.index;
  // Whether this memory's area is a TOPIC (has its own INDEX.md) or an
  // archive. Per project that is read off P.subIndex; in the cross view there
  // is no single subIndex, so the answer travels with the memory instead.
  const isTopic = P.cross ? !!m.topical : !!(P.subIndex && P.subIndex[m.area]);
  const idxName = m.area && isTopic ? esc(m.area) + "/INDEX.md" : "MEMORY.md";
  const idxBlock = ix
    ? `<div class="ixrow"><div class="meta">${idxName} ${ix.line} 行目 · ${bytes(ix.raw)}</div>
        <div class="ixline"><span class="ixtitle">${esc(ix.title)}</span>${
          ix.hook ? ` — ${ix.hookHtml}` : ` <span class="ixnone">— hook が無い</span>`}</div></div>`
    : `<div class="ixrow ixmissing"><div class="ixline">${idxName
        } に行が無い — このメモは開かれない限り存在しないのと同じ</div></div>`;
  $("detail").innerHTML = `
    <h2>${esc(m.name)}</h2>
    <div class="meta"><span class="t t-${esc(m.type)}">${esc(m.type)}</span>${
      P.cross ? `<span class="ar">${esc(m.project)}/</span>` : ""}
      <code>${m.area ? esc(m.area) + "/" : ""}${esc(m.file)}</code> · ${kb(m.size)} ·
      更新 ${ago(m.mtime)}${m.area
        ? ` · <span class="arch">${isTopic ? "第2階層" : "退避"} (${esc(m.area)}/)</span>`
        : ""}</div>
    ${idxBlock}
    <div class="meta">frontmatter description</div>
    <div class="ds">${esc(m.description)}</div>
    ${egoGraph(m)}
    ${linkRow("→ このメモが指す", m.links)}
    ${linkRow("← このメモを指す", m.inbound)}
    ${sendPanel(m)}
    <hr>${m.html}`;
  $("detail").scrollTop = 0;
}

// This memory and what it is wired to, one hop.
//
// One hop on purpose: clicking a node re-centres, so walking reaches any depth
// while the drawing stays bounded by DEGREE. Measured over this corpus
// 2026-09-16 — median 4 neighbours, worst 24 — which is small enough that a
// fixed polar placement beats an iterative layout, and this file carries no
// dependency to run one with anyway.
//
// Direction is POSITION: what this memory points at is above it, what points
// at it is below, and a mutual pair sits out on the equator. Arrowheads would
// restate what the layout already says, and a legend would be a second thing
// to read before the picture means anything.
//
// A [[link]] that resolves to nothing is DRAWN, dashed, not dropped. It is the
// same defect the 要保守 panel lists, and a picture that quietly omitted it
// would show this memory as wired up when it is not.
function egoGraph(m) {
  // Links never cross projects, so in the cross view resolve inside the
  // memory's OWN project: otherwise a harness [[link]] could land on a
  // same-named memory belonging to another repo.
  const pool = P.cross ? P.memories.filter(x => x.project === m.project) : P.memories;
  const by = new Map();
  for (const x of pool) by.set(x.name, x.name);
  for (const x of pool) {
    const stem = x.file.replace(/\.md$/, "");
    if (!by.has(stem)) by.set(stem, x.name);
  }
  const resolve = (t) => by.get(t.replace(/\.md$/, "")) || null;

  const inn = new Set(m.inbound);
  const out = new Set();
  const dangling = [];
  for (const t of m.links) {
    const r = resolve(t);
    if (r === null) { if (!dangling.includes(t)) dangling.push(t); }
    else if (r !== m.name) out.add(r);
  }
  const mutual = [...out].filter(n => inn.has(n));
  const outOnly = [...out].filter(n => !inn.has(n));
  const inOnly = [...inn].filter(n => !out.has(n));
  if (!outOnly.length && !inOnly.length && !mutual.length && !dangling.length) return "";

  const W = 820, H = 470, cx = W / 2, cy = H / 2, R = 112, STEP = 44;
  const nodes = [];
  // Rings, not one circle. Labels are the thing that collides, not the dots:
  // measured at 18 inbound on one arc they overlapped into unreadable mush at
  // two radii. One ring per 8 keeps roughly 20 degrees between neighbours at
  // any degree this corpus reaches (worst node: 24).
  const arc = (items, a0, a1) => {
    const tiers = Math.min(3, Math.max(1, Math.ceil(items.length / 8)));
    items.forEach((it, i) => {
      const t = items.length === 1 ? 0.5 : i / (items.length - 1);
      const a = (a0 + (a1 - a0) * t) * Math.PI / 180;
      const r = R + (i % tiers) * STEP;
      nodes.push({...it, x: cx + r * Math.cos(a), y: cy - r * Math.sin(a)});
    });
  };
  arc([...outOnly.map(n => ({label: n, go: n, kind: "out"})),
       ...dangling.map(t => ({label: t, go: null, kind: "dang"}))], 168, 12);
  arc(inOnly.map(n => ({label: n, go: n, kind: "in"})), -168, -12);
  // The equator, alternating sides so a third and a fourth do not stack on one
  // point. Beyond a handful they climb into the arcs, which is rare and still
  // reads as "on the side".
  mutual.forEach((n, i) => {
    const right = i % 2 === 1;
    const a = ((right ? 0 : 180) + Math.floor(i / 2) * (right ? 11 : -11)) * Math.PI / 180;
    nodes.push({label: n, go: n, kind: "both",
                x: cx + (R + 20) * Math.cos(a), y: cy - (R + 20) * Math.sin(a)});
  });

  // The type prefix is dropped from the LABEL, never from the tooltip: nearly
  // every name starts with one, so without this the visible characters are the
  // ones every node shares and the distinguishing tail is what gets cut.
  //
  // It does NOT reduce label collisions, and the comment here said it did until
  // it was measured. The cap below decides the width, so dropping ten leading
  // characters changes WHICH eighteen are shown, not how wide they are. Nor did
  // geometry: widening 720 -> 820 and spreading the rings took the worst node
  // from 7 overlapping pairs to 6. What is left is the tail of one project —
  // 0-2 pairs everywhere else, 4-8 on the three densest nodes in
  // remote-agent-harness, where every name is also listed in full in the chips
  // directly below and carried in each node's tooltip.
  const cut = (s) => {
    const t = s.replace(/^(feedback|project|reference|user)[_-]/, "");
    return t.length > 18 ? t.slice(0, 17) + "…" : t;
  };
  const side = (n) => n.x < cx - 14 ? "end" : (n.x > cx + 14 ? "start" : "middle");
  const lx = (n) => n.x + (side(n) === "end" ? -9 : side(n) === "start" ? 9 : 0);
  const ly = (n) => n.y + (side(n) === "middle" ? (n.y < cy ? -11 : 17) : 4);
  const xy = (v) => v.toFixed(1);

  const edges = nodes.map(n =>
    `<line class="${n.kind}" x1="${xy(cx)}" y1="${xy(cy)}" x2="${xy(n.x)}" y2="${xy(n.y)}"></line>`).join("");
  const dots = nodes.map(n =>
    `<g class="n ${n.kind === "dang" ? "dang" : ""}"${n.go ? ` data-goto="${esc(n.go)}"` : ""}>
       <title>${esc(n.label)}${n.go ? "" : "  (リンク先が無い)"}</title>
       <circle cx="${xy(n.x)}" cy="${xy(n.y)}" r="5"></circle>
       <text x="${xy(lx(n))}" y="${xy(ly(n))}" text-anchor="${side(n)}">${esc(cut(n.label))}</text>
     </g>`).join("");

  const cap = [
    outOnly.length ? `↑ 指す ${outOnly.length}` : "",
    inOnly.length ? `↓ 指される ${inOnly.length}` : "",
    mutual.length ? `↔ 相互 ${mutual.length}` : "",
    dangling.length ? `⚠ リンク先が無い ${dangling.length}` : "",
  ].filter(Boolean).join(" · ");

  return `<div class="egowrap">
    <svg class="ego" viewBox="0 0 ${W} ${H}" height="${H}" role="img" aria-label="link graph">
      ${edges}
      <g class="n me"><title>${esc(m.name)}</title>
        <circle cx="${xy(cx)}" cy="${xy(cy)}" r="8"></circle>
        <text x="${xy(cx)}" y="${xy(cy - 16)}" text-anchor="middle">${esc(cut(m.name))}</text>
      </g>
      ${dots}
    </svg>
    <div class="egocap">${cap}</div>
  </div>`;
}

// projectGraph: one project's memories as an undirected graph of indices.
//
// Resolution matches the ego graph's, and for the same reason: a [[target]]
// names a memory by its `name` or by its filename stem, and links never cross
// projects, so the pool is one project's memories and nothing else.
//
// Edges are undirected — 354 of the 373 edges in the largest project here are
// one-way, so arrowheads on all of them would restate what is true of nearly
// all of them. The rare reciprocal pairs are marked instead, in `mutual`,
// keyed "i:j" with i < j.
function projectGraph(memories) {
  const names = memories.map((m) => m.name);
  const index = new Map();
  memories.forEach((m, i) => index.set(m.name, i));
  const byStem = new Map();
  memories.forEach((m, i) => {
    const stem = m.file.replace(/\.md$/, "");
    if (!index.has(stem)) byStem.set(stem, i);
  });
  const resolve = (t) => {
    const k = t.replace(/\.md$/, "");
    return index.has(k) ? index.get(k) : (byStem.has(k) ? byStem.get(k) : -1);
  };
  const directed = new Set();
  // Counted here rather than from m.links, which is raw [[targets]] and so
  // includes danglers, self-links and repeats that the drawing does not show.
  const out = memories.map(() => 0);
  memories.forEach((m, i) => {
    const hit = new Set();
    for (const raw of m.links) {
      const j = resolve(raw);
      if (j >= 0 && j !== i) { directed.add(i + ":" + j); hit.add(j); }
    }
    out[i] = hit.size;
  });
  const edges = [], mutual = new Set(), seen = new Set();
  for (const key of directed) {
    const [a, b] = key.split(":").map(Number);
    const lo = Math.min(a, b), hi = Math.max(a, b), k = lo + ":" + hi;
    if (seen.has(k)) continue;
    seen.add(k);
    edges.push([lo, hi]);
    if (directed.has(lo + ":" + hi) && directed.has(hi + ":" + lo)) mutual.add(k);
  }
  return { names, index, edges, mutual, resolve, out };
}

// mapLayout: where the whole-project map's nodes sit.
//
// Force-directed, run to completion once rather than animated — measured at
// 94.4 ms for the largest project here (168 nodes, 373 edges), so there is
// nothing worth streaming. Pure: indices in, coordinates out, no DOM, no
// globals, which is what lets maplayout_test.mjs assert it.
//
// The seed is a PARAMETER and the caller passes a constant. This tool is
// revisited; a layout that moved between visits would make a change in the
// notes indistinguishable from a change in the drawing.
// `bias`, when given, is one number per node in [0,1]: 1 is pulled to the top
// of the drawing, 0 to the bottom. Passing nothing leaves the layout exactly as
// it was — a picture already looked at must not move because an option exists.
// bandsFor: one horizontal strip per distinct weight, and strips never touch.
//
// This is the strict alternative to the soft bias, and it exists because the
// soft one was reported as "uncomfortable" — it got the ordering MOSTLY right,
// which is the worst of the three possible states. A drawing that is ordered
// nine times out of ten is read as ordered and then misleads on the tenth. Here
// y is a function of weight alone, so no two different weights can invert. Not
// by tuning; by construction.
//
// Strips rather than lines: the weights are not evenly populated — 83 of this
// corpus's 169 memories have at most one inbound link — and a line would make
// those rows a hundred nodes wide. A strip gives the crowd somewhere to go
// without ever reaching its neighbour's.
function bandsFor(counts, span){
  const SPAN = span || 1800;
  // One strip per distinct weight, EVENLY spaced — the row is an ordinal, not
  // a measurement. Spacing proportional to the weight was tried first and its
  // whole effect was empty space: this corpus jumps 13 -> 19 at the top, so the
  // gap between the two highest rows was 561px, 30% of the drawing's height,
  // holding one node. Magnitude is already carried by node size and by colour;
  // a third encoding of it costs the layout and tells nobody anything, and a
  // 561px gap is not read as "six more inbound links" by anyone.
  const distinct = [...new Set(counts)].sort((a, b) => a - b);
  const k = distinct.length;
  const centre = (c) => (k > 1 ? (0.5 - distinct.indexOf(c) / (k - 1)) * SPAN : 0);
  // 0.45 rather than 0.5 so neighbouring strips are separated, not merely
  // adjacent — touching strips let a node sit exactly level with one a weight
  // below, which is the inversion this exists to rule out.
  const half = k > 1 ? (SPAN / (k - 1)) * 0.45 : SPAN / 4;
  return counts.map((c) => [centre(c) - half, centre(c) + half]);
}

function mapLayout(n, edges, seed, bias, span, pull, bands) {
  const rnd = (() => { let s = seed | 0; return () => {
    s = s + 0x6D2B79F5 | 0;
    let t = Math.imul(s ^ s >>> 15, 1 | s);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  }; })();
  const X = new Float64Array(n), Y = new Float64Array(n);
  const vx = new Float64Array(n), vy = new Float64Array(n);
  for (let i = 0; i < n; i++) {
    const a = rnd() * Math.PI * 2, d = 200 + rnd() * 260;
    X[i] = Math.cos(a) * d; Y[i] = Math.sin(a) * d;
    // Start inside the strip rather than relaxing into it: a node that begins
    // outside is clamped on the first step anyway, and the shove it takes on
    // the way is a force nothing asked for.
    if (bands) Y[i] = bands[i][0] + (bands[i][1] - bands[i][0]) * rnd();
  }
  const ITER = 400, SPRING = 260, REPEL = 9000;
  // Defaults are the LEAN setting, so a caller that passes only a bias gets
  // the picture that shipped first. Measured on this corpus: span is the dial
  // that matters and pull is not — 900 -> 1800 took the rank correlation
  // between height and inbound count from 0.61 to 0.87, while multiplying
  // pull fifteenfold at a fixed span bought 33% more separation and no more
  // order. Past 1800 nothing improves and the drawing turns portrait, which
  // wastes a wide screen.
  const BIAS_SPAN = span || 900, BIAS_PULL = pull || 0.02;
  for (let it = 0; it < ITER; it++) {
    const cool = 1 - it / ITER;
    for (let i = 0; i < n; i++) { vx[i] *= 0.85; vy[i] *= 0.85; }
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const dx = X[i] - X[j], dy = Y[i] - Y[j];
      const d2 = Math.max(1, dx * dx + dy * dy), d = Math.sqrt(d2);
      const f = REPEL / d2, ux = dx / d, uy = dy / d;
      vx[i] += ux * f; vy[i] += uy * f; vx[j] -= ux * f; vy[j] -= uy * f;
    }
    for (const [a, b] of edges) {
      const dx = X[b] - X[a], dy = Y[b] - Y[a];
      const d = Math.hypot(dx, dy) || 1;
      const f = (d - SPRING) * 0.0004 * d, ux = dx / d, uy = dy / d;
      vx[a] += ux * f; vy[a] += uy * f; vx[b] -= ux * f; vy[b] -= uy * f;
    }
    // Weak pull to the origin, or the unconnected drift away without limit.
    // With a bias the vertical half of that pull is REPLACED rather than added
    // to: two targets on one axis fight, and the result is a layout that obeys
    // neither. Gentle enough that the springs still decide who sits by whom —
    // this leans the picture, it does not sort it into rows.
    for (let i = 0; i < n; i++) {
      vx[i] -= X[i] * 0.0012;
      // Bands own the vertical axis outright — no pull toward them, because a
      // pull is a force that can be outvoted and that is exactly the failure
      // being fixed.
      if (bands) vy[i] = 0;
      else if (bias) vy[i] += ((0.5 - bias[i]) * BIAS_SPAN - Y[i]) * BIAS_PULL;
      else vy[i] -= Y[i] * 0.0012;
      X[i] += Math.max(-30, Math.min(30, vx[i])) * cool;
      Y[i] += Math.max(-30, Math.min(30, vy[i])) * cool;
      if (bands) Y[i] = Math.max(bands[i][0], Math.min(bands[i][1], Y[i]));
    }
  }
  return { X, Y };
}

// Collapsed by default: this is a reading tool, and the form should not sit
// between the reader and the memory.
function sendPanel(m) {
  const S = D.send || {ok:false, reason:"", targets:[]};
  if (!S.ok) {
    return `<details class="send"><summary>このメモを送る (使えません)</summary>
      <div class="meta">${esc(S.reason || "harness-cli に届きません")}</div></details>`;
  }
  if (!S.targets.length) {
    return `<details class="send"><summary>このメモを送る (宛先なし)</summary>
      <div class="meta">生きているタスクが自分以外にありません</div></details>`;
  }
  return `<details class="send"><summary>このメモを送る</summary>
    <div class="sendform">
      <select id="sd-to">${S.targets.map(t =>
        `<option value="${esc(t.topic)}"${t.self ? " selected" : ""}>${esc(t.label)}</option>`
      ).join("")}</select>
      <textarea id="sd-c" rows="3" placeholder="コメント (必須)"></textarea>
      <label><input type="checkbox" id="sd-m" checked> hook + description を添える</label>
      <label><input type="checkbox" id="sd-b"> 本文全体を添える (${kb(m.size)})${
        m.size > 60000 ? ` <span class="warn-inline">⚠ 64KiB の inline 上限に近い</span>` : ""
      }</label>
      <div><button id="sd-go">送信</button> <span class="meta" id="sd-r"></span></div>
    </div></details>`;
}

async function doSend() {
  const btn = $("sd-go"), out = $("sd-r");
  const comment = $("sd-c").value.trim();
  if (!comment) { out.textContent = "コメントが空です"; return; }
  btn.disabled = true; out.textContent = "送信中…";
  try {
    const r = await fetch("/send", {
      method: "POST", headers: {"Content-Type": "application/json"},
      body: JSON.stringify({
        topic: $("sd-to").value, comment,
        meta: $("sd-m").checked, body: $("sd-b").checked,
        project: P.key, file: sel.file, area: sel.area || "",
      }),
    });
    const j = await r.json();
    // The harness's own ok line reports bytes and source; show it verbatim
    // rather than a "sent!" of our own, which would be a claim we did not check.
    out.textContent = j.ok ? `${j.out || "ok"}  (${j.bytes}B)${j.warn ? "  ⚠ " + j.warn : ""}`
                           : `失敗: ${j.err || j.out}`;
    out.className = j.ok && j.warn ? "meta warn-inline" : "meta";
    if (j.ok) $("sd-c").value = "";
  } catch (e) {
    out.textContent = "失敗: " + e;
  } finally {
    btn.disabled = false;
  }
}

// The whole index, one row per line with what that line costs. This is the
// only view of the thing that is actually loaded every session; everything
// else in this tool shows a memory, which is read only on demand.
let idxSort = "file";
let idxArea = "";
const warnOpen = new Set();
let area = "";
// Which project's memories the cross view is showing. Separate from `area`
// because they are different axes and the cross view has no areas: sharing one
// variable would make leaving the cross view inherit a filter naming a project
// that the per-project view has no chip for, and nothing would clear it.
let projFilter = "";
function renderIndexView(a) {
  if (a !== undefined) idxArea = a;
  sel = null;
  const enc = new TextEncoder();
  const top = !idxArea;
  const name = top ? "MEMORY.md" : `${idxArea}/INDEX.md`;
  const raw = top ? (P.indexRaw || "") : ((P.subIndexRaw || {})[idxArea] || "");
  const rowsSrc = top ? P.index : ((P.subIndex || {})[idxArea] || []);
  const lines = raw.split("\n");
  const byFile = {};
  for (const r of rowsSrc) byFile[r.line] = r;

  let rows = lines.map((text, i) => {
    const n = i + 1, r = byFile[n];
    // +1 for the newline: the file is what is loaded, not the sum of its
    // visible characters.
    return {n, text, bytes: text ? enc.encode(text).length + 1 : 1, row: r};
  }).filter(x => x.text.trim() || x.bytes > 1);

  const total = rows.reduce((x, y) => x + y.bytes, 0);
  const nonRow = rows.filter(x => !x.row);
  if (idxSort === "bytes") rows = [...rows].sort((x, y) => y.bytes - x.bytes);


  $("detail").innerHTML = `
    <h2>${esc(name)}</h2>
    <div class="meta">${lines.length} 行 · ${top
        ? `${kb(P.indexBytes)} / ${kb(D.indexLimit)} · ${P.indexLines} / ${D.indexLimitLines} 行 上限 · 常時読み込み`
        : `${total}B · 触っているときだけ読む`} ·
      索引行 ${rowsSrc.length} · 索引行でない行 ${nonRow.length} (${bytes(nonRow.map(x=>x.text).join("\n"))})</div>
    <div class="ixsort">
      <button class="chip${idxSort==="file"?" on":""}" data-isort="file">行順</button>
      <button class="chip${idxSort==="bytes"?" on":""}" data-isort="bytes">大きい順</button>
      <button class="chip${idxSort==="raw"?" on":""}" data-isort="raw">raw</button>
      <span class="meta">合計 ${total}B</span>
    </div>` + (idxSort === "raw"
      // The file as it actually is. The table above is a reading of it; this
      // is the bytes that get injected, and the thing to copy out of.
      ? `<pre class="rawidx">${esc(raw)}</pre>`
      : `<table class="idxtbl"><tbody>${rows.map(x => `
      <tr class="${x.row ? "" : "notrow"}">
        <td class="n">${x.n}</td>
        <td class="b">${x.bytes}</td>
        <td>${x.row
          ? `<a href="#" data-goto="${esc(x.row.file.replace(/\.md$/, "").split("/").pop())}">${esc(x.row.title)}</a>`
            + (x.row.hook ? ` — ${x.row.hookHtml}` : ` <span class="ixnone">— hook が無い</span>`)
          : (x.text.match(/\*\*([A-Za-z0-9_-]+)\/INDEX\.md\*\*/)
              ? `<a href="#" data-idxarea="${esc(RegExp.$1)}">${esc(x.text)}</a>`
              : `<span class="meta">${esc(x.text)}</span>`)}</td>
      </tr>`).join("")}</tbody></table>`);
  $("detail").scrollTop = 0;
  if (narrow()) {
    $("detail").style.scrollMarginTop = document.querySelector("header").offsetHeight + "px";
    $("detail").scrollIntoView();
  }
}

function goto(name) {
  const m = P.memories.find(x => x.name === name || x.file === name + ".md");
  if (!m) return;
  sel = m; renderList(); renderDetail();
  // Under 800px the panes stack, so the detail lands below a list that can be
  // 48 orphans long: without this, tapping a memory looks like nothing happened.
  if (narrow()) {
    // The header is sticky, so a bare scrollIntoView parks the memory's own
    // title underneath it. Measure it rather than hard-coding a height: the
    // header wraps to a different number of rows at every width.
    $("detail").style.scrollMarginTop = document.querySelector("header").offsetHeight + "px";
    $("detail").scrollIntoView();
  }
}

// Direction, scoped to one node. Globally it says nothing — every edge is
// somebody's outbound and somebody else's inbound, so a global direction
// filter selects the whole set. Against ONE node it is the real question:
// what does this reach, and what reaches it.
function mapHover(name){
  const lines = $("mapcam").querySelectorAll("line");
  if (!name) { lines.forEach(l => l.classList.remove("hout","hin")); return; }
  const g = mapState.g;
  const i = g.index.get(name);
  if (i === undefined) return;
  const mem = P.memories[i];
  const out = new Set(), inn = new Set();
  for (const raw of mem.links) {
    const j = g.resolve(raw);
    if (j >= 0 && j !== i) out.add(j);
  }
  for (const n of mem.inbound) if (g.index.has(n)) inn.add(g.index.get(n));
  lines.forEach((l) => {
    const a = +l.dataset.a, b = +l.dataset.b;
    l.classList.remove("hout","hin");
    const other = a === i ? b : (b === i ? a : -1);
    if (other < 0) return;
    if (out.has(other)) l.classList.add("hout");
    else if (inn.has(other)) l.classList.add("hin");
  });
}

$("mapsvg").addEventListener("pointerover", (e) => {
  if (!mapState.open) return;
  const g = e.target.closest("[data-mapnode]");
  mapHover(g ? g.dataset.mapnode : null);
});

$("mapneigh").addEventListener("change", (e) => { mapState.neighbours = e.target.checked; renderMap(); });
$("mapcolor").addEventListener("change", (e) => { mapState.colorBy = e.target.value; renderMap(); });
$("mapsize").addEventListener("change", (e) => { mapState.sizeBy = e.target.value; renderMap(); });
$("maplabels").addEventListener("change", (e) => { mapState.labels = e.target.checked; renderMap(); });
// The bias changes WHERE the nodes are, so it re-runs the layout rather than
// just repainting. 94 ms on the largest project, which is a click, not a wait.
$("mapbias").addEventListener("change", (e) => { mapState.bias = e.target.value; mapRelayout(); mapFit(); });
// Changing WHICH count drives the vertical axis moves the nodes, so it is a
// relayout too — but only while the axis is actually in use.
$("mapby").addEventListener("change", (e) => { mapState.by = e.target.value;
  if (mapState.bias) { mapRelayout(); mapFit(); } });

// Zoom about the CURSOR, not the centre: zooming toward a corner otherwise
// walks the thing you were pointing at off the screen and you pan it back by
// hand every time.
$("mapsvg").addEventListener("wheel", (e) => {
  if (!mapState.open) return;
  e.preventDefault();
  const r = $("mapsvg").getBoundingClientRect();
  const mx = e.clientX - r.left, my = e.clientY - r.top;
  const nk = Math.max(0.05, Math.min(12, mapState.cam.k * Math.exp(-e.deltaY * 0.0015)));
  mapState.cam.x = mx - (mx - mapState.cam.x) * (nk / mapState.cam.k);
  mapState.cam.y = my - (my - mapState.cam.y) * (nk / mapState.cam.k);
  mapState.cam.k = nk;
  mapCam();
}, {passive:false});

// Panning listens on the WINDOW for the duration of a drag, and the pointer is
// never captured.
//
// Both halves are load-bearing and each was learned by breaking the other.
// setPointerCapture on pointerdown redirects the click that follows to the
// capturing element, so every click on a node arrived with target = the <svg>
// and the map looked completely unclickable. Dropping the capture and leaving
// the listeners on the <svg> then broke panning the other way: the moment the
// pointer crosses onto anything that is not the svg — the HUD sits right there
// in a corner — the svg stops hearing it and the drag dies mid-gesture.
let mapDrag = null;
function mapDragMove(e){
  if (!mapDrag) return;
  const dx = e.clientX - mapDrag.x, dy = e.clientY - mapDrag.y;
  if (!mapDrag.moved) {
    if (Math.hypot(dx, dy) <= 3) return;   // a press with a tremor is a click
    mapDrag.moved = true;
    $("mapsvg").classList.add("drag");
  }
  mapState.cam.x = mapDrag.cx + dx; mapState.cam.y = mapDrag.cy + dy;
  mapCam();
}
function mapDragEnd(){
  window.removeEventListener("pointermove", mapDragMove);
  window.removeEventListener("pointerup", mapDragEnd);
  $("mapsvg").classList.remove("drag");
  // Cleared on the next tick, not here: the click that follows a plain press
  // still has to be able to ask whether this gesture was a drag.
  const was = mapDrag;
  setTimeout(() => { if (mapDrag === was) mapDrag = null; }, 0);
}
$("mapsvg").addEventListener("pointerdown", (e) => {
  if (!mapState.open) return;
  mapDrag = {x:e.clientX, y:e.clientY, cx:mapState.cam.x, cy:mapState.cam.y, moved:false};
  window.addEventListener("pointermove", mapDragMove);
  window.addEventListener("pointerup", mapDragEnd);
});

// The map. Its state lives here rather than in the DOM because the camera has
// to survive a round trip out to a memory and back — walking out to a node and
// returning is the expected loop, and a camera that reset would undo the pan
// that got you there.
const mapState = {open:false, cam:{x:0,y:0,k:1}, colorBy:"area", sizeBy:"in",
                  labels:false, area:"", neighbours:false, bias:"", by:"in", g:null, pos:null,
                  drawn:{nodes:0, edges:0, filtered:false}};

// One number per node in [0,1] for the vertical bias: inbound count over the
// largest inbound count. Linear, and the long tail is the POINT rather than a
// problem to normalise away.
//
// The first version used a rank — the share of the project with strictly fewer
// inbound links — on the reasoning that a long-tailed distribution would press
// everything against the floor. That was backwards, and measurably so. A rank
// spends its range where the nodes are, and 83 of this corpus's 169 memories
// have at most one inbound link, so the whole heavy end was compressed into a
// 45px sliver: inbound 19 targeted -900, inbound 13 targeted -894, inbound 8
// targeted -857. Inside a band that thin the springs decide everything, and the
// single most-linked memory in the project came out BELOW several memories with
// half its inbound count.
//
// Measured over the four mappings, at the layer setting:
//
//   mapping   rank-correlation   inversions among   the heaviest node
//                                per-count means    lands in the top
//   rank                 0.870             31/91                  35%
//   linear               0.881              9/91                   4%
//   sqrt                 0.898             25/91                  22%
//   log                  0.883             28/91                  32%
//
// Note what that table also says about the metric: rank correlation barely
// separates the four, because it is dominated by the 83 light nodes whose order
// among themselves nobody is asking about. It was the wrong thing to optimise.
function biasWeights(counts){
  const max = Math.max(0, ...counts);
  return counts.map((c) => (max > 0 ? c / max : 0.5));
}

// Two settings, and they differ in KIND rather than in strength.
//
// "lean" is a force: inbound count pulls a node up, the springs pull it toward
// its neighbours, and the drawing is the compromise. It claims no ordering, and
// it should not be read as one.
//
// "layer" is a constraint: y is a function of inbound count and nothing else,
// so two different counts cannot appear in the wrong order. The soft setting
// was tried for this job first and got the ordering right about nine times in
// ten, which the operator called uncomfortable and was right to — a drawing
// that is nearly ordered gets read as ordered.
function mapLayoutNow(){
  const n = mapState.g.names.length, edges = mapState.g.edges;
  if (mapState.bias === "layer")
    return mapLayout(n, edges, 1234567, null, null, null, bandsFor(mapCounts(), 1800));
  if (mapState.bias === "lean")
    return mapLayout(n, edges, 1234567, biasWeights(mapCounts()), 900, 0.02);
  return mapLayout(n, edges, 1234567);
}

function mapRelayout(){
  mapState.pos = mapLayoutNow();
  renderMap();
}

function openMap(){
  if (P.cross) return;           // links never cross projects; nothing to draw
  mapState.open = true;
  // Recomputed per open rather than cached: the corpus is re-read on every
  // request, so a cached layout could describe memories that have changed.
  mapState.g = projectGraph(P.memories);
  mapState.pos = mapLayoutNow();
  $("mapview").hidden = false; $("mapbar").hidden = false;
  document.querySelector("main").hidden = true;
  $("mapbtn").classList.add("on");
  mapAreaChips();
  renderMap();
  if (mapState.cam.k === 1 && mapState.cam.x === 0 && mapState.cam.y === 0) mapFit();
  else mapCam();
}

function closeMap(){
  mapState.open = false;
  $("mapview").hidden = true; $("mapbar").hidden = true;
  document.querySelector("main").hidden = false;
  $("mapbtn").classList.remove("on");
}

const MAP_AREA_COLORS = ["#6bb3f7","#5cc98a","#f0a060","#c080f0","#e06c75",
                         "#56b6c2","#d19a66","#98c379","#c678dd","#abb2bf"];
const MAP_TYPE_COLORS = {feedback:"#f0a060", project:"#5cc98a",
                         reference:"#5aabf7", user:"#c080f0", "?":"#888"};

// A sentinel rather than "": a directory cannot be called this, and the select
// needs a value for the top level that is not also the value for "everything".
const MAP_TOP = "(top)";

function mapAreas(){ return [...new Set(P.memories.map(m => m.area || ""))].sort(); }

function mapColor(nd, i){
  if (mapState.colorBy === "type") return MAP_TYPE_COLORS[nd.type] || "#888";
  if (mapState.colorBy === "deg" || mapState.colorBy === "outdeg") {
    const c = mapState.colorBy === "outdeg" ? mapState.g.out[i] : nd.inbound.length;
    const t = Math.min(1, c / 12);
    return `hsl(${210 - t*200},70%,${45 + t*15}%)`;
  }
  if (mapState.colorBy === "age") {
    const t = Math.min(1, (Date.now()/1000 - nd.mtime) / 86400 / 180);
    return `hsl(${200 - t*200},55%,${60 - t*15}%)`;
  }
  const areas = mapAreas();
  return MAP_AREA_COLORS[areas.indexOf(nd.area || "")%MAP_AREA_COLORS.length];
}

function mapRadius(nd, i){
  if (mapState.sizeBy === "flat") return 4;
  if (mapState.sizeBy === "sz") return 3 + Math.sqrt(nd.size / 700);
  const c = mapState.sizeBy === "out" ? mapState.g.out[i] : nd.inbound.length;
  return 3 + Math.sqrt(c) * 1.6;
}

// Same trimming the ego graph uses, and for the same reason: nearly every name
// starts with its type, so without this the visible characters are the ones
// every node shares.
function mapLabel(name){
  const t = name.replace(/^(feedback|project|reference|user)[_-]/, "");
  return t.length > 20 ? t.slice(0, 19) + "\u2026" : t;
}

// The two countable things a memory has, and they ask different questions:
// inbound is what the rest of the project leans on, outbound is what one
// memory gathers up. Worth offering both because the two orderings disagree,
// and measured on this repo they disagree ASYMMETRICALLY: the top of inbound
// (feedback_verify_llm_framing, 19 in) is also high in outbound at 6, only 5
// memories above it — but the top of outbound
// (reference_herdr_for_harness_design, 20 out) has 2 inbound with 59 above it.
// So a memory can be a hub by gathering without anything pointing back at it,
// which is exactly the shape the inbound ordering cannot show you.
function mapCounts(){
  return mapState.by === "out"
    ? mapState.g.out
    : P.memories.map((m) => m.inbound.length);
}

// Which nodes the map draws. Narrowing WHICH nodes appear is not the same as
// forcing WHERE they sit: the layout stays innocent of areas, because the links
// do not group by area and a layout that pretended otherwise would be asserting
// a structure the data does not have.
//
// The neighbours toggle exists because a subsystem on its own is mostly dots —
// measured: user has 5 memories and 0 internal edges, history 9 and 1. It is
// off by default because on (top) the neighbours are everything else.
// "" means every node; MAP_TOP means the top level only. They are different
// questions and the empty string cannot carry both — top-level IS an area here
// (100 of the 169 memories, and 177 of the 375 edges), and being unable to look
// at it alone was the one thing the area filter existed for.
// Same strip the list carries, drawn from the data for the same reason: an
// area added tomorrow has to appear without an edit here.
function mapAreaChips(){
  const n = new Map();
  for (const m of P.memories) n.set(m.area || "", (n.get(m.area || "") + 1) || 1);
  const areas = [...n.keys()].sort();
  const chip = (val, label) =>
    `<button class="chip${mapState.area === val ? " on" : ""}" data-maparea="${esc(val)}">${label}</button>`;
  $("mapareas").innerHTML = chip("", "\u5168\u90e8")
    + areas.map((a) => chip(a === "" ? MAP_TOP : a,
        esc(a === "" ? MAP_TOP : a + "/") + " " + n.get(a))).join("");
}

function mapVisible(){
  const g = mapState.g;
  if (!mapState.area) return {show: new Set(g.names.map((_, i) => i)), dim: new Set()};
  const want = mapState.area === MAP_TOP ? "" : mapState.area;
  const show = new Set();
  g.names.forEach((_, i) => {
    if ((P.memories[i].area || "") === want) show.add(i);
  });
  const dim = new Set();
  if (mapState.neighbours) {
    for (const [a, b] of g.edges) {
      if (show.has(a) && !show.has(b)) dim.add(b);
      if (show.has(b) && !show.has(a)) dim.add(a);
    }
  }
  return {show, dim};
}

function renderMap(){
  const {names, edges} = mapState.g, {X, Y} = mapState.pos;
  const vis = mapVisible();
  const drawn = (i) => vis.show.has(i) || vis.dim.has(i);
  const xy = (v) => v.toFixed(1);
  let h = "";
  for (const [a, b] of edges) {
    if (!drawn(a) || !drawn(b)) continue;
    const mut = mapState.g.mutual.has(a + ":" + b) ? " mut" : "";
    h += `<line class="e${mut}" data-a="${a}" data-b="${b}" x1="${xy(X[a])}" y1="${xy(Y[a])}" x2="${xy(X[b])}" y2="${xy(Y[b])}"/>`;
  }
  names.forEach((name, i) => {
    if (!drawn(i)) return;
    const nd = P.memories[i], r = mapRadius(nd, i);
    h += `<g class="${vis.dim.has(i) ? "dim" : ""}" data-mapnode="${esc(name)}"><title>${esc(name)}${nd.area ? "  [" + esc(nd.area) + "/]" : ""}  \u2190${nd.inbound.length} \u2192${mapState.g.out[i]}</title>`
       + `<circle cx="${xy(X[i])}" cy="${xy(Y[i])}" r="${r.toFixed(1)}" fill="${mapColor(nd, i)}"/>`
       + (mapState.labels
            ? `<text x="${xy(X[i]+r+3)}" y="${xy(Y[i]+3)}">${esc(mapLabel(name))}</text>` : "")
       + `</g>`;
  });
  $("maplegend").innerHTML = mapState.colorBy === "area"
    ? mapAreas().map((a,i) => `<span style="color:${MAP_AREA_COLORS[i%MAP_AREA_COLORS.length]}">\u25cf</span>${esc(a||"(top)")} `).join("")
    : (mapState.colorBy === "type"
        ? Object.entries(MAP_TYPE_COLORS).map(([k,v]) => `<span style="color:${v}">\u25cf</span>${k} `).join("") : "");
  $("mapcam").innerHTML = h;
  // Count what was DRAWN, not what exists. With a filter on, a HUD reporting
  // the whole graph contradicts the picture beside it.
  mapState.drawn = {
    nodes: names.filter((_, i) => drawn(i)).length,
    edges: edges.filter(([a, b]) => drawn(a) && drawn(b)).length,
    filtered: !!mapState.area,
  };
  mapHud();
}

function mapHud(){
  const d = mapState.drawn;
  const of = d.filtered ? ` / ${mapState.g.names.length} · ${mapState.g.edges.length}` : "";
  $("maphud").textContent = `${d.nodes} nodes · ${d.edges} edges${of} · zoom ${mapState.cam.k.toFixed(2)}x`;
}

function mapCam(){
  $("mapcam").setAttribute("transform",
    `translate(${mapState.cam.x} ${mapState.cam.y}) scale(${mapState.cam.k})`);
  mapHud();
}

function mapFit(){
  const {X, Y} = mapState.pos;
  if (!X.length) return;
  let x0=Infinity, y0=Infinity, x1=-Infinity, y1=-Infinity;
  for (let i=0;i<X.length;i++){ x0=Math.min(x0,X[i]); x1=Math.max(x1,X[i]);
                                y0=Math.min(y0,Y[i]); y1=Math.max(y1,Y[i]); }
  const w = $("mapsvg").clientWidth, h = $("mapsvg").clientHeight;
  const k = Math.min(w/(x1-x0+120), h/(y1-y0+120));
  mapState.cam.k = k;
  mapState.cam.x = w/2 - ((x0+x1)/2)*k;
  mapState.cam.y = h/2 - ((y0+y1)/2)*k;
  mapCam();
}

function render() {
  areaChips();
  // Links never cross projects, so a union map is fifteen disconnected islands
  // whose largest is the one already reachable by picking that project.
  $("mapbtn").hidden = !!P.cross;
  if (P.cross && mapState.open) closeMap();
  const w = warnBlock();
  $("warn").innerHTML = w;
  // The heading is static markup, so an empty panel would leave 要保守 sitting
  // over nothing — which reads as "no findings" rather than "not applicable".
  $("warnsec").hidden = !w;
  renderList();
  renderDetail();
}

// The type chips are markup, so their lit state has to be pushed when
// something other than a click changes the filter.
function syncTypeChips() {
  document.querySelectorAll(".chip[data-type]").forEach(c =>
    c.classList.toggle("on", types.has(c.dataset.type)));
}

document.addEventListener("click", (e) => {
  if (e.target.id === "sd-go") { doSend(); return; }
  if (e.target.id === "mapbtn") { mapState.open ? closeMap() : openMap(); return; }
  if (e.target.id === "mapfit") { mapFit(); return; }
  const mn = e.target.closest("[data-mapnode]");
  if (mn) {
    // Closing is not incidental: the map occupies both panes, so a click that
    // only selected would look like nothing happened. mapState.cam is kept, so
    // re-opening returns to the same view — walking out to a memory and back is
    // the expected loop.
    if (mapDrag && mapDrag.moved) return;   // a pan that ended on a node is not a click
    closeMap();
    goto(mn.dataset.mapnode);
    return;
  }
  if (e.target.id === "open-index") { e.preventDefault(); renderIndexView(""); return; }
  const ia = e.target.closest("[data-idxarea]");
  if (ia) { e.preventDefault(); renderIndexView(ia.dataset.idxarea); return; }
  const wm = e.target.closest("[data-warnmore]");
  if (wm) {
    const k = wm.dataset.warnmore;
    warnOpen.has(k) ? warnOpen.delete(k) : warnOpen.add(k);
    $("warn").innerHTML = warnBlock();
    return;
  }
  const is = e.target.closest("[data-isort]");
  if (is) { idxSort = is.dataset.isort; renderIndexView(); return; }
  const t = e.target.closest("[data-goto]");
  if (t) { e.preventDefault(); goto(t.dataset.goto); return; }
  const mac = e.target.closest("[data-maparea]");
  if (mac) { mapState.area = mac.dataset.maparea; mapAreaChips(); renderMap(); return; }
  const pc = e.target.closest("[data-proj]");
  if (pc) {
    // No index view to open alongside it, unlike an area: a project filter
    // narrows the union, it does not point at a file.
    projFilter = pc.dataset.proj;
    projChips(); renderList();
    return;
  }
  const ac = e.target.closest("[data-area]");
  if (ac) {
    area = ac.dataset.area;
    areaChips(); renderList();
    // Filtering to a place is also how you open that place's index: the
    // detail pane is empty at that moment anyway, and the index is the thing
    // that says what is in there.
    if (P.subIndex && P.subIndex[area]) renderIndexView(area);
    else if (!area || area === "_top") renderIndexView("");
    return;
  }
  const c = e.target.closest(".chip");
  if (c) {
    if (c.dataset.type) {
      c.classList.toggle("on");
      types.has(c.dataset.type) ? types.delete(c.dataset.type) : types.add(c.dataset.type);
    } else if (c.dataset.view) {
      document.querySelectorAll(".chip[data-view]").forEach(x => x.classList.remove("on"));
      c.classList.add("on"); view = c.dataset.view;
    }
    renderList();
  }
});
$("q").addEventListener("input", (e) => { q = e.target.value; renderList(); });
$("proj").addEventListener("change", (e) => {
  const was = P && P.cross;
  P = D.projects[+e.target.value];
  sel = null; area = ""; projFilter = "";
  // A different project is a different graph, so the camera from the last one
  // means nothing over it.
  if (mapState.open) closeMap();
  mapState.cam = {x:0, y:0, k:1};
  // Entering the cross view preselects `feedback`: 351 memories on one screen
  // is a wall, and the 139 that are about how to work are the reason this view
  // exists. The chips are right there to widen it. Leaving clears the filter
  // again rather than carrying a default nobody asked for into a project view.
  if (P && P.cross && !was) types = new Set(["feedback"]);
  else if (was && !(P && P.cross)) types = new Set();
  syncTypeChips();
  render();
});

const narrow = () => window.matchMedia("(max-width:800px)").matches;
$("back").addEventListener("click", () => $("list").scrollIntoView());
addEventListener("scroll", () => {
  $("back").hidden = !narrow() || $("detail").getBoundingClientRect().top > 40;
}, {passive: true});

const blank = (msg) => { document.body.innerHTML = `<p style='padding:2rem'>${msg}</p>`; };

async function boot() {
  // The embedded tag is present only in the `-o` file. Its absence IS the
  // signal to fetch, so neither mode needs to be told which one it is.
  const tag = $("data");
  try {
    if (tag) {
      D = JSON.parse(tag.textContent);
    } else {
      const r = await fetch("/api/payload", {cache: "no-store"});
      if (!r.ok) throw new Error("HTTP " + r.status);
      D = await r.json();
    }
  } catch (e) {
    // Reached when the shell arrived but the payload did not: the server died
    // between the two requests, or build_payload() raised and the 500 came
    // back instead. A plain server-is-down does NOT land here — then `/` never
    // loads either. Without this the page stays blank, which reads as "there
    // are no memories".
    blank("/api/payload を取得できません — サーバが落ちたか、コーパスの読み込みに失敗しています: " + e);
    return;
  }
  P = D.projects[0] || null;
  projOptions();
  if (P) render(); else blank("memory が見つかりません");
}
boot();
"""


def page(payload: dict | None = None) -> str:
    """The page. With a payload it is self-contained; without one it is the
    ~25kB shell that fetches /api/payload — 1.3% of what the two weigh as one.
    Both render the same build_payload() output, so there is nothing to keep
    in step between the modes."""
    data = "" if payload is None else (
        '<script id="data" type="application/json">'
        + json.dumps(payload, ensure_ascii=False).replace("</", "<\\/")
        + "</script>")
    return f"""<!DOCTYPE html>
<html lang="ja"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claude memory</title><style>{CSS}</style></head><body>
<header>
  <h1>claude memory</h1>
  <select id="proj"></select>
  <input id="q" type="search" placeholder="検索 (空白で AND)">
  <button class="chip" data-type="feedback">feedback</button>
  <button class="chip" data-type="project">project</button>
  <button class="chip" data-type="reference">reference</button>
  <button class="chip" data-type="user">user</button>
  <span style="width:1rem"></span>
  <button class="chip on" data-view="all">名前順</button>
  <button class="chip" data-view="big">大きい順</button>
  <button class="chip" data-view="old">古い順</button>
  <button class="chip" data-view="linked">被リンク順</button>
  <button class="chip" id="mapbtn">🗺 地図</button>
  <span class="meta" id="count"></span>
</header>
<div id="areas" class="arearow"></div>
<div id="mapbar" class="mapbar" hidden>
  <label>色 <select id="mapcolor">
    <option value="area">area</option><option value="type">type</option>
    <option value="age">古さ</option><option value="deg">被リンク数</option><option value="outdeg">リンク数</option>
  </select></label>
  <label>大きさ <select id="mapsize">
    <option value="in">被リンク数</option><option value="out">リンク数</option><option value="sz">サイズ</option><option value="flat">一定</option>
  </select></label>
  <label><input type="checkbox" id="maplabels"> ラベル</label>
  <label>縦位置 <select id="mapbias">
    <option value="">使わない</option>
    <option value="lean">上へ傾ける</option>
    <option value="layer">上から層にする</option>
  </select></label>
  <label>基準 <select id="mapby">
    <option value="in">被リンク数</option>
    <option value="out">リンク数</option>
  </select></label>
  <label><input type="checkbox" id="mapneigh"> 隣接も含める</label>
  <button class="chip" id="mapfit">全体に合わせる</button>
  <span class="meta" id="maplegend"></span>
  <span class="mapareas" id="mapareas"></span>
</div>
<div id="mapview" hidden><svg id="mapsvg"><g id="mapcam"></g></svg><div class="maphud" id="maphud"></div></div>
<main>
  <div class="pane">
    <h2 class="sec" id="warnsec">要保守</h2><div id="warn"></div>
    <h2 class="sec">memories</h2><div id="list"></div>
  </div>
  <div class="pane detail" id="detail"></div>
  <button id="back" hidden>\u2191 \u4e00\u89a7</button>
</main>
{data}
<script>{JS}</script>
</body></html>"""


# --------------------------------------------------------------------------
# entry points
# --------------------------------------------------------------------------


def print_check() -> int:
    worst = 0
    for p in build_payload()["projects"]:
        # The cross-project entry has no findings by construction — every check
        # resolves within one project. Skipped explicitly rather than relying on
        # its empty lists, so a check added later cannot start reporting a union
        # figure here without someone deciding it should.
        if p.get("cross"):
            continue
        c = p["checks"]
        n = (len(c["orphans"]) + len(c["dangling"]) + len(c["missing_line"])
             + len(c["missing_file"]) + len(c["unreachable"]))
        over = (p["indexBytes"] >= INDEX_TARGET_BYTES
                or p["indexLines"] >= INDEX_TARGET_LINES)
        if not n and not over and not c["similar"]:
            continue
        worst = max(worst, 1)
        print(f"\n## {p['label']}  ({len(p['memories'])} memories, index "
              f"{p['indexBytes']/1000:.1f}kB / {p['indexLines']} lines)")
        if over:
            # Name WHICH axis is the near one. "compact the index" is acted on
            # differently depending on the answer: fewer bytes per line, or
            # fewer lines.
            near = ("lines" if p["indexLines"] / INDEX_LIMIT_LINES
                    > p["indexBytes"] / INDEX_LIMIT_BYTES else "bytes")
            print(f"  index: {p['indexBytes']/1000:.1f}kB of {INDEX_LIMIT_BYTES/1000:.1f}kB, "
                  f"{p['indexLines']} of {INDEX_LIMIT_LINES} lines — {near} run out first; "
                  f"compact to {INDEX_TARGET_BYTES/1000:.1f}kB / {INDEX_TARGET_LINES} lines")
        if c["orphans"]:
            print(f"  orphan ({len(c['orphans'])}): " + ", ".join(c["orphans"][:8]) + (" …" if len(c["orphans"]) > 8 else ""))

        # Every cap says what it hid. A list silently cut at 12 reads as the
        # whole set, and the reader then counts it and compares that number
        # against a later run — which is the same partial-observation mistake
        # the corpus this tool reads has a memory about.
        def capped(items, cap, render, label):
            for it in items[:cap]:
                print("  " + render(it))
            if len(items) > cap:
                print(f"  … {label} 他 {len(items) - cap} 件 (全 {len(items)} 件)")

        capped(c["dangling"], 12, lambda d:
               f"dangling: {d['from']} -> [[{d['target']}]]" +
               (f"   (did you mean {d['suggest']}?)" if d.get("suggest") else ""), "dangling")
        capped(c["unreachable"], 10, lambda f: f"archived but unreachable: {f}", "unreachable")
        capped(c["missing_line"], 10, lambda f: f"no index line: {f}", "no index line")
        capped(c["missing_file"], 10, lambda f: f"index line, no file: {f}", "index line, no file")
        capped(c["similar"], 8, lambda s: f"similar {s['score']}: {s['a']} <-> {s['b']}", "similar")
    return worst


# --------------------------------------------------------------------------
# sending a memory to another agent
#
# The viewer is otherwise read-only, and this is the one thing that leaves the
# machine. Three guards, because "on loopback" is not by itself an
# authorisation: SEND_ENABLED is false unless the bind is loopback, the topic
# has to be one this process just discovered (so there is no free-text field
# that could publish anywhere), and a missing harness-cli disables the panel
# with the reason rather than failing at submit time.
# --------------------------------------------------------------------------

SEND_ENABLED = False
CLI = "harness-cli"

# cli/agent/json_emit.go: hookInlineLimit = 64 * 1024. A larger payload is
# accepted, delivered and acked — and then NOT spliced into the recipient's
# wake prompt; the hook record carries the size and a command to fetch it
# instead. So an oversized send looks exactly like a successful one from here,
# which is the failure worth naming rather than the rejection at 1 MiB
# (ErrPayloadTooLarge, agentboard/board.go) that at least says no.
HOOK_INLINE_LIMIT = 64 * 1024


def _cli(args: list[str], stdin: str | None = None, timeout: int = 60):
    """Run harness-cli. Returns (ok, stdout, stderr). Never raises."""
    try:
        r = subprocess.run([CLI, *args], input=stdin, capture_output=True,
                           text=True, timeout=timeout)
        return r.returncode == 0, r.stdout, r.stderr
    except FileNotFoundError:
        return False, "", f"{CLI} が PATH にありません"
    except subprocess.TimeoutExpired:
        return False, "", f"{CLI} {' '.join(args[:2])} が {timeout}s で応答しません"
    except OSError as e:
        return False, "", str(e)


def send_targets() -> dict:
    """Live agents addressable as chat.<first-8-hex>, minus this task.

    Joined from `ls` (which knows the agent and the status) and `board topics`
    (which knows a topic exists at all). A task whose topic has no subscriber
    is still listed but marked, because "sent, nobody home" is a real outcome
    the operator should see BEFORE sending rather than after.
    """
    if not SEND_ENABLED:
        return {"ok": False, "reason": "loopback bind のときだけ有効です", "targets": []}
    ok, out, err = _cli(["ls", "--json"])
    if not ok:
        return {"ok": False, "reason": (err or out).strip()[:200], "targets": []}
    try:
        tasks = json.loads(out).get("tasks", [])
    except json.JSONDecodeError as e:
        return {"ok": False, "reason": f"ls --json を解釈できません: {e}", "targets": []}

    # Subscriber counts are gated on board_observe; without it the server
    # answers denied, not an empty board. Degrade to "unknown" rather than
    # letting a missing capability render as "nobody is listening" on every
    # row — that reads as a fact and is not one.
    subs: dict[str, str] | None = None
    tok, tout, _ = _cli(["board", "topics"])
    if tok:
        subs = {}
        for line in tout.split("\n"):
            f = line.split()
            if f and f[0].startswith("chat."):
                subs[f[0]] = next((x.split("=", 1)[1] for x in f if x.startswith("subs=")), "?")

    # Every live task, INCLUDING the one that launched this viewer. An earlier
    # version excluded "self" and that was wrong twice over: the exclusion
    # keyed on HARNESS_TASK_ID, which is whatever the launching shell happened
    # to carry — so the target list changed depending on who started the
    # server — and sending to your own agent is a real use, the way to hand it
    # a memory through the inbox without interrupting the conversation.
    me = (os.environ.get("HARNESS_TASK_ID") or "")[:8]
    live = []
    for t in tasks:
        if t.get("status") in ("succeeded", "failed", "cancelled"):
            continue
        short = (t.get("id") or "")[:8]
        if not short:
            continue
        topic = f"chat.{short}"
        live.append({
            "topic": topic,
            "self": short == me,
            "label": f"{topic} — {t.get('agent') or '?'} ({t.get('status')})"
                     + ("  ※このビューアを起動したタスク" if short == me else "")
                     + ("" if subs is None or subs.get(topic, "0") != "0" else "  ※購読者なし"),
        })
    # The launching task first, then by topic. It is the agent whose work the
    # viewer was opened alongside, so it is the one a comment is usually for —
    # and being first makes it the default selection with no extra markup.
    live.sort(key=lambda x: (not x["self"], x["topic"]))
    return {"ok": True, "reason": "", "targets": live}


# Every message from here is a TOOL send: a human clicked a button in a viewer
# that has no inbox of its own. Nothing in the envelope says so — the `from`
# block names whichever task's env launched the viewer, which is usually the
# RECIPIENT (send-to-self is the common case, see send_targets), so a reply on
# the board is either a loop back to the reader or a message into a task that
# is not waiting for one. The recipient cannot tell any of that from the
# envelope: the first one (2026-09-11) spent four tool calls deriving it,
# re-reading the inbox JSON and this file's own memory, before answering.
# Saying it in the message costs one line and removes the derivation.
#
# It says where to ANSWER, not that the message can be ignored — a bare
# "返信不要" reads as "no action needed", which is the opposite of why a human
# sent it.
TOOL_NOTE = ("(memviewer からのツール送信 — 返信先はありません。"
             "board に送り返さず、自分の会話で答えてください)")

# The comment is the ONLY part of the message a human wrote; every other line
# is generated here. Unmarked they run together, and the recipient cannot see
# where the operator's words stop: the 2026-09-20 send put a one-line comment
# between the header and a bare `index:` row, and the reader had to guess
# whether `index:`/`desc:`/`file:` were part of what was said. Two delimiters
# cost two lines and remove the guess.
#
# The closer is emitted even when nothing follows it, so the extent is read
# off the message rather than inferred from whether a later block happens to
# be attached — with both checkboxes off there is no later block at all.
COMMENT_OPEN = "--- ここから操作者 (人間) が書いたコメント ---"
COMMENT_CLOSE = "--- 操作者のコメントここまで ---"


def compose(mem: Memory, ix: dict | None, comment: str, meta: bool, body: bool) -> str:
    """The message text. Comment FIRST, and fenced: it is the thing being said,
    and the only part of the message that is not machine-generated.

    Takes the Memory read fresh from disk, never text posted by the page. The
    page sends identifiers; the content comes from the file. That keeps the
    raw body out of the served HTML and makes what is sent match what is on
    disk at the moment of sending.

    TOOL_NOTE rides with the header rather than the foot: with 本文全体
    attached the tail is thousands of bytes away, and past the inline limit it
    is not in the recipient's wake context at all.
    """
    out = [f"[memviewer] {mem.name}", TOOL_NOTE, "",
           COMMENT_OPEN, comment.strip(), COMMENT_CLOSE, ""]
    if meta:
        out.append(f"index: {ix['title']} — {ix['hook']}" if ix else "index: (MEMORY.md に行が無い)")
        if mem.description:
            out.append(f"desc: {mem.description}")
        out.append(f"file: {mem.area + '/' if mem.area else ''}{mem.path.name}")
        out.append("")
    if body:
        out += [f"--- 全文 ({mem.size}B) ---", mem.body.strip()]
    return "\n".join(out).rstrip() + "\n"


def find_memory(project_key: str, area: str, filename: str):
    """(Memory, index row) read fresh, or (None, None). Path-traversal safe:
    the name has to MATCH one this process just scanned, so nothing the page
    posts is ever joined onto a path."""
    for p in scan():
        if p["key"] != project_key:
            continue
        for m in p["memories"]:
            if m.path.name == filename and m.area == area:
                rel = f"{area}/{filename}" if area else filename
                ix = next((r for r in p["index"] if r["file"] == rel), None)
                return m, ix
    return None, None


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        # Match on the PATH, not the raw target: a query string is how you
        # force a reload of a page whose url is otherwise identical, and
        # comparing the whole thing 404'd on `/?v=2`.
        path = self.path.split("?", 1)[0].split("#", 1)[0]
        if path == "/favicon.ico":
            # Answered rather than 404'd so the console stays empty and a real
            # error is the only thing in it.
            self.send_response(204)
            self.end_headers()
            return
        if path == "/api/payload":
            # Re-read on every request: memories are written while this is open.
            self._json(200, build_payload())
            return
        if path == "/out.html":
            # The same bytes `-o` writes, reachable without stopping the
            # server. Inline by default so it can just be looked at;
            # ?download=1 when the point is to keep the file.
            attach = "download=1" in (self.path.split("?", 1)[1:] or [""])[0]
            self._html(page(build_payload()),
                       filename="out.html" if attach else None)
            return
        if path not in ("/", "/index.html"):
            self.send_error(404)
            return
        # The shell alone. The corpus arrives from /api/payload, so a reload
        # here costs ~25kB instead of the ~1.9MB the two used to travel as one.
        self._html(page())

    def _html(self, text: str, filename: str | None = None) -> None:
        b = text.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(b)))
        # Never cached: --serve exists to show what is on disk right now.
        self.send_header("Cache-Control", "no-store")
        if filename:
            self.send_header("Content-Disposition", f'attachment; filename="{filename}"')
        self.end_headers()
        self.wfile.write(b)

    def _json(self, code: int, obj: dict) -> None:
        b = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(b)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(b)

    def do_POST(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path != "/send":
            self.send_error(404)
            return
        if not SEND_ENABLED:
            self._json(403, {"ok": False, "err": "送信は loopback bind のときだけ有効です"})
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
            req = json.loads(self.rfile.read(n) or b"{}")
        except (ValueError, json.JSONDecodeError) as e:
            self._json(400, {"ok": False, "err": f"リクエストを解釈できません: {e}"})
            return

        comment = str(req.get("comment") or "").strip()
        if not comment:
            self._json(400, {"ok": False, "err": "コメントが空です"})
            return

        # The topic must be one just discovered. Re-derived here rather than
        # trusted from the page, so a stale tab cannot publish to a task that
        # has since ended, and nothing can name an arbitrary topic.
        avail = send_targets()
        topic = str(req.get("topic") or "")
        if topic not in {t["topic"] for t in avail["targets"]}:
            self._json(400, {"ok": False,
                             "err": f"{topic or '(宛先なし)'} は今の生きている宛先にありません"})
            return

        mem, ix = find_memory(str(req.get("project") or ""), str(req.get("area") or ""),
                              str(req.get("file") or ""))
        if mem is None:
            self._json(404, {"ok": False, "err": "そのメモが見つかりません"})
            return

        text = compose(mem, ix, comment, bool(req.get("meta")), bool(req.get("body")))
        # --data - reads the body from stdin. The trailing-words form would
        # let a body that starts with a dash be re-read as a flag, and the
        # verb's own notes call that out.
        n = len(text.encode("utf-8"))
        ok, out, err = _cli(["agent", "send", "--topic", topic, "--data", "-"], stdin=text)
        warn = ""
        if ok and n > HOOK_INLINE_LIMIT:
            warn = (f"{n}B は inline 上限 {HOOK_INLINE_LIMIT}B 超 — 届いてはいるが相手の"
                    f"起床文脈には入らない。相手が `harness-cli agent read <seq>` を"
                    f"走らせるまで読まれません")
        self._json(200 if ok else 502,
                   {"ok": ok, "out": (out or "").strip(), "err": (err or "").strip(),
                    "bytes": n, "warn": warn})

    def log_message(self, *a) -> None:  # quiet
        pass


def holder_of(port: int) -> str:
    """ ' (pid N: cmdline)' for whoever is listening, or '' if we cannot tell.

    Read out of /proc rather than shelling to ss or lsof: this runs on the
    error path, where a missing tool would replace the diagnosis with a second
    failure.
    """
    try:
        want = f"{port:04X}"
        inodes = set()
        for tcp in ("/proc/net/tcp", "/proc/net/tcp6"):
            try:
                for line in Path(tcp).read_text().splitlines()[1:]:
                    f = line.split()
                    # st 0A == LISTEN
                    if len(f) > 9 and f[1].split(":")[1] == want and f[3] == "0A":
                        inodes.add(f[9])
            except OSError:
                continue
        if not inodes:
            return ""
        for proc in Path("/proc").iterdir():
            if not proc.name.isdigit():
                continue
            try:
                for fd in (proc / "fd").iterdir():
                    if os.readlink(fd).strip("socket:[]") in inodes:
                        cmd = (proc / "cmdline").read_bytes().replace(b"\0", b" ").decode(errors="replace")
                        return f" (pid {proc.name}: {cmd.strip()[:70]})"
            except OSError:
                continue
    except Exception:
        pass
    return ""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--serve", action="store_true", help="run a local server that re-reads on each request")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--host", default="127.0.0.1", help="loopback by default: this page contains your notes")
    ap.add_argument("--check", action="store_true", help="print the maintenance findings and exit")
    ap.add_argument("-o", "--out", default="out.html")
    args = ap.parse_args()

    if args.check:
        return print_check()

    if args.serve:
        # Sending is enabled ONLY on a loopback bind. Off loopback the page is
        # reachable by other machines, and a form that publishes to the
        # agentboard is not something to hand out with the page.
        global SEND_ENABLED
        SEND_ENABLED = args.host in ("127.0.0.1", "::1", "localhost")
        try:
            srv = ThreadingHTTPServer((args.host, args.port), Handler)
        except OSError as e:
            if e.errno != errno.EADDRINUSE:
                raise
            # "address already in use" without naming the holder sends you to
            # ss/lsof before you can do anything. This host runs more than one
            # little loopback server, so which one it is decides whether you
            # kill it or pick another port.
            print(f"{args.host}:{args.port} は使用中です{holder_of(args.port)}", file=sys.stderr)
            print("  別のポートで:  --port 0  (空きを自動で選ぶ)", file=sys.stderr)
            return 1
        # --port 0 asks the kernel for a free one, so report what it gave.
        port = srv.server_address[1]
        print(f"http://{args.host}:{port}  (Ctrl-C to stop)", file=sys.stderr)
        if SEND_ENABLED:
            t = send_targets()
            print("  agent send: " + (f"宛先 {len(t['targets'])} 件" if t["ok"]
                                      else f"使えません — {t['reason']}"), file=sys.stderr)
        else:
            print(f"  agent send: 無効 (loopback bind のときだけ有効; --host {args.host})",
                  file=sys.stderr)
        try:
            srv.serve_forever()
        except KeyboardInterrupt:
            pass
        return 0

    out = Path(args.out)
    out.write_text(page(build_payload()), encoding="utf-8")
    print(f"{out}  ({out.stat().st_size/1024/1024:.1f} MB)", file=sys.stderr)
    print("this file embeds every memory body — it is your notes, not a report", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
