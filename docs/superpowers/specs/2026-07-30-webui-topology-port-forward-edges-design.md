# WebUI topology: draw port forwards as edges

**Date:** 2026-07-30
**Status:** Approved (brainstorming)

## Problem

The WebUI now lists port forwards (`2026-07-30-port-forward-registry-design.md`),
but the list is text: `#3  -L  5ba8f743…  127.0.0.1:18099 -> 127.0.0.1:18500  cli
ws:127.0.0.1:43164-27030`. Reading which machine is forwarding into which means
correlating a connection id by eye against the connection list beside it.

Verbatim: *"なんとなくさこうwebuiのトポロジー図のところにここからここにポートフォー
ワードしてるっての出せたら面白そうよね"*

The topology diagram already draws every connection and every task. A forward is
a relationship between two things it already draws, so the relationship can be
shown instead of spelled out.

## What already exists

`renderConnTopology` (`webui/static/main.js:3840`) builds an SVG radial layout
from scratch on each snapshot poll:

- server at the centre; **cluster ring** R1=95 (one node per remote IP, from
  `groupConnsByIP`); **connection leaves** starting at R2=165 (one node per
  connection); tasks outside that. `W=640 H=560`, `SERVER_R=22`, `CLUSTER_R=14`,
  `LEAF_R=8`.
- Leaves are **not on a single circle**: each cluster fans its leaves inside its
  own angular slot and spills onto concentric tiers once one arc would pack too
  tightly — `r = R2 + tier * (LEAF_R * 2.4)`. So two leaves can sit at different
  radii, and an edge between them is not a chord of one circle.
- Its stated invariant: *"hierarchy radiates strictly OUTWARD — server → cluster
  ring → connection leaves → tasks. Each level is further from the centre, so
  depth reads as distance."*
- Clusters are sorted by IP so a given IP keeps its angular slot across polls —
  the server's snapshot order is non-deterministic because it ranges a Go map.
- Node groups are keyed by cid for the class-based add/remove animation.
- **Three kinds of edge are drawn, all radial**: server→cluster (`.ct-spoke`,
  `#333`), cluster→leaf (`.ct-leaf-spoke`, `#2a2a2a`), and leaf→task
  (`.ct-task-spoke`, `#6b5836`) terminating at a `.ct-task-node` rect. So a
  connection leaf that owns tasks already has task spokes and rects crowding it —
  which is where a forward curve will terminate.
- On `max-width: 600px` it returns early and draws nothing.

## Decisions

1. **The external endpoint is not a node.** `127.0.0.1:18500` and friends get no
   circle; the address appears only in the edge label. They are arbitrary
   addresses the harness knows nothing else about, and giving them nodes would
   add a ring the layout has no room for.
2. **Always drawn, no toggle.** With no forwards registered there are no edges,
   so the feature is invisible when unused.
3. **The task end terminates on the task's rect, not on the runner's leaf.** In a
   dense cluster the renderer suppresses per-leaf labels — *"suppressed for dense
   clusters, where per-leaf labels would overlap; role is still conveyed by colour
   + legend"* — and the real fleet has one host carrying 13 runner leaves in a
   single fan, all orange, all unlabelled. An edge landing among them would say
   "some runner on this host". The rect is unique per task, carries the repo name,
   and is the entity the forward is actually bound to.
4. **One curve per forward, bowed clear of the centre** — see Rendering. Rejected
   alternatives:
   - *Light up the four existing radial edges the traffic really traverses*
     (client leaf → its cluster → server → runner's cluster → runner leaf).
     Topologically honest and needs no new geometry, but the two server-adjacent
     spokes are shared with every other connection in those clusters, so with
     more than one forward it stops saying which is which.
   - *An arc riding outside the leaf ring.* Cannot collide with the centre by
     construction, but it invades the band the code already calls crowded:
     *"taller so the outermost ring (dense cluster's tier-2 leaves + their tasks
     + labels) fits with margin"*. The chosen curve is therefore routed on the
     **inside** — see the annulus rule in Rendering, which is what keeps the two
     options actually distinct.
