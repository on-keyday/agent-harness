# memviewer: a whole-project link map

**Date:** 2026-09-16
**Status:** design, awaiting implementation plan

## Problem

memviewer shows the link graph one memory at a time. `87701a4c` added an ego
graph to the detail pane — this memory, what it points at, what points at it —
and that answers "what is this wired to". Nothing shows the whole graph.

Verbatim: *"グラフで見ると特徴が見える可能性があるからいろんな見方で見たい"*.

That is an exploratory motive, and it is worth naming as such: the value is the
chance of noticing something, which cannot be specified in advance. An earlier
framing of this design tried to justify the map by a decision it would serve
(which top-level memory belongs in which subsystem directory) and that framing
was wrong three times over: it was measured and found unsupported, it was never
the requester's goal, and it misread the placement rule — see below.

## What was measured before designing

Everything below is from the live corpus on 2026-09-16, and several numbers
changed the design.

**Re-derive it with `examples/memory-viewer/graphstats.py`** (`--areas` for the
last table alone). It reads the corpus through memviewer's own `build_payload()`,
so the nodes and edges are the ones the viewer draws rather than a second
parser's. Every table below is that script's output: a number in a design
document that cannot be re-run is not evidence, and these were inline one-liners
that existed only in a transcript until the omission was caught.

**The graphs are small, and only one of them is hard.**

| project | nodes | edges | isolated | components | largest | max deg | median deg |
|---|---:|---:|---:|---:|---:|---:|---:|
| remote-agent-harness | 168 | 373 | 6 | 7 | 162 | 24 | 4 |
| sisaku-security-entrepreneurship | 59 | 61 | 14 | 18 | 31 | 7 | 2 |
| sisaku-security-rule-review | 37 | 65 | 0 | 1 | 37 | 14 | 3 |
| kcdn-ansible-ksdk | 34 | 62 | 0 | 1 | 34 | 16 | 3 |
| sdplane-sdplane-dev | 15 | 22 | 0 | 1 | 15 | 9 | 3 |
| three others | ≤9 | ≤12 | 0 | 1–2 | ≤9 | ≤6 | 2 |

Seven of eight are small enough to draw without any cleverness. The design
question is entirely about remote-agent-harness and its 162-node component.

**Only harness uses subdirectories at all.** `(top)` holds 100 of its 168; the
nine subsystem directories hold 68 between them. Every other project keeps
everything at the top level. 56% of harness's edges are within one area and the
cross-area ones are almost all `(top)`↔something.

**`(top)` is not a backlog.** What belongs in MEMORY.md is decided by how likely
a memory is to fire, and a memory moves down to a subsystem only when it is
needed *while touching that subsystem and not otherwise* — the index says so in
its own first line, and `feedback_demote_by_trigger_kind` says it again. So the
100 at the top are there because they fire broadly, not because nobody has got
round to them. An earlier draft of this design read them as unfiled work and
proposed a map to help clear it; that was wrong about the corpus before it was
wrong about anything else.

**And the signal that draft rested on is not in the data either.** Of the
top-level memories with at least two links and at least one neighbour in a
subsystem (56 of them), the strongest pull toward a single area is 3 links out of
8. Everything else is two. The cause is structural: most of anyone's neighbours
are also at the top, so subsystem neighbours are a minority everywhere. A map
drawing that as an attraction would invite reading two links as a verdict — and
would be recommending a move the placement rule does not ask for.

**harness has no community structure to show.** Deterministic label propagation
puts 147 of 168 in one community (the rest: one of 15 and six singletons), at
modularity Q = 0.085 — below any threshold at which communities are considered
present. Colouring by community would paint 87% of the map one colour.

| project | communities | sizes | Q |
|---|---:|---|---:|
| remote-agent-harness | 8 | 147, 15, 1×6 | 0.085 |
| sisaku-security-entrepreneurship | 21 | 12, 11, 8, 5, 4, 3, 2… | 0.660 |
| sisaku-security-rule-review | 4 | 25, 5, 4, 3 | 0.236 |
| kcdn-ansible-ksdk | 2 | 29, 5 | 0.107 |
| sdplane-sdplane-dev | 1 | 15 | 0.000 |

This is itself the kind of thing the map was wanted for, and it is worth stating
plainly because it bounds what the map can ever show: **harness's memories are
linked as a fairly uniform mesh, not as topics.** It is not that the declared
areas disagree with the observed communities — there are no observed communities
to disagree with. Any map of harness will therefore show a blob and a halo, and
that is the corpus, not the drawing.

**Reciprocal links are rare, so drawing direction everywhere would say nothing.**
A `[[link]]` is directed, and an edge is either one-way or mutual:

| project | edges | one-way | mutual | mutual % |
|---|---:|---:|---:|---:|
| remote-agent-harness | 373 | 354 | 19 | 5% |
| sisaku-security-entrepreneurship | 61 | 49 | 12 | 19% |
| sisaku-security-rule-review | 65 | 61 | 4 | 6% |
| kcdn-ansible-ksdk | 62 | 52 | 10 | 16% |

