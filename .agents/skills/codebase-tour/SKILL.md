---
name: codebase-tour
description: Use when a repository's owner wants to see where the code stands against what they decided — they have stopped reading the code agents write, specs go unread, unease about drift or recurring "A and B disagreed" bugs — when running-parallel-workers' tour checkpoint fires, and when answering the owner's pushback on such a page.
---

# codebase-tour (one page for the owner to push back on)

When agents write the code, the owner stops reading it, and the work still
functions. Tests pass, behaviour is right, and nothing reports that a decision
the owner made was only half carried out. This skill puts one page in front of
the owner, then answers what they push back on.

**The owner's decisions are the yardstick.** Every item on the page is a
decision and how far the code has carried it out. The author's own sense of
what debt looks like does not pick items. Why: in the run this skill comes
from, items picked by the author's signals ("comments calling themselves
mirrors", file growth) produced one wrong item and one the owner already knew.
The useful outcome came from the owner's follow-up question, not from the page.

**The page is the opening, the conversation is the work.** It exists to be
questioned, so it is short and every claim on it holds up.

Not for: correctness review (tests and review do that); unattended runs (a page
nobody questions has no purpose); a metrics dashboard.

## The tour directory

Each repository has one tour directory, **outside the repository**. It holds
the pages (`<date>.md`) and `settings.md`.

- Inside running-parallel-workers it is `<records>/tours/`.
- A standalone run uses the same directory when those records exist, so there
  is one reaction history. To find them, look in the repository for
  running-parallel-workers' conventions document. If it names a records
  location, the tour directory is `<records>/tours/`.
- Without records, ask the owner where to put it on the first run. On later
  runs the owner names it again, since nothing in the repository may point to
  it.

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

Pages contain local paths, so **a page is never committed** to the repository.

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
   baseline is the commit the last page recorded. With no earlier page, use a
   point the owner names, or the commit 30 days back.
2. **Gather decisions.** List decisions made or touched since the baseline,
   plus any still open from earlier pages. Record where each one lives (file
   and row) and whether it is the owner's or an agent's. An agent's decision
   that the owner confirmed in an earlier page's reaction line is the owner's;
   do not ask about it again.
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
   statement. Drop anything an owner decision explains. Label anything you
   could not check. A difference that only an agent's decision covers stays,
   labelled as that agent's. Why: in the source run, a badge spelled
   differently on two surfaces went on the page as drift, but an owner-marked
   spec row had decided that difference. The owner spent a round correcting
   the page instead of using it.
7. **Write the page.** Use about three items, in the format below, in the
   owner's language and a polite register. Record the commit the page
   describes. More items do not get more pushback; the owner engages with one
   or two.
8. **Hand it over and end the turn.** The owner may take a while.
9. **Answer the pushback** (below).
10. **Record reactions** on the page, one line per item.

## The page

```
# Codebase tour — <date>  (describes <commit>; baseline <commit>)

## <n>. <decision, in a few words>: <how far it got, in a few words>

Decision: <what was decided>, recorded at <file and row> — <the owner's |
an agent's, not yours (do you stand by it?)>.
Now: <how far it got, with the figure>; <where the rest is>.
<Locations in the settings.md form; two things being compared go side by side>

What I'd like to ask: <what you intended, or what you want done>

---
Reactions (filled in after the conversation):
- <n>: pushed back | accepted | passed over — <what came of it>
```

**The question asks what only the owner can answer:** what they intended, or
what they want done. When a fact matters, show it side by side with its
counterpart so the difference is visible on the page. Why: in the source run,
questions that hid an answer the author already knew and asked the owner to
predict it read as being tested, which is the wrong seat for the person who
owns the code's intent.

## Answering the pushback

Answer each question by digging into the artifact: history, decision records,
the code, a run. Do not answer from memory or a summary. Why: in the source
run, every finding that mattered came from this digging, such as how a split
had only half happened and a comment naming a function that never existed.
None of them was on the page.

Route each outcome as follows:

- **Inside running-parallel-workers:** the parent does no child's work. A fix
  becomes a backlog item or a child's assignment. A choice becomes a registered
  owner question (`Q<n>`).
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
| Quiz questions with the answer held back | The owner feels tested | Ask intent or choice; show facts side by side |
| A long, complete report | Nothing gets questioned | About three items |
| Committing the page | Local paths published | Keep it in the tour directory |