5. **The edge is coloured by the listening side's role**, not by a hue reserved
   for forwards, and is distinguished from the hierarchy edges by weight and dash
   instead. This was chosen over a fixed unowned hue (pink `#f07ab0`) because it
   makes the colour carry the direction: `cli→runner` is cli-coloured,
   `runner→tui` is runner-coloured. See Rendering for the trade it accepts.
6. **Read-only.** No kill from the diagram; the panel button and the `forward
   kill` verb already cover that.
7. **Desktop only**, inherited: the renderer already no-ops under 600px. The
   list panel is the mobile surface. Not a scoping choice — the diagram does not
   exist on mobile.

## The join

The two endpoints are the **client's connection leaf** and the **task's rect**.
Both are already drawn, and both are already keyed by exactly the identifier the
forward carries:

```
forward.origin_cid  ──►  the leaf drawn for conns[].cid          (client end)
forward.task        ──►  the rect drawn with data-task=<task id>  (task end)
```

Verified against the running code and live data, not inferred:

- The renderer attaches a task rect to a leaf with
  `tasks.filter(tk => isActiveTask(tk) && tk.assignedTo === conn.cid)`, and
  `isActiveTask` is `status === "Running" || status === "Detached"`
  (`webui/static/main.js:3805`). Those are **precisely** the statuses the server
  requires to register a forward (`handleRegisterPortForward` gates on
  `Running || Detached`), so a registered forward's task rect is always on screen.
  No fallback endpoint is needed — the two sets coincide by construction.
- Each rect is emitted as `<g class="ct-task-node" data-task="<task id>">`, so it
  is individually addressable by the same hex id `forward.task` carries.
- Rect geometry, for the terminal approach: `tr = r + 24 + (ti % 2) * 12` — 24 to
  36px radially outward from its leaf — fanned to its own angle within the leaf's
  slot.

**`tasks[].assignedTo` is not needed.** An earlier draft routed the task end
through `assignedTo → conns[].cid, role=runner` to find the *runner's leaf*. That
join does hold — a runner's registry id is its connection id string
(`server/runner_handler.go:77`: `runnerID := conn.ConnectionID().String()`), the
wasm bridge publishes it in that form (`cmd/harness-webui-wasm/main.go:491`), and
it measured byte-identical live. It is also the join the renderer itself uses to
place the rect. But terminating on the rect makes it redundant: the rect is keyed
by task id directly. Recorded here because it is the reason the rect is where it
is, not because this feature performs it.

`RunnerInfo.id` (a typed `RunnerID`) is not used either. A zero-value `RunnerID`
trips an `IpAddrLen` assertion in the encoder, and nothing here needs to go near
it.

### One payload change is required

`harnessSnapshot` currently publishes the origin as a single **display** string
(`cmd/harness-webui-wasm/main.go:545`: `"origin": cli.PortForwardOrigin(fi)`),
which renders as `"cli ws:127.0.0.1:48668-17690"` — kind and cid concatenated.
Joining on that would mean splitting a display string on a space, i.e. depending
on a formatting convention.

So the forward payload gains `"origin_cid": string(fi.OriginCid)` alongside the
existing `origin`. The wire already carries the field; this is only the JS-facing
object. `origin` stays as-is for the panel's display column.

### Implementation note

The renderer computes leaf coordinates (`lx, ly`) and rect coordinates
(`tx, ty`) inside its cluster/leaf/task loops and does not keep them. Drawing
forward edges needs both after those loops have run, so the loops must record
`cid → {x, y}` and `taskId → {x, y}` into two maps, and the forward edges are
appended in a second pass. Appending them last also puts them above the spokes in
SVG paint order, which is what we want.

## Rendering

