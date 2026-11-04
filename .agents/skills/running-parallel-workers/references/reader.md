# A reader on a different model: briefs and delivery

Read with the "A reader on a different model" section of SKILL.md.

## What it reads, and the brief

The conventions set what the reader reads, what it is asked, and whether it
reads only what it is handed or may look into the repository as it needs.

- The run this skill comes from gave it only **user-facing text** — help,
  explanations, error messages — and asked where a newcomer could misread it,
  without judging correctness or proposing rewrites.
  `readback-request.md` is that brief; replace its description of the input with
  this repository's.
- It can as well review **code** or a **design**, with access to the repository
  where the review needs it. Give it a brief that fits; `independent-review` has
  an adversarial one for code and designs.

## Reaching it

- **A reader that reads its inbox** (skill-injected claude, or any runtime with
  the hook): push the files with `file push`, send the request over the
  agentboard, `/clear` it by keystrokes between rounds.
- **A reader with no agentboard hook, on the same machine**:

  ```
  python scripts/review_one.py --reviewer <id> --worktree <its worktree> <name> <records>/readback/prepared/<name>
  ```

  The prepared directory holds `TASK.md` (the brief) and the input. The script
  archives any earlier review left in the reader's `review/`, refuses to go on
  unless `review/` is then empty, copies the input in and checks `TASK.md`
  arrived, sends `/clear` and the request by keystrokes, waits for `findings.md`
  and an idle reader, and archives the result to `<records>/readback/`. It does
  not verify the /clear — snapshot the reader when a run looks wrong.

## Round limit

Set in the conventions. For user-facing text the run this skill comes from used
two: past that, rewording mostly creates new ambiguity. What changed after the
last round, you check yourself and record what you checked.
