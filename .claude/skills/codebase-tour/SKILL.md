---
name: codebase-tour
description: Use when a repository's owner wants to see the code as it stands now — they ask to be shown the code, they have stopped reading the code agents write, specs go unread, unease about drift or recurring "A and B disagreed" bugs — when running-parallel-workers' tour checkpoint fires, and when answering the owner's pushback on such a page.
---

# codebase-tour (walking the owner through the code, against what they decided)

When agents write the code, the owner stops reading it, and the work still
functions. Tests pass, behaviour is right, and nothing reports that a decision
the owner made was only half carried out. This skill puts one page in front of
the owner that walks them through the code itself, then answers what they push
back on.

**The owner's decisions are the yardstick.** Every item on the page is a
decision and how far the code has carried it out. The author's own sense of
what debt looks like does not pick items. Why: in the run this skill comes
from, items picked by the author's signals ("comments calling themselves
mirrors", file growth) produced one wrong item and one the owner already knew.
The useful outcome came from the owner's follow-up question, not from the page.

**The page shows the code, not just where it is.** Each item walks through the
code in reading order, quoting it. Why: in the source run, a page that gave
locations and a line or two of evidence, under rules that only asked for
brevity, left the owner with nothing to read. Another session using this skill
would not show code at all, and when the owner asked to see the code, it said
that was outside the skill. The same item rewritten as a walk through quoted
code was the one the owner found clear.

**The page is the opening, the conversation is the work.** It exists to be
questioned, so it has few items and every claim on it holds up.

**Showing the code is always this skill's job.** When the owner asks to see
code, walk them through it in the form below, whether or not a decision
anchors it.

Not for: correctness review (tests and review do that); unattended runs (a page
nobody questions has no purpose); a metrics dashboard.

## The tour directory

Each repository has one tour directory, **outside the repository**. It holds
the pages, `settings.md`, and `walks/` (code walks no decision anchors; see
"Answering the pushback").

**Page files are numbered:** `<nnn>-<date>.md`, three digits from `001`, such
as `001-2026-11-07.md`. The highest number is the last page. A page redone,
even on the same day, takes the next number; no page is overwritten, so every
reaction stays on record. Why: with date-only names, two pages on one day left
no way to tell which was the last one, and the baseline comes from it. Pages
from before this rule, named by date only, are renamed once in date order,
from `001`.

- Inside running-parallel-workers it is `<records>/tours/`.
- A standalone run looks for where the repository's own conventions put it.
  Repositories differ: one has running-parallel-workers' conventions document
  naming a records location (then `<records>/tours/`, so there is one reaction
  history), one records the tour directory some other way, one records it
  nowhere. Look; assume none of these.
- If you keep a memory that persists across sessions, look there too. Once the
  directory is known, record it there, so the next run finds it without asking.
- If nothing names it, ask the owner. If it is named but you cannot read or
  write it (a sandbox that does not mount it), say so and wait. Never pick a
  directory yourself, not even one you can write to. Why: a run that found
  nothing chose a directory it could write to and labelled its settings
  "proposed", while the owner's tour directory, with two pages and settled
  settings, sat where that run's sandbox could not see.