Arrowheads on all 373 would be 354 marks in the dense core restating what is
already known — almost everything is one-way. The 5% that is not is the part
carrying information.

**The subsystem areas are not internally connected, and that is where the "56%
within-area" figure came from.** Induced subgraph of each area in harness:

| area | nodes | internal edges | isolated | components | largest |
|---|---:|---:|---:|---:|---:|
| (top) | 100 | 177 | 5 | 6 | 95 |
| history | 9 | 1 | 7 | 8 | 2 |
| host | 10 | 6 | 2 | 4 | 6 |
| pty | 12 | 6 | 6 | 7 | 6 |
| reference | 2 | 1 | 0 | 1 | 2 |
| tools | 7 | 2 | 3 | 5 | 2 |
| trsf | 10 | 7 | 3 | 5 | 5 |
| user | 5 | 0 | 5 | 5 | 1 |
| webui | 7 | 5 | 2 | 3 | 5 |
| windows | 6 | 4 | 2 | 3 | 4 |

177 of the 209 within-area edges are `(top)` linked to itself; the nine
subsystem directories contribute 32 between them, and `user` contributes none at
all. So the earlier reading — "over half the links stay inside an area" — was
the top level talking to itself, not evidence that placement follows the links.
It does not, and it is not meant to: memories go down when their trigger is a
location, so the grouping is by when a memory fires, and the link structure has
no reason to reproduce that.

**A throwaway spike answered the readability question.** Force-directed layout
over the real data, with zoom:

- At fit zoom (0.43×) with labels on, the labels are noise.
- At fit zoom with labels **off**, the shape reads: one dense core, a scattered
  halo of weakly-connected nodes, and the isolated ones sitting alone far out.
- At 2.6× the labels, node sizes and colours are all legible and the graph is
  navigable.

So zoom is not a convenience here. It is what makes the view usable at all, and
evaluating the layout without it would have reached the opposite conclusion.

**Layout cost is not a constraint.** Measured in the browser, 400 iterations of
an O(n²) repulsion pass: 94.4 ms for harness (168 nodes / 373 edges), 10.5 ms at
59 nodes, under 5 ms below that. Running it twice on the same input produced
byte-identical coordinates.

## Decisions

1. **One layout, force-directed.** Not a switcher. The ego graph needed four
   rounds of measurement to stop its labels colliding at 24 nodes; three layouts
   would be three times that work, and the exploratory motive is served more
   cheaply by varying what the marks *mean* than by varying where they sit.
2. **The seed is fixed and the layout is deterministic.** This is a tool you come
   back to. If the picture moved between visits you could not tell a change in
   the notes from a change in the layout. Verified: identical coordinates across
   runs.
3. **Computed once when a project's map is opened, not animated.** At 94 ms there
   is nothing to stream, and a settling animation would only make decision 2
   harder to keep.
4. **The channels carry the different viewpoints.** Colour by area / type / age /
   inbound count; size by inbound count / file size / flat.
5. **Labels on/off is a mode, not a nicety.** Measured above: it is the difference
   between the overview reading as a shape and reading as noise. It is a
   first-class control, and the overview default is off.
6. **No community detection and no community colouring.** Measured: dead on the
   one project that most needs the map. Reporting modularity as a number may be
   worth doing, but it belongs to `--check` and the 要保守 panel, not here.
7. **No area-pinned or cluster-pinned layout.** Same measurement: there is nothing
   to pin harness apart by, and the other projects have one area. This is about
   POSITION and is not the area filter in decision 14 — narrowing which nodes are
   drawn is a different act from forcing where they sit, and only the latter
   would be asserting a grouping the links do not support.
8. **Zoom and pan are required.** Wheel zooms about the cursor, drag pans, and a
   button fits the whole graph. Stated as a requirement by the operator and
   confirmed necessary by the spike.
9. **A node click closes the map and selects that memory in the detail pane.**
   The map is a navigation surface over the corpus already on screen, not a
   second application. Closing is not incidental — decision 10 gives the map the
   whole width, so the detail pane does not exist while the map is open and a
   click that only selected would look like nothing happened. This is also how
   the map reaches the ego graph: pick a region, land on a memory, read its
   neighbourhood. Re-opening the map restores it at the same camera, because
   walking out to a memory and back is the expected loop.
10. **The map takes the full width, not the detail pane.** The panes are
    `minmax(20rem,26rem) 1fr`; half a screen is not enough for 162 nodes, and on
    a phone the panes stack anyway. Toggled from the header, replacing the two
    panes while it is open. It draws **the project currently chosen in the
    header** — the dropdown stays where it is and keeps meaning what it means,
    so there is no second place to pick a project.
11. **Not offered in the cross-project view.** Links never cross projects, so a
    union map is fifteen disconnected islands whose largest is the one already
    reachable by picking that project.
