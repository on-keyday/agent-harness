#!/usr/bin/env bash
# e2e-oneshot-progress.sh — prove a one-shot run's progress is visible WHILE
# THE RUN IS STILL GOING, and that a codex-shaped agent does not hang waiting
# for stdin.
#
# WHY THIS EXISTS
#   A one-shot run is a `session new --oneshot` session: the runner runs the
#   profile's oneshot (print-mode) argv in a PTY, and the task ends when that
#   process exits. What an operator sees of the run while it goes is the
#   session's screen (`session snapshot`). An output path that delivered the
#   agent's writes only at exit would still show every line by the time the
#   task ended, so "the line appears somewhere" is not asserted. The
#   load-bearing assertion is a TIMESTAMP COMPARISON: a snapshot showing the
#   run's first event but not yet its last must be taken at least
#   PROGRESS_LEAD_S seconds BEFORE the task's terminal status event.
#
#   The second thing proved is a regression guard. While one-shot runs had a
#   stdin pipe, every codex-profile task hung until the runner's 30-minute
#   default timeout: the pipe never reached EOF and `codex exec` blocked
#   reading it. The codex-shaped fake here reads stdin to EOF whenever stdin is
#   NOT a terminal, so it reproduces that hang if a one-shot run's stdin ever
#   stops being the session's PTY — and its deadline is measured in seconds,
#   so a hang cannot pass.
#
# WHAT IS ASSERTED
#   claude-shaped profile (default, stream-json oneshot argv)
#     1. a snapshot shows the run's first event and not yet its result event,
#        taken >= PROGRESS_LEAD_S before the terminal status event
#                                                          <- the whole point
#     2. a snapshot taken while the run is live shows its tool_use event: the
#        screen carries the agent's output as emitted (no decoding on this
#        path, so the JSON itself)
#   codex-shaped profile
#     3. terminal status reached within CODEX_DEADLINE_S seconds of the open
#     4. the agent got past its stdin check and saw a terminal there (it writes
#        "stdin=tty" to a marker file; a crash would also "reach a terminal
#        status quickly", and a pipe would read "stdin=pipe")
#   shell-sandbox profile (`{args} -c {prompt}`, the bash preset's argv)
#     5. the prompt runs as a shell command and its output is on the screen
#        while the run is live
#     6. a prompt of `exit 3` ends the task Failed: the exit code is the run's
#   all three profiles
#     7. terminal status is Succeeded (not Failed/Cancelled)
#
#   Until A3 (docs/superpowers/plans/2026-11-15-a3-retire-oneshot.md) this
#   script asserted decoded task-log lines (`logs`): the one-shot task kind
#   decoded an agent's event stream into the task log, and a profile with no
#   log format had to come through byte-identical. Both are gone with that
#   kind: a session's PTY shows what the agent printed, and the line
#   discipline turns "\n" into "\r\n" on the way, so byte identity is not a
#   property of this path.
#
# EXIT CODES
#   0 = every assertion passed
#   1 = an assertion FAILED
#   2 = SETUP ERROR (build failed, server or runner never came up, an open was
#       rejected). A setup error is never reported as a pass: the sibling
#       wire-skew-check.sh shipped a first version that passed on everything
#       because harness-server refuses to start without webui/static/main.wasm
#       and the runner therefore only ever saw "connection refused". `make
#       build` below regenerates that artifact, and the server is required to
#       actually listen before anything is opened.
#
# USAGE
#   scripts/e2e-oneshot-progress.sh
#   E2E_PORT=<port> scripts/e2e-oneshot-progress.sh   # pin the loopback port
set -uo pipefail

# The harness injects HARNESS_* into task shells; left set, they would point
# this script's runner and cli at the REAL server (and make harness-cli
# authenticate as an agent instead of an operator).
for v in $(env | grep -oE '^HARNESS_[A-Z_]+'); do unset "$v"; done
# Stable byte semantics for grep and a '.' decimal point in EPOCHREALTIME.
export LC_ALL=C

REPO="$(cd "$(dirname "$0")/.." && pwd)"; cd "$REPO" || exit 2
BIN="$REPO/bin"

