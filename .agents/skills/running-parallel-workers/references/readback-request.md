# Skeleton: a readback request for user-facing text (TASK.md)

One brief among those a reader can be given: this one asks where user-facing
text can be misread, and confines the reader to the files it is handed. A code
or design review needs its own brief (see `independent-review`), and may let the
reader into the repository.

Put this in the prepared directory as `TASK.md`, beside the files to read.
Replace NAME with the directory's name and the "What you are given" section with
this repository's description of the input. `review_one.py` counts the
findings by the `N. **quote:**` lines, so keep that output shape.

```
# Task: find passages a newcomer could misread (NAME)

You are an outside reader. You know nothing about this tool beyond the files in this directory, `review/NAME/`.

## What you are given
<what each file is: e.g. "help.txt is what a user sees when they run the tool with --help;
terms.md explains the labels it uses">

Use only these files. Do not open any other directory under `review/`, and do not use anything you may remember from an earlier task. Each directory is a separate task.

## What to look for
Read the text the way a user who wants to understand it would read it. List every passage where a careful reader could reach a meaning other than the one most likely intended, or could not decide what is meant. That includes:

- a term used before it is defined, or defined nowhere in these files
- a sentence with two plausible readings
- a statement that seems to contradict another statement or an example
- a claim whose condition or scope is unclear (when it applies, to what)
- an example whose labels do not obviously follow from the text

Do not judge whether the tool is technically right, and do not propose rewrites. Only report where the text can be misread. If you believe some syntax in an example does not exist, say so as a reading, but mark it low confidence: your knowledge may be out of date.

## Output
Write your findings to `review/NAME/findings.md`, as a numbered list. Each item has these parts:

1. **quote:** the exact text, with its section heading
2. **readings:** the readings a reader could take (at least two, or "cannot tell")
3. **why:** what in the text allows each reading
4. **missing:** the information that would settle it
5. **confidence:** high, medium or low that a real user would stumble here

Order the list from high to low confidence. At the end, add a short paragraph on the text as a whole: what a newcomer would still not understand after reading it.

Do not edit any other file. Do not commit. When you are done, say so in one line.
```
