# memory-viewer

A browser viewer and maintenance check for Claude Code's per-project
auto-memory (`~/.claude/projects/*/memory/`). One file, standard library only.
Its UI and its comments are in Japanese.

It is in this repo because it drives `harness-cli` the way an outside tool has
to: a program the harness did not start, with a UI of its own, that wants to
hand something to a live agent. The recipe it follows is
[`.claude/skills/harness-cli-from-a-tool/SKILL.md`](../../.claude/skills/harness-cli-from-a-tool/SKILL.md);
this is that skill with a program around it. Its comments cite
`cli/agent/json_emit.go` and `agentboard/board.go` for the limits it guards
against, which are now the files next to it.

```bash
python3 examples/memory-viewer/memviewer.py --check           # findings, as text
python3 examples/memory-viewer/memviewer.py --serve           # browser, on 8765; --port 0 picks a free one
python3 examples/memory-viewer/memviewer.py -o out.html       # one self-contained page
```

`--check` exits 1 when it found something, so it works in a script. `--serve`
re-reads the corpus on every request but bakes the page template at process
start: an edit to `memviewer.py` needs a restart, not a reload.

## The harness-cli part

Three calls. `ls --json` for the destination list, `board topics` for whether
anyone is subscribed to each, and `agent send --no-contact --topic chat.<8-hex>
--data -` to publish. Everything else in the file is the viewer.

Four things it does that are worth copying into the next tool:

- **It opens no reply route.** The send runs in the name of whichever task's
  environment launched the viewer, and nothing in the viewer reads a reply.
  Without `--no-contact` the recipient would gain that task's `chat.<id>` as a
  contact — a route to answer through that nobody is waiting on.

- **The send panel exists only on a loopback bind.** `--host` off loopback
  makes the page reachable by other machines, and a form that publishes to the
  agentboard is not something to hand out with it. The panel then renders with
  the reason in place of the form, rather than failing at submit time.
- **The destination list is not free text.** Topics are the ones this process
  just discovered, and the POST handler re-derives them instead of trusting the
  page — so a stale tab cannot publish to a task that has since ended.
- **It measures the composed body and says so past 64 KiB.** Over the inline
  limit the send still succeeds and still reports `delivered_to: 1`; the
  recipient is woken with no body. That is the failure worth naming, because
  nothing else about it looks like one.

Missing `harness-cli`, a denied capability and a dead server are all ordinary
states here with a sentence a human can act on — never a stack trace and never
an empty list rendered as "nobody is listening".

**A stale ticket is the one with a known remedy, so it is named.** The viewer
authenticates with the `HARNESS_AUTH_TICKET` of the task whose shell launched
it, frozen into its environment; resuming that task issues a new ticket and
ending it revokes the old one, and from then on every call answers
`BadTicket`. `/api/payload` then carries `send.stale_auth: true` and a reason
saying to restart the viewer from a live task's shell (a failed send answers
the same field). Nothing in the process can renew the ticket. Reasons are cut
from `harness-cli`'s stderr with its slog connection lines removed first —
they used to fill the cut and leave `server rejected: B`.

```bash
python3 -m unittest memviewer_test     # from this directory
```

## It writes your notes to disk

`-o` is the default action and embeds every memory body in the page it writes —
a couple of MB of somebody's private notes, in a public repository. The root
`.gitignore` covers `out.html` at the root and any `.html` in this directory.
That is not tidiness; leave it there.