`settings.md` holds two answers, settled with the owner on the first run (or
copied from running-parallel-workers' conventions):

1. **How code locations are written** so the owner can open them as they are in
   their editor. One owner may want an absolute `path:line` in their own
   checkout; ask, don't assume. Why: a relative link resolves only from where
   the page sits, and the page sits outside the repository.
2. **Where the owner's decisions are recorded and how their maker is marked.**
   Examples are a `Decided by` column in specs, `DECIDED (operator, <date>)`
   text, or an answered-questions directory. A record without such a mark
   counts as an agent's decision.

Pages and walks contain local paths, so **neither is ever committed** to the
repository.

## Whose decision it is

- **The owner's decision** is a record marked as the owner's under
  `settings.md`, a line in conventions they approved, or an answer they gave.
- **An agent's decision** is anything else that records a choice: a spec row
  the author decided, a plan line, a commit message, a code comment.

An agent's decision may anchor an item. The page then says it is an agent's
and asks whether the owner stands by it. If the owner confirms it, it is the
owner's from then on; record that in the item's reaction line.

**Only the owner's decisions explain a difference away.** If an agent's written
reason is the only thing covering a difference, the difference stays on the
page as that agent's decision. Why: the tour exists to show drift from the
owner's intent. An agent's justification is exactly where that drift hides, so
letting it cancel a finding would hide the thing the page is for.

## Procedure

1. **State and baseline.** Find the tour directory and read `settings.md`. On
   a first run, settle its two answers with the owner and write them. The
   baseline is the commit the highest-numbered page recorded. With no earlier page, use a
   point the owner names, or the commit 30 days back.
2. **Gather decisions.** List decisions made or touched since the baseline,
   plus any still open from earlier pages. Record where each one lives (file
   and row) and whether it is the owner's or an agent's. An agent's decision
   that the owner confirmed in an earlier page's reaction line is the owner's;
   do not ask about it again. Look first at decisions carried out since the
   last page: drift there is newest, and the reasons written for it are
   still fresh. Older decisions stay candidates; the first page of the source
   run held three, all pushed back on.
3. **Gather the owner's symptoms.** Take the symptoms the owner has named in
   conversation, in questions or in earlier reactions ("many bugs where A and
   B disagreed"). Grouping the fix commits since the baseline by the kind of
   fault they repair shows a symptom's shape.
4. **Read earlier reactions.** Raise the weight of sources whose items the
   owner pushed back on. Lower it for sources whose items were passed over.
5. **Measure how far each decision got.** For each candidate, look at the
   artifact: how much of what was decided is in place, and where the rest is.
   Mechanical searches (grep, line counts, history) are how you find this
   evidence. They are never a reason on their own to list an item.
6. **Check every claim.** Re-read the decision record and the code behind each
   statement. That includes the comments around the code and the commit that
   introduced it, which is where an agent usually wrote down its reason. Drop
   anything an owner decision explains. Label anything you could not check. A difference that only an agent's decision covers stays,
   labelled as that agent's. Why: in the source run, a badge spelled
   differently on two surfaces went on the page as drift, but an owner-marked
   spec row had decided that difference. The owner spent a round correcting
   the page instead of using it.
7. **Read the code you will show, then write the page.** Use about three
   items, in the format below, in the owner's language and a polite register.
   If no more than one or two hold up after step 6, use those; never pad. When
   the owner names a number, use theirs. Why: the first page of the source run
   held three, and the owner pushed back on all three. Record the commit the
   page describes. When it is written, open every location at its line and check
   that the quoted code is there. Why: a page author once wrote line 16 for
   code on line 18.
8. **Hand it over and end the turn.** The owner may take a while.
9. **Answer the pushback** (below).
10. **Record reactions** on the page, one line per item.

## The page

````
# Codebase tour <nnn> — <date>  (describes <commit>; baseline <commit>)

## <n>. <decision, in a few words>: <how far it got, in a few words>

Decision: <what was decided>, recorded at <file and row> — <the owner's |
an agent's, not yours (do you stand by it?)>.

### ① <what this stop shows, in a few words>

<location in the settings.md form>

```<language>
<the code itself>
```

<what to look at here, in one to three lines>

### ② … (more stops, in reading order)

### Now

<how far it got, with figures, each with the command that produced it;
where the rest is>

What I'd like to ask: <what you intended, or what you want done>

---
Reactions (filled in after the conversation):
- <n>: pushed back | accepted | passed over — <what came of it>
````

**The stops follow the order you would read the code in:** where it starts,
how it is laid out, where the part that follows the decision meets the part
that does not, and an earlier case that already solved the problem, if one
exists.

**When the point is behaviour, a stop is a run:** the smallest input that
shows it, varied in one thing at a time, with the command and its output
quoted. Run only what is safe in the checkout: nothing that pushes, sends, or
writes outside a scratch place. Why: in a run of this skill on another
repository, the strongest evidence on the page was two lines of a linter run
on the same input with and without one field.

**Each stop quotes enough code to read without opening the file:** a whole
function, or a call together with the code it reaches. Elide long middles
with `...`, keeping the lines the prose points at. When the code is too long
for that, such as a dispatch over dozens of cases, quote its shape instead: the
head, one case of each kind, and how many cases there are. When two things are
compared, quote both, one after the other. An agent's reason written in a
comment is quoted too, labelled as an agent's.

**The prose under a quote says what to look at.** It does not restate the
code line by line.

**The question asks what only the owner can answer:** what they intended, or
what they want done. Why: in the source run, questions that hid an answer the
author already knew and asked the owner to predict it read as being tested,
which is the wrong seat for the person who owns the code's intent.

If the decision is fully carried out and nothing is left for the owner to
decide, the item says so in place of a question. A question found by looking
around the item is a new item, picked by your own signals.

## Answering the pushback

Answer each question by digging into the artifact: history, decision records,
the code, a run. Do not answer from memory or a summary. Show the code you
found, in the same quoted form as the page. Why: in the source run, every
finding that mattered came from this digging, such as how a split had only
half happened and a comment naming a function that never existed. None of
them was on the page.

When the owner asks to see code the page did not cover, walk them through it
the same way. A walk that no decision anchors is kept, unless the owner says
the conversation is enough, so they can read it again later. It goes in
`walks/` in the tour directory as `<nnn>-<date>-<topic>.md`, numbered on its
own from `001`, in the same quoted form. It is not a page: it gets no reaction
line, and the baseline never comes from it, because pages are the record of
decisions measured.

Route each outcome as follows:

- **Inside running-parallel-workers:** the parent does no child's work. A fix
  becomes a backlog item or a child's assignment, or, when the owner agrees,
  goes into the current item of a child already working in that area, as a new
  instruction. A choice becomes a registered owner question (`Q<n>`).
- **Standalone:** make a fix yourself when the owner asks for one. Put a
  choice to the owner directly.

## Reactions

After the conversation, give each item one line: pushed back, accepted, or
passed over, plus what came of it (a fix, a question, a confirmation that an
agent's decision is now the owner's). The next run reads these lines in step 4.
That is how the choice of sources learns from the owner instead of from the
author's assumptions.

## Common mistakes

| Mistake | What happens | Instead |
|---|---|---|
| Picking items by your own debt signals | Wrong or already-known items; the owner stops reading | Anchor on decisions; use signals only to find evidence |
| Calling a difference drift before checking the records | The owner spends a round correcting you | Step 6; only owner decisions explain a difference away |
| Letting an agent's spec row cancel a finding | The drift the tour exists to show is hidden | Keep it, labelled as an agent's decision |
| Listing locations without quoting the code | The owner has nothing to read and must open every file | Stops in reading order, each with the code quoted |
| Declining to show code because no decision anchors it | The owner cannot see the code they asked for | Showing code is always this skill's job; walk them through it |
| Quiz questions with the answer held back | The owner feels tested | Ask intent or choice; show facts side by side |
| A long, complete report | Nothing gets questioned | About three items, each with its walk; fewer only when no more hold up; the owner's number when they name one |
| Committing the page | Local paths published | Keep it in the tour directory |
