# PTY wake as a bracketed paste, and input modes across a server restart

Status: DECIDED (operator, 2026-10-24) — rows marked `this spec` in the
decision table were chosen while writing and are the ones worth a second look.

## Problem

1. **A PTY wake can be typed and never submitted.** `ptyWake`
   (`runner/session.go`) writes the marker, sleeps `wakeSubmitDelay` (100ms),
   then writes a lone `\r`, relying on the agent to see the two writes as two
   reads. The sleep is on the WRITER; whether the agent reads in between is
   decided by the agent's event loop. When the agent is late (host overload, a
   `/clear` being processed), both writes sit in the PTY queue, one `read()`
   returns them together, and the agent takes the `\r` as a newline inside the
   input box — the failure the `wakeMarker` comment already records for a
   single combined write.

   Observed 2026-10-23 on a fleet claude: two wakes 23 minutes apart were both
   logged `wake written` by the runner and neither started a turn; the first
   coincided to the second with a `/clear`. The agent's next prompt, submitted
   by a third wake 32 minutes later, was the marker three times:
   `marker \n marker marker`. The first `\r` became a newline, the second
   vanished, the third submitted.

   Reproduced deterministically on a dummy harness (claude 2.1.307): `kill
   -STOP` the agent, `board wake chat.<id>`, wait for `wake written`, `kill
   -CONT`. The marker stays in the input box with the cursor on a new line. A
   second, unstopped wake appends its marker and its `\r` vanishes; a third
   submits `marker\nmarkermarker`, byte-identical to the fleet prompt. So the
   two observed losses have one cause, the first. Why the second `\r` vanishes
   is the agent's own input handling and is not explained here.

   Meanwhile the message is at risk: a topic is evicted 30 minutes after its
   last publish (`--agentboard-ttl`), unread or not, so an agent finally woken
   later finds nothing to read.

2. **WakeStdin blocks the runner's receive loop.** `RunnerRequestType_TaskWake`
   calls `session.WakeStdin` synchronously (`runner/connect.go`), on the loop
   `RebindSession`'s comment there says must never wait. Every wake stalls
   every control message for every task on that runner by the 100ms sleep, and
   the stdin write itself blocks while the PTY's input queue is full — an
   agent that stops reading (stopped with Ctrl-Z, say) and keeps being woken
   eventually stalls the loop outright.

3. **A deliberate server restart drops the input modes.** A held session's
   capture (`heldCapture`, `server/session_mux.go`) is the PTY size and
   `vtgrid` `Repaint()`. `Repaint` deliberately carries no input-affecting
   private modes — bracketed paste, mouse, application cursor keys — because
   the mode tracker's preamble is meant to (comment in `vtgrid/repaint.go`). The
   capture carries no preamble, so the re-adopted mux's tracker starts with
   none of them. claude re-asserts `?2004h` constantly and recovers at once;
   codex, agy, opencode and pi set it at startup only, so after a server
   restart an attaching client's terminal is never told to bracket pastes.

## Design

### 1. One mode tracker, shared (`termmode`)

`server/mode_tracker.go` moves to a new package `termmode` as `Tracker`, with
its tests. Behaviour is unchanged; the API is exported:

- `New() *Tracker`, `Feed([]byte)`, `OnAltScreen() bool`, `Preamble() []byte`
  — today's `newModeTracker` / `feed` / `onAltScreen` / `preamble`.
- `Mode(n int) (set, seen bool)` — the last value of private mode `n`.
- `Modes() []byte` — the ordinary part of `Preamble`: every tracked mode at its
  last value except DECTCEM, the alternate-screen modes and synchronized
  output. `Preamble` is the screen-switch head followed by `Modes()`.
- `const BracketedPaste = 2004`.

The runner needs the same parser the server has; a second parser would be a
second answer to the same question.

### 2. Runner: the wake is a bracketed paste when the agent asked for them

