# VT corpora

Captured PTY byte streams, so that anything which renders a session screen can
be checked against real terminal output instead of hand-written escapes: a
replay-trimming heuristic, a different emulator, a purpose-built grid model.
`cli/vtcorpus_test.go` loads them; `go test -run TestVTCorpusCoverage -v ./cli/`
prints what each one exercises.

## How they were made

Each file is `harness-cli session snapshot --raw` against a live session on
2026-08-22, **tail-trimmed to 256 KiB** and gzipped. Trimming from the tail is
deliberate: it keeps the most recent activity and reproduces the condition a
real reattach starts from, where the head is cut mid-sequence because the ring
evicted the frames that opened it.

Three things to know before using them:

- **There is no mode preamble.** The server prepends one
  (`server/mode_tracker.go`), but it sits at the head of the capture and the
  trim removed it. A renderer fed one of these starts from its own defaults —
  which is exactly the situation the preamble exists to paper over.
- **The size is not in the bytes.** `--raw` carries PTY output only; the size
  travels beside it. It is recorded per corpus in `vtCorpora`
  (`cli/vtcorpus_test.go`) and repeated below. Render at a different size and
  the wrapping will not match what the app drew for.
- **They were produced from neutral content on purpose.** A capture is a
  photograph of a terminal, and whatever was on that terminal came with it.
  See "Keeping them publishable" below.

## What is in each

| corpus | size | source | what it exercises |
|---|---|---|---|
| `altscreen` | 40x150 | bash, captured **while inside** the alternate screen | alt-screen content that must not survive the exit; SU/SD; DECSC/DECRC; RI |
| `bash-scroll` | 40x150 | `seq 1 200000` | pure scrolling — 196k printable runes and **one CSI in the whole file** |
| `claude-tui` | 40x150 | a live Claude Code session answering arithmetic | **relative** cursor motion (CHA/CUU/CUD/CUF/CUB), 5.8k non-ASCII runes, an OSC 0 title per spinner tick, charset select, XTVERSION and DA1 queries |
| `conpty-ssh` | 36x173 | bash over ssh, hosted by Windows `cmd.exe` | a third emitter: everything arrives as SGR runs over printable text, no OSC at all |
| `herdr-tui` | 36x173 | the herdr multiplexer repainting a pane | **absolute** cursor motion (CUP ×5127), and the only source of **OSC 8 hyperlinks** |
| `torture` | 40x150 | a script written for this purpose | SGR attrs + 16/256/truecolor, CJK/combining/emoji, DECSTBM, IL/DL/ICH/DCH, TBC/HTS, autowrap off/on, EL/ED |
| `vim-split` | 40x150 | vim with a vertical split, scrolled ^F/^B/j | DECSTBM in anger, DA2/DSR queries, window ops (CSI t), DCS |

`claude-tui` and `herdr-tui` are near-opposites in drawing strategy, which is
why both are here: a renderer that only ever meets one of them will look
correct and be wrong on the other.

Across all seven there are **25 distinct CSI final bytes**, five OSC commands
and seven ESC finals. That number is the useful one — it says the surface a
renderer has to cover is enumerable, and it is a measurement rather than an
estimate.

## Keeping them publishable

This repository is public. `TestVTCorpusNoLocalIdentifiers` fails the build if a
corpus holds a private-range address, a `user@host` prompt or window title, a
home-directory path, a runner connection id, or a field whose value encodes an
address.

That test exists because the first attempt at this directory would have shipped
all of those. Three lessons are baked into how it is written:

- **Match shapes, not names.** The obvious deny-list — the actual hostnames and
  usernames — cannot be used, because in a public repository *the deny-list is
  the disclosure*. Shapes leak nothing, and they turned out to catch strictly
  more: connection ids, and the `/home/kf` fragments a TUI leaves when cursor
  motion splits a path across two writes, both of which a name list missed.
- **Prefer re-capturing over scrubbing.** One capture held server log lines
  whose base64 fields *encode* addresses; a text substitution does not reach
  those. Content you chose is safe in a way content you filtered is not.
- **If you do substitute, keep the byte length identical and pick a replacement
  that does not match the shapes.** A VT stream carries column positions, so a
  shorter placeholder moves every cell after it and quietly invalidates the
  corpus for the one thing it is for. And a placeholder that still looks like
  `user@host` would force an exemption list, which is a hole.

## Adding one

```bash
harness-cli session snapshot --raw --settle-ms 2500 <task-id> > /tmp/new.raw
tail -c 262144 /tmp/new.raw | gzip -9 > cli/testdata/vtcorpus/<name>.raw.gz
```

Then add it to `vtCorpora` with the size the session reported
(`session snapshot --json` prints `rows`/`cols`) and a line in the table above.
A corpus with no recorded size is not usable for a differential check. Run
`go test -run TestVTCorpus ./cli/` before committing.

**Beware the alt-screen trap.** If the captured session ends by *leaving* the
alternate screen, the server replays only from that exit onward
(`server/session_mux.go` `mainMark`) and the capture will be a few hundred
bytes. Capture while the full-screen app is still running, or arrange for
output after it exits.

## Known gaps

- **codex** and **agy** are not represented. Both are installed on the machine
  these came from but were not running.
- No sixel or kitty graphics, and no mouse reporting — nothing captured emitted
  either. That is a fact about these captures, not about the agents.
- `bash-scroll` is the only corpus with essentially no escape sequences. It is
  here for the cost profile — scrolling dominates rendering time — not for
  sequence coverage.