12. **Edges are drawn undirected, and only the mutual ones are styled apart.**
    This decision existed only in the spike's code until it was questioned;
    writing it down is the point of this entry. Measured above: 95% of harness's
    edges are one-way, so arrowheads on everything would put 354 marks in the
    core to restate a thing that is true of nearly all of them. The 19 reciprocal
    pairs get their own stroke. Direction as such is already carried, better,
    by the ego graph, which encodes it as position — and a node click leads
    there.
13. **Direction appears on hover, scoped to one node.** Hovering a node colours
    the edges leaving it differently from the edges arriving at it. This is the
    only shape in which "show one direction" means anything: globally every edge
    is somebody's outbound and somebody else's inbound, so a global direction
    filter selects the whole set. Bounded by degree — at worst 24 edges light up.
14. **The area chips filter the map, with the same meaning they have for the
    list**, plus one toggle, `隣接も含める`, default off. Measured above, the
    plain filter is worth having for `(top)` (100 nodes, 177 edges) and shows
    little more than dots for a subsystem — `user` has five nodes and no internal
    edges at all. The toggle adds the filtered nodes' direct neighbours, drawn
    dimmed, which is how you see where a subsystem's links actually go. It stays
    off by default because on `(top)` the neighbours are everything else.

## Architecture

```
build_payload()                      unchanged — links/inbound already there
        │
        ▼
mapLayout(nodes, edges, seed)         pure: positions only, no DOM
        │                             O(n²) repulsion + edge springs + weak
        ▼                             centring, 400 iterations, fixed seed
renderMap(positions, channels)        one <svg>, one camera <g>
        │
        ├── colour/size/label channels read the same node records the list does
        └── click → goto(name) → the existing detail pane and ego graph
```

Link resolution reuses what the ego graph already does: `[[target]]` is matched
against names then stems, within one project.

The layout function takes nodes and edges and returns coordinates. It touches no
DOM and no globals, so it can be exercised on its own — which matters because
memviewer has no test runner and determinism is the one property worth asserting
mechanically.

## Trade-offs

| Axis | Effect |
|---|---|
| Function | The only view of the corpus as a whole; the one place a shape, an outlier or an unexpected neighbour can show up at all |
| Security | None new. The payload is already in the page, the layout is arithmetic, nothing is fetched and nothing is written |
| Non-functional | +94 ms on opening the map for the largest project, and the map's own code in a file that is already ~1700 lines. Nothing is added to the always-loaded path: the map is computed only when opened |

## Scope / non-goals

- Multiple layouts, community detection, community colouring, area-pinned layout.
- Dragging nodes, editing, or persisting positions.
- A cross-project map.
- Animation of any kind.
- Anything that writes. memviewer is read-only apart from the one `agent send`
  panel, and the map does not touch it.

## Known limitations

- **harness will not show topics, because it has none** (Q = 0.085). Anyone
  opening the map expecting clusters there will find a mesh. This is recorded so
  that finding is read as a property of the corpus rather than a defect in the
  drawing.
- **Filtering to a subsystem area shows mostly unconnected dots**, and that is
  the corpus rather than the filter misbehaving: `user` 5 nodes / 0 internal
  edges, `history` 9 / 1, `tools` 7 / 2. Anyone opening `trsf` expecting a
  little constellation will get five connected and three alone. `隣接も含める`
  exists for exactly this case.
- At fit zoom the overview is shape-only. Reading names needs zoom, by
  construction.
- At 390px the map is a zoom-and-pan surface on a small screen. Usable, cramped,
  and not the width the view was designed for — the phone case is served by the
  ego graph, which is bounded by degree.
- The layout is force-directed, so the absolute position of anything is
  meaningless; only adjacency and grouping are. Nothing in the UI should invite
  reading an axis.

## Testing

memviewer has no test runner — it is a single stdlib-only script, and its checks
are `--check` plus the browser. So:

- **Determinism**, the one mechanical assertion: run `mapLayout` twice on the
  same input and compare coordinates exactly. Already verified in the spike for
  all eight projects.
- `--check` unchanged and still exiting on findings rather than on a crash.
- Browser, against the real corpus: the overview at fit zoom with labels off; a
  working zoom around 2–3× with labels on; each colour and size channel; wheel
  zoom about the cursor; drag pan; the fit button; a node click landing on that
  memory's detail pane and ego graph; and the map absent from the cross-project
  entry.
- Direction: the 19 mutual edges in harness are the ones drawn apart, counted
  rather than eyeballed. Hovering a node lights exactly its own edges, split
  into leaving and arriving, and nothing else changes.
- Area filter: `(top)` yields 100 nodes and 177 edges; `user` yields 5 nodes and
  none; `隣接も含める` on `trsf` brings in the neighbours its 10 members link to,
  dimmed, and the same toggle on `(top)` is expected to pull in essentially the
  whole project — checked so the behaviour is known rather than discovered.
- 390px: the page itself must not overflow horizontally — the map scrolls and
  zooms inside its own box, the way the ego graph already does.

## Implementation note

memviewer holds its CSS and JS as module-level constants, so a running
`--serve` keeps serving the old page after an edit. It has to be restarted to
see a change; this cost one wrong measurement while building the cross-project
view.
