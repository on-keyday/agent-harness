# Owner questions: q.py and the question file

Read with the "Questions for the owner" section of SKILL.md, which has the
rules and why. This is the tool and the format.

## q.py

Works on `questions/` in the records directory (`--dir`, else `PARALLEL_DIR`,
else the recommended place).

```
python q.py                         open questions; those with a "blocks:" line first
python q.py all                     every question
python q.py <n>                     one question in full
python q.py register <draft>        number and timestamp a draft, file it as open
python q.py answer <n> <text>       record the owner's answer, file it as answered
python q.py answer-recommended <n>...
                                    answer each with its one option marked
                                    "(recommended)" — for when the owner says
                                    "the recommendations, all of them"
```

The owner runs `! python3 <path to q.py>` (`py -3` on Windows) from the
repository for the open list. Give them the exact command: with `--dir <records>`
unless the records are at the recommended place, or outside the repository. With
a "blocks:" line on every question that holds a child, the owner can answer the
ones that stop work first.

`answer-recommended` refuses a question with no option, or more than one,
marked "(recommended)" and leaves it open. It records which option was taken and
that it was the recommendation accepted as proposed, so the record shows the
owner did not choose it independently.

## The file

`register` refuses a draft without the `id:`, `asked:` and `status: open` lines.
It fills `id:` and `asked:`, and turns every `Q?-` into `Q<n>-`. `answer`
rewrites `status:` and the `answer:` line.

```
id: ?
status: open
plan: <the work this belongs to>
title: <the decision, in a few words>
blocks: <who waits on the answer — "child T (idle)", "child U after its plan"; empty if no one>
asked:
answer-form: <"one of the options" | "yes / no" | "free text">
answer:

## What needs deciding
<one or two sentences>

## What was decided before
<when, in which work, in which question, what was decided — written out, never "the earlier decision">

## What we know
<facts, each with where it comes from>

## Options
- Q?-1 <option> (recommended)
- Q?-2 <option>

## Why the recommendation
<the reasons; when evidence was added since, which way each new fact leans and what would change the recommendation>
```

Mark exactly one option "(recommended)" when you have a recommendation. Keep
"blocks:" current: clear it when the child it named moved on to other work.
`Q<n>-<m>` names the options of this one question. Decisions the owner makes
independently are separate questions with their own numbers.