- **One quadratic Bézier per forward**, from the client leaf to the **task rect**,
  with its bulk routed inside the leaf ring. The control point is the midpoint of
  the two endpoints displaced along the perpendicular to the chord, and the
  governing rule is an annulus, not a direction:

  > the curve's closest approach to the centre must exceed `SERVER_R + 12 = 34px`,
  > and its bulk must stay within `R2 = 165` — only the terminal approach to the
  > task rect crosses outward past the leaf ring.

  "Displace away from the centre" would be wrong as a rule. For two leaves in
  nearly opposite clusters the midpoint sits near the centre, so outward is fine
  and the constraint that bites is the server node. But for two leaves in
  *adjacent* clusters the midpoint is already out near `R2`, and displacing
  outward pushes the curve past the leaf ring into the crowded outer band — the
  exact thing the rejected arc option was rejected for. There, the displacement
  must go **inward**, toward the centre, where there is room.
- **Crossing spokes is acceptable; crossing task content is not.** The interior
  annulus is not empty — server→cluster and cluster→leaf spokes run through it —
  but those are 1px near-background greys (`#333`, `#2a2a2a`), so a 1.5px pink
  dashed curve over them stays legible and occludes nothing meaningful. The outer
  band instead holds `.ct-task-node` rects and `.ct-task-label` text, which a
  curve would obscure. That asymmetry is the whole reason for routing inside.
- **A single arrowhead, pointing listener → dialer.** For `-L` that is client →
  task: something connects to the client's listening port and the runner dials the
  target on that task's behalf, so the head sits on the rect. For `-R` it is task →
  client: the runner listens and the client dials, so the head sits on the client
  leaf and the tail on the rect. The arrow reverses between the two directions
  because the relationship does; no second decoration is needed.
- **Label at the curve's midpoint**: `<dir> <bind_port> → <target_host>:<target_port>`,
  e.g. `-L 18099 → 127.0.0.1:18500`. This is the only place the external endpoint
  appears.
- **The edge takes the role colour of the side that ACCEPTS connections**, which
  is the listener. This is defined by role, not by which end the curve happens to
  touch — the task end terminates on a task rect (amber `#d9a441`), and that must
  not colour the edge, because a task is not a role.
  - `-L`: the **client** accepts, so the edge takes the owning client's role hue —
    cli `#5aabf7`, tui `#f7d05a`, webui `#2d5`, in-task agent `#c080f0`.
  - `-R`: the **runner** accepts, so every `-R` edge is runner orange `#f0a060`,
    whichever client owns it.
  Consequence, intended: the hue alone says which way the forward is accepted, and
  the arrowhead becomes reinforcement rather than the only cue.
- **For `-L`, the role is read from `conns[].role` of the client end** — the same
  datum that end's own `role-<x>` class is built from, so the edge colour cannot
  drift from the leaf it touches. No `origin_kind` payload field is needed, only
  `origin_cid` (see The join). **For `-R` the hue is the constant runner orange**;
  there is nothing to look up, since the accepting side is a runner by definition.
- **Distinction from the hierarchy is by weight and dash, not hue.** New CSS class
  `.ct-forward`: `stroke-dasharray: 6 3; stroke-width: 2.5; fill: none`, with the
  stroke set per-role (`.ct-forward.from-cli { stroke: #5aabf7 }` and so on).
  - **2.5** is thicker than every stroke already in the diagram: spokes `1`
    (`.ct-spoke`, `.ct-leaf-spoke`), conn and cluster nodes `1.5`
    (`webui/static/style.css:994, 1012`), server node `2`.
  - **`6 3`** must differ from **both** existing dash patterns, not one: `3 2`
    means "unidentified connection" on nodes (`style.css:1023`) and `2 2` is the
    task spoke (`style.css:1062`) — which the forward edge now runs alongside for
    its last 30px, making that one the important collision to avoid. At 2.5px
    against the task spoke's 1px the two also differ in weight.
  - Zoom is applied by rewriting the SVG viewBox (`attachTopoZoom`), so strokes
    and dashes scale with it and these relative distinctions survive any zoom
    level.
  - **Residual risk, accepted**: an edge now shares its hue with the node it
    leaves, which is exactly what a fixed unowned hue would have avoided. The
    separation rests entirely on being 2.5px and dashed against a 1.5px solid
    circle outline. If that turns out too subtle in the browser, the fallback is
    to keep the role hue and darken or lighten it by a fixed amount rather than
    reverting to one colour for all forwards — the direction-carrying hue is the
    point of this choice.
  - **Unexpected or unidentified role**: `role-unspecified` is `#777`, which would
    render an edge indistinguishable from a spoke. It cannot occur for a forward
    endpoint — registration requires an authenticated client, and the far end is
    always `role=runner` — but `.ct-forward` carries a default stroke of `#d4d4d4`
    so an unforeseen role renders visibly rather than vanishing.
