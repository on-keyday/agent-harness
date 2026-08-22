# VT corpora

Captured PTY byte streams, so that anything which renders a session screen can
be checked against real terminal output instead of hand-written escapes: a
replay-trimming heuristic, a different emulator, a purpose-built grid model.
`vtgrid/vtcorpus_test.go` loads them; `go test -run TestVTCorpusCoverage -v ./vtgrid/`
prints what each one exercises.

## How they were made

Each file is `harness-cli session snapshot --raw` against a live session on
2026-08-22, **tail-trimmed to 256 KiB** and gzipped. Trimming from the tail is
deliberate: it keeps the most recent activity and reproduces the condition a
real reattach starts from, where the head is cut mid-sequence because the ring
evicted the frames that opened it.

**The four `*-start` corpora are the exception, and they exist because of a
mistake worth recording.** Most captures were taken after deliberately flushing
the ring with ~1 MB of neutral filler, to push a session's accumulated shell
history out of the window before it could be committed. That solved the privacy
problem and silently created a coverage one: the filler also evicted **every
session's opening bytes**, so not one capture contained a startup handshake.
The privacy fix and the coverage goal pulled in opposite directions and only
one of them was being watched.

The replacement recipe keeps both: echo a nonce, start the program, capture,
and keep the bytes **from the nonce onward**. No history survives and no
opening is lost. `win-start` needed no nonce at all — a session captured the
moment it starts has nothing in front of it.

That the startups were worth chasing is measurable: they carry the DSR cursor
query, the OSC 10/11 colour queries, DA1, Kitty keyboard push/pop, DECSCUSR,
and the private modes `2026` (synchronized output), `2027` (grapheme
clustering) and `2031` (colour-scheme notification) — none of which appears
anywhere in the steady-state captures.

Three things to know before using them:

- **There is no mode preamble.** The server prepends one
  (`server/mode_tracker.go`), but it sits at the head of the capture and the
  trim removed it. A renderer fed one of these starts from its own defaults —
  which is exactly the situation the preamble exists to paper over.
- **The size is not in the bytes.** `--raw` carries PTY output only; the size
  travels beside it. It is recorded per corpus in `vtCorpora`
  (`vtgrid/vtcorpus_test.go`) and repeated below. Render at a different size and
  the wrapping will not match what the app drew for.
- **They were produced from neutral content on purpose.** A capture is a
  photograph of a terminal, and whatever was on that terminal came with it.
  See "Keeping them publishable" below.

## What is in each