The PTY path of `handleOpenExec` gives agentexec an `Auditor` whose `Stdout`
(and `Stderr`) feed a `termmode.Tracker` — the hook the stream kind already
taps (`streamTap`). The runner sees every byte the agent writes from its first,
and survives a server restart with it, so its answer does not depend on a
server that may have been replaced since the agent set the mode.

`ptyWake` takes the tracker. At each wake:

- `Mode(BracketedPaste)` set → ONE write:
  `ESC[200~` marker `ESC[201~` `\r`. The paste end is explicit, so the agent
  takes the `\r` as Enter even when everything arrives in one read.
- otherwise → today's split write, unchanged.

The marker is a constant with no `ESC[201~` in it; that is what makes it safe
to bracket. Arbitrary text would need that sequence stripped first.

Measured 2026-10-24, each agent's process group stopped across one such write
and continued (the merged-read condition): claude 2.1.307, codex 0.163.0,
agy 1.4.6, opencode 1.18.18 and pi all submitted the text as one prompt. All
five set `?2004h` at startup.

### 3. Runner: WakeStdin off the receive loop

`TaskWake` dispatches `go session.WakeStdin(id)`. `taskEntry` gains
`waking bool`: `WakeStdin` sets it under `s.mu` before writing and clears it
after, and a wake that finds it set is skipped with `reason=in flight`. That is
the debounce's own semantics — a skipped wake loses nothing, the agent's next
inbox read takes every message — extended to the window the write is still
running, which the debounce alone cannot see now that two wakes can run at once.

### 4. Server: the held capture carries the input modes

`heldCapture` writes `Repaint()` followed by `modes.Modes()` in the same Synth
frame. Order is `Repaint`'s own requirement (its comment: the preamble goes
after it, since it restores DECOM/DECAWM that the repaint clears).
`loadHeldCapture` → `loadScreen` already feeds a Synth frame into the tracker
and the grid, so the reading side needs no change and the file format does
not change. `Modes()` rather than `Preamble()` because the repaint already
carries DECTCEM and the screen selection, and a second `?1049l`/`?1049h` fed
to the grid would enter the alternate screen again over the repainted content.

A capture written by an older server simply lacks the modes, as today.

## Decisions

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | A PTY wake is written as `ESC[200~` marker `ESC[201~` `\r` in one write when the agent has bracketed paste on | operator |
| D2 | Whether it is on is decided in the runner, from the PTY output it relays | operator |
| D3 | An agent without `?2004h` keeps today's split write | this spec |
| D4 | `WakeStdin` runs off the receive loop; one wake in flight per task, a second is skipped like a debounced one | this spec — proposed to ride with D1, not separately confirmed |
| D5 | The held capture carries the tracker's input modes after the repaint | operator |
| D6 | The mode tracker moves to a shared package `termmode` used by server and runner | this spec — instead of a second parser in the runner |
| D7 | The capture carries `Modes()`, not `Preamble()` | this spec |

## Not in this change

- Topic eviction that ignores unread subscribers (`--agentboard-ttl`): a missed
  wake still loses the message after 30 minutes. Separate decision — it trades
  against `MaxTopics` memory.
- Why claude drops the second `\r` (Problem 1). Moot once the first one cannot
  become a newline.

## Verification

- Unit: the moved tracker tests pass in `termmode`; `Mode` and `Modes`;
  `ptyWake` writes one bracketed write with the mode on and text, then `\r`
  with it off; two concurrent `WakeStdin` calls write once; `heldCapture` fed
  `?2004h` round-trips through `loadHeldCapture` into a preamble containing it.
- Dummy harness, new runner, real claude: the `kill -STOP` reproduction of
  Problem 1 submits the wake on the first try.
- Dummy harness, server restarted deliberately with a session whose program
  set `?2004h` once: after re-adoption `session snapshot --raw` begins with a
  preamble containing `?2004h`.
