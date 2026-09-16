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
(which unfiled memory belongs in which subsystem directory) and that framing was
wrong twice over — it was measured and found unsupported, and it was never the
requester's goal.

## What was measured before designing

Everything below is from the live corpus on 2026-09-16, and several numbers
changed the design.

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

**Only harness has areas, and most of it is unfiled.** `(top)` holds 100 of its
168; the nine subsystem directories hold 68 between them. Every other project
keeps everything at the top level. 56% of harness's edges are within one area
and the cross-area ones are almost all `(top)`↔something.

**The filing signal that an earlier draft was built on does not exist.** Of the
top-level memories with at least two links and at least one filed neighbour (56
of them), the strongest pull toward a single area is 3 links out of 8. Everything
else is two. The cause is structural: with 100 unfiled, most of anyone's
neighbours are also unfiled, so filed neighbours are a minority everywhere. A map
drawing that as an attraction would invite reading two links as a verdict.

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
   to pin harness apart by, and the other projects have one area.
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
- 390px: the page itself must not overflow horizontally — the map scrolls and
  zooms inside its own box, the way the ego graph already does.

## Implementation note

memviewer holds its CSS and JS as module-level constants, so a running
`--serve` keeps serving the old page after an edit. It has to be restarted to
see a change; this cost one wrong measurement while building the cross-project
view.