- **The displacement arithmetic is not obvious.** For a quadratic Bézier,
  `B(0.5) = ¼P₀ + ½C + ¼P₂`, so the curve's midpoint sits only **half way** from
  the chord's midpoint to the control point. To move the curve `d` px off the
  chord, displace the control point by `2d`. So clearing the server node in the
  near-opposite case (`d = 34`) needs a `68px` control-point displacement;
  displacing by 34px would leave the curve crossing the node.
- **Several forwards between the same pair fan out**: add `20px` to the
  perpendicular displacement per additional forward already drawn between the
  same ordered endpoint pair, so two forwards between the same two leaves are two
  visibly separate curves rather than one thick one. The fan is subject to the
  same annulus rule — additional offsets stop when the next curve would leave it,
  and further forwards on that pair share the outermost admissible curve rather
  than escaping the ring.
- **Two tasks on one runner sharing a repo get identical rect labels.** They are
  still separate rects at separate fanned angles, so which one an edge lands on is
  unambiguous positionally even when the text is not. No extra labelling.
- Curves are rebuilt with the rest of the SVG on each poll. They carry no
  animation state of their own — the existing keyed-node animation is for nodes.

## Failure modes

- **An endpoint is missing from the drawn diagram** (the forward is registered but
  its client connection is already gone, or no rect was drawn for its task): draw no curve for that forward. It still appears in the list panel,
  so nothing is hidden — the diagram just declines to draw an edge it cannot
  place. This is a real race, not a hypothetical: a registration outlives its
  connection until the server tears the connection down.
- **Both endpoints resolve to the same leaf.** A client and a runner are always
  separate connections, so this does not arise today; guard anyway and skip the
  curve, because a zero-length Bézier with a perpendicular control point renders
  as a stray spike rather than as nothing.
- **Mobile**: nothing to handle. The renderer returns before any of this.

## Testing

The topology renderer has no unit tests today and this adds no Go logic beyond
one payload field, so verification is browser-driven, as it was for the panel:

- With a dummy harness, register a `-L` from the CLI and confirm a dashed curve
  appears between the CLI's leaf and that task's rect, arrowhead pointing at the
  rect, coloured cli blue, labelled with the bind port and target.
- Register a `-R` and confirm the arrowhead points the other way (at the client
  leaf) and the edge is runner orange rather than the owning client's hue.
- Register two forwards between the same pair and confirm two distinct curves.
- Kill one and confirm its curve disappears on the next poll.
- Confirm no curve crosses the server node: check with clusters at opposite
  angles, which is the worst case for a straight chord.
- Confirm on a dense cluster — the real fleet's 13-runner host is the case this
  design exists for — that the edge visibly lands on one rect and not merely
  "somewhere in the fan".
- Confirm the page still renders with `forwards` present but a matching
  connection absent (kill a forward's owning client with SIGKILL and look during
  the window before the server reaps the connection).
- Confirm mobile (390px) is unchanged.

## Out of scope

- Killing a forward from the diagram.
- Nodes for external endpoints.
- Showing per-forward traffic volume or connection counts on the edge.
- Any mobile topology rendering.
- Animating an edge on add/remove.