| corpus | size | source | what it exercises |
|---|---|---|---|
| `agy-start` | 40x150 | Antigravity's first bytes | a full private-mode reset sweep, Kitty keyboard push/pop (`CSI = u`), DECSCUSR (`CSI 0 SP q`), and the alt-screen exit that truncates its own replay |
| `agy-tui` | 40x150 | Antigravity CLI (Gemini) | banner, a permission prompt, an interrupt; 3.2k non-ASCII runes in only 22 KB |
| `altscreen` | 40x150 | bash, captured **while inside** the alternate screen | alt-screen content that must not survive the exit; SU/SD; DECSC/DECRC; RI |
| `bash-scroll` | 40x150 | `seq 1 200000` | pure scrolling — 196k printable runes and **one CSI in the whole file** |
| `claude-start` | 40x150 | Claude Code's first bytes, ending on the trust dialog | `ESC 7` / `ESC [ r` / `ESC 8` — a DECSTBM reset wrapped in a cursor save — then bracketed paste, focus and colour-scheme notification modes |
| `claude-tui` | 40x150 | a live Claude Code session answering arithmetic | **relative** cursor motion (CHA/CUU/CUD/CUF/CUB), 5.8k non-ASCII runes, an OSC 0 title per spinner tick, charset select, XTVERSION and DA1 queries |
| `codex-start` | 40x150 | OpenAI Codex's first bytes | the richest handshake here: DSR cursor query, `OSC 10;?`/`11;?` colour queries, DA1, Kitty keyboard query, and 12 synchronized-output brackets in 4.8 KB |
| `codex-tui` | 40x150 | OpenAI Codex answering arithmetic | boxed panels and a bordered composer; the densest CSI stream here (19k in 256 KB) |
| `conpty-ssh` | 36x173 | bash over ssh, hosted by Windows `cmd.exe` | a third emitter: everything arrives as SGR runs over printable text, no OSC at all |
| `herdr-tui` | 36x173 | the herdr multiplexer repainting a pane | **absolute** cursor motion (CUP ×5127), and the only source of **OSC 8 hyperlinks** |
| `htop` | 40x150 | htop filtered to root processes, captured **while still inside** the alternate screen | colour meters, tree view, 0.3 s repaint; VPA, and 2238 charset designations |
| `opencode-tui` | 40x150 | opencode driven by keystrokes only, no provider configured | command palette, tab switching, input editing — the Kitty keyboard protocol (`CSI = u` / `? u` / `< u`) and OSC 1337/99 live here |
| `pwsh` | 40x150 | PowerShell 7 as the interactive shell | PSReadLine writing VT **itself**, not through ConPTY: per-keystroke syntax highlighting, tab-completion cycling, `Write-Progress` |
| `torture` | 40x150 | a script written for this purpose | SGR attrs + 16/256/truecolor, CJK/combining/emoji, DECSTBM, IL/DL/ICH/DCH, TBC/HTS, autowrap off/on, EL/ED |
| `vim-split` | 40x150 | vim with a vertical split, scrolled ^F/^B/j | DECSTBM in anger, DA2/DSR queries, window ops (CSI t), DCS |
| `win-cmd` | 40x150 | native `cmd.exe`: dir/tree/ver, `color` changes, a PowerShell progress bar | the Console API translated to VT by **ConPTY** — and a CJK console locale |
| `win-start` | 40x150 | a fresh Windows session, captured before anything ran | **296 bytes: the whole ConPTY attach preamble.** `?9001h` Win32 Input Mode (twice), `?1004h` focus reporting, then `ED` + 39 newlines + `CUP` — the only corpus whose head is not cut mid-sequence |

`claude-tui` and `herdr-tui` are near-opposites in drawing strategy, which is
why both are here: a renderer that only ever meets one of them will look
correct and be wrong on the other.

Across all seventeen there are **37 distinct CSI final bytes**, ten OSC
commands and seven ESC finals. That number is the useful one — it says the
surface a renderer has to cover is enumerable, and it is a measurement rather
than an estimate.

It also appears to be saturating. The first seven captures reached 25; the next
three took it to 35; htop added two; and **`win-cmd`, `pwsh` and all four
startup corpora added none at all**. That is weak evidence, not proof — every
corpus here is a terminal someone drove on purpose, and none is a survey — but
it is the difference between "we implemented what we happened to see" and
"what we see stopped growing".

One caveat on that metric, which the startup corpora exposed: **a final byte is
not the only axis**. They introduced three private modes nobody had emitted
before (`2026`, `2027`, `2031`), and every one of those arrives as an ordinary
`CSI ? … h`, so the final-byte count did not move. Counting finals measures how
many *shapes* a parser must recognise, not how many *behaviours* a terminal is
being asked for.

The growth is the story. The first seven captures came to 25 CSI finals; adding
codex, agy and opencode took it to 35, and htop to 37. The newcomers brought
the Kitty keyboard protocol (`CSI = u`, `? u`, `< u`, `> u`), `CSI q`
cursor-shape and version queries, ECH, VPA, and OSC 4/12/66/99/1337.
**`vtgrid` implements almost none of those and rendered every one of them at
100% parity on first contact**, which is the useful evidence: what a screen
model must do with an unknown sequence is recognise its extent and skip it, and
that is testable separately from implementing it.

