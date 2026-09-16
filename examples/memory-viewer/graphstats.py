#!/usr/bin/env python3
"""Shape of the [[link]] graph in the memory corpus, per project.

    python3 graphstats.py            # every table
    python3 graphstats.py --areas    # only the per-area breakdown

Exists because the 2026-09-16 map design rests on six tables of measurements,
and a number in a document that cannot be re-derived is not evidence. Run this
to reproduce them; run it again later to see what has changed.

Reads the same corpus memviewer.py does, through its build_payload(), so the
node and edge sets are the ones the viewer draws — not a second parser that
could drift from it.
"""

from __future__ import annotations

import collections
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import memviewer as mv  # noqa: E402


def graph(project: dict):
    """(nodes, adjacency, directed-out) for one project.

    Resolution matches the viewer's: a [[target]] hits a memory by `name` first
    and by filename stem second, within this project only — links never cross
    projects.
    """
    mems = project["memories"]
    names = {m["name"] for m in mems}
    stems = {m["file"][:-3]: m["name"] for m in mems}

    def resolve(t: str):
        t = t[:-3] if t.endswith(".md") else t
        return t if t in names else stems.get(t)

    out = {m["name"]: set() for m in mems}
    adj = {m["name"]: set() for m in mems}
    for m in mems:
        for raw in m["links"]:
            tgt = resolve(raw)
            if not tgt or tgt == m["name"]:
                continue
            out[m["name"]].add(tgt)
            adj[m["name"]].add(tgt)
            adj[tgt].add(m["name"])
    return mems, adj, out


def components(nodes, adj) -> list[int]:
    seen, sizes = set(), []
    for n in nodes:
        if n in seen:
            continue
        stack, size = [n], 0
        while stack:
            x = stack.pop()
            if x in seen:
                continue
            seen.add(x)
            size += 1
            stack.extend(y for y in adj[x] if y not in seen)
        sizes.append(size)
    return sorted(sizes, reverse=True)


def label_propagation(nodes, adj, rounds: int = 60) -> dict:
    """Deterministic: fixed node order, ties broken by label. Same corpus, same
    partition — a randomised one would make the modularity below unquotable."""
    order = sorted(nodes)
    lab = {n: n for n in order}
    for _ in range(rounds):
        changed = False
        for n in order:
            if not adj[n]:
                continue
            c = collections.Counter(lab[x] for x in adj[n])
            best = max(sorted(c.items()), key=lambda kv: (kv[1], kv[0]))[0]
            if lab[n] != best:
                lab[n] = best
                changed = True
        if not changed:
            break
    return lab


def modularity(adj, lab) -> float:
    m2 = sum(len(v) for v in adj.values())
    if not m2:
        return 0.0
    total = 0.0
    for i in adj:
        for j in adj:
            if lab[i] != lab[j]:
                continue
            total += (1.0 if j in adj[i] else 0.0) - len(adj[i]) * len(adj[j]) / m2
    return total / m2


def undirected_pairs(out) -> set:
    return {tuple(sorted((a, b))) for a, tos in out.items() for b in tos}


def big_projects(payload, minimum: int):
    for p in payload["projects"]:
        if p.get("cross") or len(p["memories"]) < minimum:
            continue
        yield p


def table_shape(payload) -> None:
    print("## graph shape")
    print(f"{'project':34} {'nodes':>5} {'edges':>5} {'isolated':>8} {'comps':>5} "
          f"{'largest':>7} {'maxdeg':>6} {'meddeg':>6}")
    for p in big_projects(payload, 5):
        mems, adj, out = graph(p)
        names = [m["name"] for m in mems]
        deg = sorted((len(adj[n]) for n in names), reverse=True)
        comps = components(names, adj)
        assert sum(comps) == len(names), "components must partition the nodes"
        print(f"{p['label']:34} {len(names):5} {len(undirected_pairs(out)):5} "
              f"{sum(1 for n in names if not adj[n]):8} {len(comps):5} {comps[0]:7} "
              f"{deg[0]:6} {deg[len(deg) // 2]:6}")
    print()


def table_direction(payload) -> None:
    print("## one-way vs mutual  (a [[link]] is directed; an edge is one or both)")
    print(f"{'project':34} {'edges':>6} {'one-way':>8} {'mutual':>7} {'mutual%':>8}")
    for p in big_projects(payload, 8):
        _, _, out = graph(p)
        pairs = undirected_pairs(out)
        mutual = sum(1 for a, b in pairs if b in out[a] and a in out[b])
        pct = mutual * 100 // max(len(pairs), 1)
        print(f"{p['label']:34} {len(pairs):6} {len(pairs) - mutual:8} {mutual:7} {pct:7}%")
    print()


def table_communities(payload) -> None:
    print("## communities  (label propagation; Q below ~0.3 = no community structure)")
    print(f"{'project':34} {'comms':>5} {'Q':>7}  sizes")
    for p in big_projects(payload, 15):
        mems, adj, _ = graph(p)
        names = [m["name"] for m in mems]
        lab = label_propagation(names, adj)
        sizes = collections.Counter(lab.values())
        top = ", ".join(str(c) for _, c in sizes.most_common(6))
        print(f"{p['label']:34} {len(sizes):5} {modularity(adj, lab):7.3f}  {top}")
    print()


def table_areas(payload) -> None:
    print("## induced subgraph per area  (only projects that use subdirectories)")
    for p in big_projects(payload, 5):
        mems, _, out = graph(p)
        area = {m["name"]: (m["area"] or "(top)") for m in mems}
        if len(set(area.values())) < 2:
            continue
        pairs = undirected_pairs(out)
        within = sum(1 for a, b in pairs if area[a] == area[b])
        print(f"\n{p['label']}:  {len(pairs)} edges, {within} within an area "
              f"({within * 100 // max(len(pairs), 1)}%)")
        print(f"  {'area':10} {'nodes':>5} {'internal':>8} {'isolated':>8} {'comps':>5} {'largest':>7}")
        for a in sorted(set(area.values())):
            ns = [n for n in area if area[n] == a]
            es = [(x, y) for x, y in pairs if area[x] == a and area[y] == a]
            sub = {n: set() for n in ns}
            for x, y in es:
                sub[x].add(y)
                sub[y].add(x)
            comps = components(ns, sub)
            print(f"  {a:10} {len(ns):5} {len(es):8} "
                  f"{sum(1 for n in ns if not sub[n]):8} {len(comps):5} {comps[0]:7}")
    print()


def main(argv: list[str]) -> int:
    payload = mv.build_payload()
    only_areas = "--areas" in argv
    if only_areas:
        table_areas(payload)
        return 0
    table_shape(payload)
    table_direction(payload)
    table_communities(payload)
    table_areas(payload)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
