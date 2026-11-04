# The message log: setup and quirks

Read with the "Recording board traffic" section of SKILL.md.

## The hook

`log_messages.py` logs every arriving message once, on delivery, before anyone
reads it. Put the hook in the parent worktree's `.claude/settings.local.json`:
the runner merges its own hooks into `settings.json` and never writes
`settings.local.json`, and an untracked file stays out of `git add -u`. A new
parent worktree needs it created again.

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command",
        "command": "python3 \"$CLAUDE_PROJECT_DIR/.claude/skills/running-parallel-workers/scripts/log_messages.py\" --log <records dir>/log/<start date>-<run>.md --names '<8hex>=<name> ...'" } ] }
    ]
  }
}
```

- On Windows write `py -3` for `python3`.
- The hook form prints nothing and always exits 0, whatever fails: a hook's
  output lands in the parent's context, and a failing hook must not break the
  turn. Check the log grows after the first message; if it does not, run the
  same command by hand with a seq to see the error.
- `agent inbox --json` moves no delivery mark, so the hook takes nothing away
  from the agent's own delivery.
- By hand: `python log_messages.py --log <file> <seq>...`. A seq already in the
  log is skipped, matched on `(seq <N>,` in the headings.
- Log the instructions you send in the same file, verbatim, or point to the file
  in `instructions/` you sent. One log per parallel run.

## Quirks

- **Half-sent messages**: a child sometimes sends a message that is only a path,
  or has no payload. Wait for the rest and treat the two as one.
- **`--in-reply-to` an old seq** can be refused once it expired. Send a plain
  message and say in the body what it answers.
- **After a server restart** the seq prefix changes (`seq >> 20` is the server's
  boot time). Seqs abbreviated in your records (`…123`) can collide across a
  restart; write them whole.

## Option: a retained topic per child

`chat.<id>` cannot be retained, so the instruction you sent a child expires with
its topic. When a child must be able to re-read its instruction after a resume
or a /clear, and you hold `board_member` and `board_persist`: create
`tree.<your-task-id>.<child-name>`, `board retain` it, `board join` only that
child to it, subscribe yourself, and send that child's instructions there. The
child can then page back with `agent read --topic <topic> --before <seq>`.

Costs: two capabilities, the join step, and the child must be told to reply on
that topic. One shared topic for all children is worse — every child is woken
by every other child's traffic.