`win-cmd` then added **no new sequence at all**, which is worth saying out
loud: native Windows console programs do not speak VT: they call the Console
API, and ConPTY renders their screen changes back out as escape sequences. What
arrives is therefore ConPTY's vocabulary rather than the program's, and it is a
narrow one — SGR, CUP and EL carry almost everything. A Windows corpus is
valuable for the *shapes* ConPTY produces, not for sequence variety.

One shared-blind-spot check, because a differential test cannot catch two
implementations being wrong the *same* way: htop emits 2238 charset
designations, which both `vtgrid` and x/vt ignore. DEC Special Graphics is the
pre-Unicode way to draw boxes — designate it with `ESC ( 0` and the bytes
`qxlkmj` come out as `─│┌┐└┘` — so an implementation that ignores the
designation renders line art as literal letters, and two such implementations
agree with each other while both being wrong.

It is not reachable in this corpus set, by either route:

- every designation across every corpus is `ESC ( B`, select US-ASCII — the
  default. `ESC ( 0` never appears, and neither does any G1 designation.
- **SO (0x0E) count is zero** in all eleven, so nothing ever shifts to G1
  either. The ten SI (0x0F) bytes shift *in* to G0, which is where the cursor
  already is.

Every box in these captures is drawn with UTF-8 box-drawing characters. That is
checked, not assumed — and a future corpus that does designate `ESC ( 0` would
invalidate it, which is why the check is written down rather than remembered.

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
- **A shape must be tight enough not to cry wolf.** The address pattern first
  used `\d{1,3}` per octet and flagged `Microsoft Windows [Version
  10.0.26200.9168]` as a 10.0.0.0/8 address. A guard that fires on a version
  banner is worse than no guard, because the next person to meet it relaxes the
  rule. Octets are bounded to 0-255 now, and `TestLocalIdentifierPatterns`
  pins both directions — what must be caught and what must not.
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
tail -c 262144 /tmp/new.raw | gzip -9 > vtgrid/testdata/vtcorpus/<name>.raw.gz
```

Then add it to `vtCorpora` with the size the session reported
(`session snapshot --json` prints `rows`/`cols`) and a line in the table above.
A corpus with no recorded size is not usable for a differential check. Run
`go test -run TestVTCorpus ./vtgrid/` before committing.

**Beware the alt-screen trap.** If the captured session ends by *leaving* the
alternate screen, the server replays only from that exit onward
(`server/session_mux.go` `mainMark`) and the capture will be a few hundred
bytes. Capture while the full-screen app is still running, or arrange for
output after it exits.

## Known gaps

- **`opencode-tui` has no model output in it.** That capture came from an
  install with no AI provider configured, so it is the TUI's chrome — palette,
  tabs, input editing — driven by keystrokes alone. Its drawing style is real;
  a streaming answer's would be a different shape and is not represented.
- **`agy-tui` is 22 KB, not 256 KB.** Antigravity leaves the alternate screen
  during startup, and the server replays only from that exit onward (the
  alt-screen trap above), so the whole capture is what it drew afterwards.
- **`agy-start` does not begin at the nonce.** The same alt-screen exit put the
  marker *before* the replay window, so that corpus starts wherever the server
  chose to. Worth stating plainly: for any program that enters and leaves the
  alternate screen while starting, its opening bytes are **not observable
  through `session snapshot` at all** — only a client attached from before the
  program launched ever sees them.
- **There is no `opencode-start`.** The capture came back as a 359-byte
  mid-draw fragment rather than a preamble, for the same replay-window reason,
  and a fragment that is neither a startup nor representative is worse than an
  acknowledged hole.
- No sixel or kitty graphics **payloads**, and no mouse reporting — nothing
  captured emitted either, though OSC 1337 and OSC 99 (the iTerm2 and kitty
  side-channels those features also travel on) do appear. That is a fact about
  these captures, not about the agents.
- `bash-scroll` is the only corpus with essentially no escape sequences. It is
  here for the cost profile — scrolling dominates rendering time — not for
  sequence coverage.
