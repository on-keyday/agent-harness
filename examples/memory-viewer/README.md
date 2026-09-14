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
python3 examples/memory-viewer/memviewer.py --serve --port 0  # browser; 0 picks a free port
python3 examples/memory-viewer/memviewer.py -o out.html       # one self-contained page
```

`--check` exits 1 when it found something, so it works in a script. `--serve`
re-reads the corpus on every request but bakes the page template at process
start: an edit to `memviewer.py` needs a restart, not a reload.

## The harness-cli part

Three calls. `ls --json` for the destination list, `board topics` for whether
anyone is subscribed to each, and `agent send --topic chat.<8-hex> --data -` to
publish. Everything else in the file is the viewer.

Three things it does that are worth copying into the next tool:

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

## It writes your notes to disk

`-o` is the default action and embeds every memory body in the page it writes —
a couple of MB of somebody's private notes, in a public repository. The root
`.gitignore` covers `out.html` at the root and any `.html` in this directory.
That is not tidiness; leave it there.
