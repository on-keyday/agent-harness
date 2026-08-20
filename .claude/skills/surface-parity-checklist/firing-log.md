# Firing log

Append-only record of what each checklist item actually DID, per walk. The
point is not bookkeeping — it is to answer two questions the list cannot answer
about itself:

- **Which items keep getting MISSED?** A defect that ships past item N twice is
  evidence that N's wording is wrong, not that the walker was careless. Item 31
  is the worked example below.
- **Which items never fire at all?** `n/a` on every walk is dead weight, and the
  skill says why that matters: "a list that trains you to type `n/a` is how the
  walk decays." Those are pruning candidates.

## How to record

One entry per walk. Record **`done`** and **`missed`** only — everything not
listed was `n/a`, so writing n/a out would triple the size and bury the signal.

```
### <date> <commit> — <one line>
done:    11, 12, 14, 17, 20, 23, 28, 35, 36, 37
omitted: 16 (reason)
missed:  31 (what shipped, and how it was caught)
```

`missed` is filled in **later**, when the defect surfaces — edit the original
entry rather than adding a new one, so an item's miss count is countable by
grepping `missed:`. A walk that was never done at all is worth an entry saying
so: skipping the walk is itself a datum about when the trigger fails to fire.

## Entries

### 2026-08-20 `d6be39c` — skills_injected on TaskInfo

done:    11, 12, 14, 16, 17, 19, 20, 23, 24, 28, 31, 32, 35, 36, 37
omitted: 13 (whoami reports the caller's own authority; skills_injected is not
         authority, and a task can observe its own worktree directly — a
         strictly stronger check than the declaration)
missed:  —

Full walk, done before landing. Items 1–10 all `n/a`: the field is
server-derived and adds no input surface anywhere.

### 2026-08-20 `74ff10d` — observer counts (viewers / cowriters)

done:    11, 12, 14, 17, 20, 23, 24, 28, 31, 32, 35, 36, 37
omitted: 16 (an 8th column overflows an 80-cell frame), 19 (picker selects
         targets; observers are not a selection criterion)
missed:  **31** — the row elided `cowrite=0 viewer=0` at zero, so "nobody is
         watching" and "this row does not report watchers" rendered the same:
         the exact ambiguity the field was added to remove, reintroduced in the
         change that added it. Caught by the operator, not by the walk.
missed:  **16** — recorded as `omitted` on a real constraint, but the constraint
         was the column COUNT, not the feature. A width-conditional column set
         (`8f4791f`) fits it at >= 60 cells. "Omitted" was accurate and still
         cost the TUI the surface for a day.

**The walk itself was skipped before landing** and reconstructed only when the
operator asked "checklist は確認した?". That is the first datum: the trigger
("adding a field to TaskInfo") fired in the text of the skill and not in
practice, on the second and third change of the same session.

### 2026-08-20 `7004296` — exec_attach split into view/cowrite/control

done:    1, 2, 15, 35, 36, 37 (+ 3, 4, 5, 6, 11, 12, 13, 14, 17, 19, 23
         automatically: every UI enumerates from `cli.GrantableCaps()` /
         `CapsCatalog()`, so the new bits appeared with no edit)
missed:  — but the walk surfaced a **missing guard**: nothing tied
         `GrantableCaps()` to `Capability_All`, so a future cap added to the
         enum but not the list would silently make the WebUI `[all]` button
         grant a SUBSET of the server's `all`. Fixed in `db75eeb`. No item
         covers "the catalog must stay complete"; item 15 asks for the catalog
         to be updated, not for a test that catches the next omission.

Walk also skipped before landing; reconstructed with the one above.

### 2026-08-20 `8f4791f` — width-conditional Obs column

done:    16, 17, 34 (new)
missed:  —

Item **34 was born here**: making the column set dynamic panics inside bubbles'
`renderRow` if the rows and columns are ever swapped separately. Found by a
probe run, not by review. Per the skill's closing rule the number was added and
the list renumbered to 1–37.

## Standing tallies

Update when adding an entry.

| item | done | missed | note |
|---|---|---|---|
| 31 (don't hide a value for what it IS) | 2 | **2** | Both misses were the same shape: an elision the item's own text licensed. The row-width exception is now withdrawn — if 31 misses again, the wording is still wrong. |
| 16 (TUI task table) | 2 | 1 | Missed once as a defensible `omitted`; the constraint was real, the conclusion was not. |
| 13 (whoami) | 0 | 1 | Also elided `scope=subtree` until `d437f6e`. Easy to forget because it is not a task listing.
| 34 (dynamic column sets) | 1 | 0 | New. |
| 1–10 (input surfaces) | 1 walk | 0 | `n/a` for every field-only change. Do NOT prune: they fired fully for the caps split, which is exactly the change that needed them. |

**Never fired yet:** 8, 9, 21, 22, 25, 26, 27, 29, 30, 33. Too few walks to
call any of them dead — revisit after a change that adds a spawn OPTION rather
than a display field, which is what most of them are for.