PSK="e2e-oneshot-progress"   # the operator secret harness-cli proves
PROGRESS_LEAD_S=1     # visible progress must precede terminal by this much
CLAUDE_DEADLINE_S=30  # hard per-task timeout
CODEX_DEADLINE_S=20   # the guarded bug is a 30-MINUTE hang; keep this in seconds
SHELL_DEADLINE_S=30
AGENT_STEP_S=3        # pause between the fake agents' writes
# Wide enough that no event line of the fakes wraps, so a grep for one finds it.
COLS=200

TMP=""; SERVER_PID=""; RUNNER_PID=""; WATCH_PID=""; SNAP_PID=""
FAILURES=0

cleanup() {
  [ -n "$SNAP_PID" ]   && kill "$SNAP_PID"   2>/dev/null
  [ -n "$WATCH_PID" ]  && kill "$WATCH_PID"  2>/dev/null
  [ -n "$RUNNER_PID" ] && kill "$RUNNER_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  sleep 0.3
  [ -n "$SNAP_PID" ]   && kill -9 "$SNAP_PID"   2>/dev/null
  [ -n "$WATCH_PID" ]  && kill -9 "$WATCH_PID"  2>/dev/null
  [ -n "$RUNNER_PID" ] && kill -9 "$RUNNER_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill -9 "$SERVER_PID" 2>/dev/null
  [ -n "$TMP" ] && rm -rf "$TMP"
  return 0
}
trap cleanup EXIT
# Route ^C / SIGTERM through a normal exit so the EXIT trap above still runs.
# A leaked server or runner from an interrupted run poisons the next one.
trap 'exit 130' INT
trap 'exit 143' TERM

setup_err() { echo; echo "e2e-oneshot-progress: SETUP ERROR — $*" >&2; exit 2; }
pass() { echo "  PASS — $*"; }
fail() { echo "  FAIL — $*"; FAILURES=$((FAILURES + 1)); }

# now returns the current time as float seconds (bash 5 builtin, no fork).
now() { printf '%s\n' "$EPOCHREALTIME"; }
# elapsed_ge A B N -> true when B - A >= N (bash has no float arithmetic).
elapsed_ge() { awk -v a="$1" -v b="$2" -v n="$3" 'BEGIN{exit !((b-a)>=n)}'; }
# delta A B -> B - A, 2 decimals.
delta() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f", b-a}'; }

# ---------------------------------------------------------------- build ----
# `make build` (not `go build`) because harness-server embeds
# webui/static/main.wasm and refuses to start without it; the webui-build
# prerequisite regenerates it. bin/ is gitignored, so this leaves no trace.
echo "e2e-oneshot-progress: building this worktree's bin/ (make build)"
TMP="$(mktemp -d)" || setup_err "mktemp failed"
make build >"$TMP/build.log" 2>&1 || { tail -20 "$TMP/build.log" >&2; setup_err "make build failed"; }
for b in harness-server agent-runner harness-cli; do
  [ -x "$BIN/$b" ] || setup_err "$BIN/$b missing after make build"
done

# ------------------------------------------------------- fake agents -------
# Shell scripts, so the whole run is hermetic: no real agent CLI, no API keys,
# no network. Their argv templates below mirror scripts/agent_presets.py so the
# invocation shape under test is the one --agents actually produces.
mkdir -p "$TMP/agents" "$TMP/repo" "$TMP/data" || setup_err "mkdir failed"
( cd "$TMP/repo" && git init -q && git commit -q --allow-empty -m init ) >/dev/null 2>&1

# stamp.sh prefixes each line it reads with its LOCAL arrival time, so the
# terminal status time and the snapshot times compared by assertion 1 come from
# the same clock on the same host.
cat >"$TMP/agents/stamp.sh" <<'STAMP'
#!/usr/bin/env bash
while IFS= read -r line; do printf '%s %s\n' "$EPOCHREALTIME" "$line"; done
STAMP

# claude-shaped: emits stream-json events with real pauses between them, so
# "progress was visible before the run ended" is a measurable property and not
# an artifact of everything arriving at exit.
cat >"$TMP/agents/fake-claude.sh" <<CLAUDE
#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"e2e-fake-session"}'
sleep $AGENT_STEP_S
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}'
sleep $AGENT_STEP_S
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","duration_ms":4200,"total_cost_usd":0.01}'
CLAUDE

