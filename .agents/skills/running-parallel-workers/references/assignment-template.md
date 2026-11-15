# Skeleton: a child's assignment

Write it as `instructions/<date>-<child>-<n>.md` in the records directory, then
send that file's content with
`harness-cli agent send --topic chat.<child-short-id> --no-retire-on-reply`.
Fill every `<...>`. Keep the owner's decision, the letter of a convention, and
your own judgment visibly apart.

```
From the parent (task <parent id>, topic chat.<parent short id>) to child <name>, <output of date>.
Your conversation is new, so this assumes you remember nothing of earlier work.

First, reply as soon as you receive this:
  harness-cli agent send --topic chat.<parent short id> --in-reply-to <this message's seq> --data '{"kind":"hello_ack"}'

## Where things stand
The previous work <plan> has landed. The trunk is now at <commit>. <tooling changes>
This repository's conventions for parallel work are in <path>; read it in full.

## Your item (<owner's scope decision, <date and time>, question Q<n> | next in backlog order, as the conventions allow>)
<item name, or issue #N>. Read its section (or the issue) and every document it refers to, in full, before you write anything.
- <judgments that fall under the conventions' owner-decision kinds go into the design's "owner decisions", by number>
- <inputs to verify against>
- <what this work does not promise>

## How to proceed
- <the stages the conventions name>
- <with a reader: ask the parent for a reading of <what the conventions give the reader> before the final review (at most <n> rounds)>
  <without one: the work gets only the reviews the conventions name>
- <if the conventions keep estimates: when your plan is done, send the estimate per task and in total before implementing; record the actual from your `date` notes when you report ready>
- Before you settle for declaring or recording something, write whether it can be fixed and what that would cost.
- In parallel: child <name> is doing <plan>. Files that may collide: <files>. Collisions are resolved by the branch sync before landing.
- Checks: <which checks to run while you work, and whether you or the parent runs the landing checks, as the conventions say>
- Resources: <memory threshold on your machine, the heavy-check lock and how to use it (if the conventions use one)>

## First steps
1. Note the start time with `date`.
2. Bring your branch up to the trunk (<commit>) as the conventions say.
3. `harness-cli progress "<plan>: <first stage>"`. Rewrite it at every change of stage, and note the `date` when you do.

## Do not
- Land (merge or push to the trunk<, open the PR>). The parent lands.
- Create sessions of your own (a research subagent is fine).
- Use AskUserQuestion. Write questions for the owner in your message to the parent.
- `git add -A`. Use `git add -u` and explicit paths.
- Write about the harness or about accounts in the repository.
- Run an outward experiment before sending it to the parent.
- Write auto-memory. Send what you learned to the parent in your report.

## Reports
- Put the body in the message itself. Times are the output of `date`.
- If the owner talks to you directly, send the parent what the owner said, verbatim, with the `date`, at once; then act on it.
- When the design is written down, send where it is and the list of owner decisions.
- When the work is done, send the branch tip commit, check results, review findings you set aside, and the decisions you want put to the owner.
- Include durable learning candidates with source task/commit, verification result and applicable environment; label hypotheses and unresolved conflicts. The parent integrates shared memory (see `harness-cli skill memory-maintenance references/learning-report.md`).
- Reply with: harness-cli agent send --topic chat.<parent short id> --in-reply-to <seq> --data '<JSON>'
```
