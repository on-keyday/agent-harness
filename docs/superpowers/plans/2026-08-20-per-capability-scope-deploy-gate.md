# Deploy gate result — per-capability scope

**Verdict: PASS.** Run 2026-08-20 on a dummy harness (`--name scope-e2e`),
both ends built from this branch at `be9a936`.

The gate exists because nothing in the unit or integration suites proves the
thing that matters after a wire break: that there is still a way IN. Those run
in-process or against a fake agent; neither exercises a real client opening a
real session against a real server, which is the path that fails by hanging.

## What was proven

**Entry, by keystroke rather than by rendering** — a session that draws but
accepts no input looks identical in a screenshot.

| step | check | result |
|---|---|---|
| 3 | CLI `session new -d`, then `session exec` echoing a nonce, matched in `session snapshot` | PASS — `NONCE_HITS=2` |
| 4 | dummy server killed and relaunched on the same port/PSK/data-dir; session lands `Failed err="runner_disconnected"`; resumed with the rebuilt client; nonce echoed again | PASS — task returned to `Detached`, `RESUME_NONCE_HITS=2` |
| 5 | `harness-tui` run inside a hosted session, driven by keystrokes, opening a session of its own | PASS — interactive count 5 → 6, and the new row reads `from=tui` |

Step 4 is the one that would have caught the historical fleet wipe: recovery
travels on `SubmitRequest` / `OpenInteractiveRequest`, the two formats this
change breaks.

## Also run

`scripts/wire-skew-check.sh` → PASS. NEW runner against OLD server is accepted
(RunnerHello is untouched by this change), and the runner re-registers on its
own after the server is upgraded. Not the fleet-wipe shape.

That script had to be repaired first: it exited 2 (setup error) on every run
here because Go stamps VCS metadata into builds and doing so in the detached
worktree it creates fails with `exit status 128`. It could neither pass nor
fail. Fixed with `-buildvcs=false` (commit `be9a936`).

## Observed, not caused by this change

Immediately after the server restart the TUI kept a stale runner registration
and answered `ambiguous_runner` while the CLI already saw one runner. Pinning
with `--host` worked. This is client-side snapshot staleness across a restart,
unrelated to scope; recorded so it is not rediscovered as a symptom of the
deploy.

## Not covered

The WebUI was not exercised in a browser during this run. Its wasm builds
clean (`make wasm-check`) and its bridge is unit-covered, but the dialogs added
in `32c2417` have not been looked at on a screen, at either width.