# codex-shaped: reads a non-terminal stdin to EOF before doing anything — the
# shape of the original hang if a one-shot run's stdin is ever a never-EOF pipe
# again. It records what its stdin was in the (no-worktree) repo, its cwd.
cat >"$TMP/agents/fake-codex.sh" <<CODEX
#!/bin/sh
if [ -t 0 ]; then stdin=tty; else cat > /dev/null; stdin=pipe; fi
printf 'stdin=%s\n' "\$stdin" > codex-probe.marker
printf '%s\n' '{"type":"thread.started","thread_id":"e2e-fake-thread"}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":20}}'
CODEX

# shell-sandbox shaped: mirrors the `bash` preset's `{args} -c {prompt}` argv,
# i.e. run the prompt as a shell command.
cat >"$TMP/agents/fake-shell.sh" <<SHELLA
#!/bin/sh
sh "\$@"
exit \$?
SHELLA
chmod +x "$TMP/agents"/*.sh || setup_err "chmod failed"

SHELL_PROMPT="printf 'shell-probe-out\n'; sleep $AGENT_STEP_S"

# ------------------------------------------------------------ server -------
# port_in_use probes by connect. fd 3 is opened in a SUBSHELL, so there is
# nothing for this shell to close afterwards.
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# Picking a port then binding it is racy by construction; a lost race surfaces
# as "server never listened" (setup error, exit 2), never as a pass.
pick_port() {
  local p
  for _ in $(seq 1 50); do
    p=$((20000 + RANDOM % 20000))
    if ! port_in_use "$p"; then printf '%s\n' "$p"; return 0; fi
  done
  return 1
}
PORT="${E2E_PORT:-$(pick_port)}"
[ -n "$PORT" ] || setup_err "could not find a free loopback port"
CID="ws:127.0.0.1:${PORT}-*"

RUNNER_BIN="$BIN/agent-runner"; SEED_LABEL="e2e"
# The runner's key is enrolled before the server starts: the server reads
# <data-dir>/runner-keys.json at startup and refuses an unknown key.
RUNNER_KEY="$TMP/runner-key"
mkdir -p "$TMP/data"
PUB=$("$RUNNER_BIN" --print-pubkey --runner-key-file "$RUNNER_KEY") || setup_err "agent-runner --print-pubkey failed"
printf '[{"key":"%s","label":"%s","enrolled_at":"%s","enrolled_by":"operator"}]\n' \
  "$PUB" "$SEED_LABEL" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$TMP/data/runner-keys.json"

echo "e2e-oneshot-progress: starting server on loopback port $PORT"
"$BIN/harness-server" --listen "127.0.0.1:${PORT}" --operator-psk "$PSK" \
  --data-dir "$TMP/data" >"$TMP/server.log" 2>&1 &
SERVER_PID=$!
listening=0
for _ in $(seq 1 40); do
  kill -0 "$SERVER_PID" 2>/dev/null || break
  if port_in_use "$PORT"; then listening=1; break; fi
  sleep 0.25
done
# A server that did not start is a SETUP ERROR, never a pass: with nothing
# listening the runner only sees "connection refused", and "the codex task
# didn't hang" would be trivially true of an empty run.
[ "$listening" = 1 ] || { head -20 "$TMP/server.log" >&2; setup_err "server never listened on $PORT"; }

# ------------------------------------------------------------ runner -------
# Three profiles in one runner, in the flag shape scripts/agent_presets.py
# emits for `--agents claude,codex,bash`, with the bins pointed at the fakes:
#   default profile  = claude-shaped
#   extra "codex"    = codex-shaped
#   extra "shell"    = shell-sandbox
# The default profile's NAME is the basename of --agent-bin (see
# cmd/agent-runner/main.go), hence "fake-claude.sh". No log format is given: a
# session's PTY carries the agent's output undecoded, so it would change
# nothing here.
#
# agent-runner is started directly rather than through scripts/runner.sh: that
# wrapper allocates a persistent slot under bin/.run/ (pid / log / shutdown
# sentinel files) which this script would then have to clean up, and a crash
# would leave a registered slot behind. Same choice, same reason, as the
# sibling scripts/wire-skew-check.sh.
PROFILES_JSON=$(cat <<JSON
[{"name":"codex","bin":"$TMP/agents/fake-codex.sh",
  "oneshotArgv":["exec","--json","{args}","{prompt}"],
  "resumeOneshotArgv":["exec","--json","resume","--last","{args}","{prompt}"],
  "resumeInteractiveArgv":["resume","--last","{args}"]},
 {"name":"shell","bin":"$TMP/agents/fake-shell.sh",
  "oneshotArgv":["{args}","-c","{prompt}"],
  "resumeOneshotArgv":["{args}","-c","{prompt}"],
  "resumeInteractiveArgv":["{args}"]}]
JSON
)
echo "e2e-oneshot-progress: starting runner (profiles: fake-claude.sh, codex, shell)"
"$BIN/agent-runner" \
  --server-cid "$CID" --runner-key-file "$RUNNER_KEY" --roots "$TMP/repo" --no-worktree \
  --max-tasks 4 \
  --agent-bin "$TMP/agents/fake-claude.sh" \
  --agent-oneshot-argv '--output-format stream-json --verbose {args} -p {prompt}' \
  --agent-resume-oneshot-argv '--output-format stream-json --verbose {args} --continue -p {prompt}' \
  --agent-resume-interactive-argv '{args} --continue' \
  --agent-profiles "$PROFILES_JSON" >"$TMP/runner.log" 2>&1 &
RUNNER_PID=$!

export HARNESS_OPERATOR_PSK="$PSK"
registered=0
for _ in $(seq 1 40); do
  kill -0 "$RUNNER_PID" 2>/dev/null || break
  if "$BIN/harness-cli" --server-cid "$CID" ls 2>/dev/null \
       | grep -q "agent=fake-claude.sh,codex,shell"; then registered=1; break; fi
  sleep 0.25
done
[ "$registered" = 1 ] || { tail -20 "$TMP/runner.log" >&2; setup_err "runner never registered all three profiles"; }

# --------------------------------------------------- task status stream ----
# `watch` streams task status events over one long-lived connection. Polling
# `ls` instead would push the observed terminal time later by up to a poll
# interval plus a fresh handshake each time — exactly the direction that
# inflates the gap assertion 1 measures.
"$BIN/harness-cli" --server-cid "$CID" watch > >("$TMP/agents/stamp.sh" >"$TMP/watch.ts") 2>"$TMP/watch.err" &
WATCH_PID=$!
sleep 0.5

# ------------------------------------------------------------ probes -------
# open_probe <agent-profile> <prompt> -> echoes "<task-id> <open-ts>". Named,
# never left empty: with three profiles on one runner an open with no --agent
# answers ambiguous_runner instead of picking the default.
open_probe() {
  local agent="$1" prompt="$2" out id ts
  ts="$(now)"
  out="$("$BIN/harness-cli" --server-cid "$CID" session new -d --oneshot --cols "$COLS" \
           --repo "$TMP/repo" --agent "$agent" --prompt "$prompt" 2>&1)"
  id="$(printf '%s\n' "$out" | tr -d '\r' | grep -E '^[0-9a-f]{32}$' | head -1)"
  [ -n "$id" ] || { echo "session new --oneshot failed: $out" >&2; return 1; }
  printf '%s %s\n' "$id" "$ts"
}

# snapshot_loop <task-id> <dir> — snapshot the session's screen until the
# session is gone, one file per snapshot named by the local time it RETURNED
# (so a snapshot's content was on screen no later than its name says). Run in
# the background and killed once the run's terminal status arrives; a failed
# snapshot (the session already ended) stops it sooner.
snapshot_loop() {
  local id="$1" dir="$2" ts
  mkdir -p "$dir"
  while :; do
    timeout 10 "$BIN/harness-cli" --server-cid "$CID" session snapshot --settle-ms 200 "$id" \
      >"$dir/cur" 2>/dev/null || break
    ts="$(now)"
    mv "$dir/cur" "$dir/$ts"
    sleep 0.2
  done
  rm -f "$dir/cur"
}

# first_snapshot_where <dir> <ere-present> [<ere-absent>] -> echoes the time
# of the earliest snapshot whose screen matches the first pattern and not the
# second, or nothing.
first_snapshot_where() {
  local dir="$1" want="$2" not="${3:-}" f
  for f in $(ls "$dir" 2>/dev/null | grep -E '^[0-9.]+$' | sort -n); do
    grep -qE "$want" "$dir/$f" || continue
    [ -n "$not" ] && grep -qE "$not" "$dir/$f" && continue
    printf '%s\n' "$f"; return 0
  done
}

# wait_terminal <task-id> <deadline-s> -> echoes the stamped watch line, or
# nothing (and returns 1) if no terminal event arrived in time.
wait_terminal() {
  local id="$1" deadline="$2" prefix line start
  prefix="${id:0:12}"   # cli/watch.go prints %x of TaskId.Id[:6]
  start="$(now)"
  while :; do
    line="$(grep -E "task ${prefix} .*status=(Succeeded|Failed|Cancelled)" "$TMP/watch.ts" 2>/dev/null | head -1)"
    [ -n "$line" ] && { printf '%s\n' "$line"; return 0; }
    elapsed_ge "$start" "$(now)" "$deadline" && return 1
    sleep 0.2
  done
}

status_of() { printf '%s\n' "$1" | grep -oE 'status=[A-Za-z]+' | head -1 | cut -d= -f2; }
ts_of()     { printf '%s\n' "$1" | awk '{print $1}'; }

# expect_status <label> <term-rc> <term-line> <open-ts> <want> <deadline-s>
expect_status() {
  local label="$1" rc="$2" line="$3" open_ts="$4" want="$5" deadline="$6" st
  if [ "$rc" != 0 ]; then
    fail "$label never reached a terminal status within ${deadline}s"
    return 1
  fi
  st="$(status_of "$line")"
  echo "  ($label reached $st $(delta "$open_ts" "$(ts_of "$line")")s after the open)"
  if [ "$st" = "$want" ]; then
    pass "$label finished $want"
  else
    fail "$label finished $st (expected $want): $line"
  fi
}

echo
echo "e2e-oneshot-progress: [1/3] claude-shaped profile (stream-json oneshot argv)"
read -r CLAUDE_ID CLAUDE_OPEN_TS < <(open_probe fake-claude.sh "e2e claude probe") \
  || setup_err "session new --oneshot on the default (claude-shaped) profile failed"
snapshot_loop "$CLAUDE_ID" "$TMP/claude.snap" &
SNAP_PID=$!
CLAUDE_TERM_LINE="$(wait_terminal "$CLAUDE_ID" "$CLAUDE_DEADLINE_S")"
CLAUDE_TERM_RC=$?
kill "$SNAP_PID" 2>/dev/null; wait "$SNAP_PID" 2>/dev/null; SNAP_PID=""

expect_status "claude probe" "$CLAUDE_TERM_RC" "$CLAUDE_TERM_LINE" "$CLAUDE_OPEN_TS" Succeeded "$CLAUDE_DEADLINE_S"
if [ "$CLAUDE_TERM_RC" = 0 ]; then
  CLAUDE_TERM_TS="$(ts_of "$CLAUDE_TERM_LINE")"
  # ASSERTION 1 — the whole point.
  MID_TS="$(first_snapshot_where "$TMP/claude.snap" '"subtype":"init"' '"type":"result"')"
  if [ -z "$MID_TS" ]; then
    fail "no snapshot ever showed the run's first event without its result; snapshots taken: $(ls "$TMP/claude.snap" | wc -l), the last was:
$(f=$(ls "$TMP/claude.snap" | sort -n | tail -1); [ -n "$f" ] && sed -n '1,8p' "$TMP/claude.snap/$f")"
  elif elapsed_ge "$MID_TS" "$CLAUDE_TERM_TS" "$PROGRESS_LEAD_S"; then
    pass "run in progress visible $(delta "$MID_TS" "$CLAUDE_TERM_TS")s BEFORE terminal status (need >= ${PROGRESS_LEAD_S}s)"
  else
    fail "run in progress visible only $(delta "$MID_TS" "$CLAUDE_TERM_TS")s before terminal status (need >= ${PROGRESS_LEAD_S}s) — output is not reaching the screen until the run ends"
  fi
fi
# ASSERTION 2
if [ -n "$(first_snapshot_where "$TMP/claude.snap" '"type":"tool_use","name":"Bash"')" ]; then
  pass "the tool_use event reached the screen while the run was live, as emitted"
else
  fail "no live snapshot showed the tool_use event"
fi

echo
echo "e2e-oneshot-progress: [2/3] codex-shaped profile (reads a non-terminal stdin to EOF)"
read -r CODEX_ID CODEX_OPEN_TS < <(open_probe codex "e2e codex probe") \
  || setup_err "session new --oneshot on the codex profile failed"
CODEX_TERM_LINE="$(wait_terminal "$CODEX_ID" "$CODEX_DEADLINE_S")"
CODEX_TERM_RC=$?
# ASSERTION 3 — the guarded bug is a 30-minute hang, so this deadline is
# deliberately in seconds: a generous one would make the guard useless.
if [ "$CODEX_TERM_RC" != 0 ]; then
  fail "codex probe did NOT reach a terminal status within ${CODEX_DEADLINE_S}s — it is hanging, most likely on a stdin that is not the session's terminal"
else
  expect_status "codex probe" 0 "$CODEX_TERM_LINE" "$CODEX_OPEN_TS" Succeeded "$CODEX_DEADLINE_S"
fi
# ASSERTION 4
MARKER="$(cat "$TMP/repo/codex-probe.marker" 2>/dev/null)"
if [ "$MARKER" = "stdin=tty" ]; then
  pass "the codex-shaped agent got past its stdin check, with the session's terminal as stdin"
else
  fail "the codex-shaped agent's marker reads '${MARKER:-<missing>}' (want stdin=tty)"
fi

echo
echo "e2e-oneshot-progress: [3/3] shell-sandbox profile ({args} -c {prompt})"
read -r SHELL_ID SHELL_OPEN_TS < <(open_probe shell "$SHELL_PROMPT") \
  || setup_err "session new --oneshot on the shell profile failed"
snapshot_loop "$SHELL_ID" "$TMP/shell.snap" &
SNAP_PID=$!
SHELL_TERM_LINE="$(wait_terminal "$SHELL_ID" "$SHELL_DEADLINE_S")"
SHELL_TERM_RC=$?
kill "$SNAP_PID" 2>/dev/null; wait "$SNAP_PID" 2>/dev/null; SNAP_PID=""
expect_status "shell probe" "$SHELL_TERM_RC" "$SHELL_TERM_LINE" "$SHELL_OPEN_TS" Succeeded "$SHELL_DEADLINE_S"
# ASSERTION 5
SHELL_SEEN="$(first_snapshot_where "$TMP/shell.snap" '^shell-probe-out')"
if [ -n "$SHELL_SEEN" ] && [ "$SHELL_TERM_RC" = 0 ] \
   && elapsed_ge "$SHELL_SEEN" "$(ts_of "$SHELL_TERM_LINE")" "$PROGRESS_LEAD_S"; then
  pass "the prompt ran as a shell command; its output was on the screen $(delta "$SHELL_SEEN" "$(ts_of "$SHELL_TERM_LINE")")s before the run ended"
else
  fail "the shell prompt's output was not on a live snapshot >= ${PROGRESS_LEAD_S}s before the run ended (seen at: ${SHELL_SEEN:-never})"
fi
# ASSERTION 6
read -r EXIT_ID EXIT_OPEN_TS < <(open_probe shell "exit 3") \
  || setup_err "session new --oneshot on the shell profile (exit 3) failed"
EXIT_TERM_LINE="$(wait_terminal "$EXIT_ID" "$SHELL_DEADLINE_S")"
EXIT_TERM_RC=$?
expect_status "exit-3 probe" "$EXIT_TERM_RC" "$EXIT_TERM_LINE" "$EXIT_OPEN_TS" Failed "$SHELL_DEADLINE_S"

echo
if [ "$FAILURES" != 0 ]; then
  echo "e2e-oneshot-progress: FAIL — $FAILURES assertion(s) failed"
  echo "--- runner log (tail) ---"; tail -15 "$TMP/runner.log" 2>/dev/null
  exit 1
fi
echo "e2e-oneshot-progress: PASS — a one-shot run's output is on its screen before the run ends,"
echo "  a codex-shaped agent does not wait on stdin, and the exit code ends the task."
exit 0
