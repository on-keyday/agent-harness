#!/usr/bin/env bash
# e2e-oneshot-progress.sh — prove a oneshot task's progress reaches the log
# topic WHILE THE TASK IS STILL RUNNING, and that a codex-shaped agent does
# not hang waiting for stdin.
#
# WHY THIS EXISTS
#   runner/agentlog decodes an agent's structured event stream into readable
#   lines and Process.Run applies it to stdout. Every part of that is unit
#   tested — and none of those tests can tell the difference between "the line
#   was rendered" and "the line reached an operator before the task finished".
#   A decoder that works perfectly but whose output is only flushed at exit
#   passes every unit test and delivers nothing. So the load-bearing assertion
#   here is a TIMESTAMP COMPARISON: a rendered progress line must be observed
#   at least PROGRESS_LEAD_S seconds BEFORE the task's terminal status event.
#   "the line appears somewhere in the output" is not asserted, because that is
#   also true of the broken behaviour.
#
#   The second thing proved is a regression guard. Before the oneshot stdin
#   pipe was removed, every codex-profile task hung until the runner's
#   30-minute default timeout: handleAssign attached a stdin pipe that never
#   reached EOF and `codex exec` blocks reading it. The codex probe here runs
#   `cat > /dev/null` first, so it reproduces that hang if the pipe ever comes
#   back — and its deadline is measured in seconds, so a hang cannot pass.
#
# WHAT IS ASSERTED
#   claude-shaped profile (--agent-log-format claude-stream-json)
#     1. a rendered tool line ("[out]→ ...") observed >= PROGRESS_LEAD_S before
#        the terminal status event                       <- the whole point
#     2. the rendered session-start line is present
#     3. the final assistant text is present as "[out]done"
#     4. no log line contains raw JSON ('"type":"')
#   codex-shaped profile (logFormat codex-jsonl)
#     5. terminal status reached within CODEX_DEADLINE_S seconds of submit
#     6. its decoded tool + text lines are present (proves the agent actually
#        got past `cat > /dev/null` rather than dying early — a crash would
#        also "reach a terminal status quickly")
#   shell-sandbox profile (no logFormat)
#     7. the task log is byte-identical to what the agent printed, "[out]"-
#        prefixed, including a CRLF line and an unterminated final line
#   all three
#     8. terminal status is Succeeded (not Failed/Cancelled)
#
# EXIT CODES
#   0 = every assertion passed
#   1 = an assertion FAILED
#   2 = SETUP ERROR (build failed, server or runner never came up, submit
#       rejected). A setup error is never reported as a pass: the sibling
#       wire-skew-check.sh shipped a first version that passed on everything
#       because harness-server refuses to start without webui/static/main.wasm
#       and the runner therefore only ever saw "connection refused". `make
#       build` below regenerates that artifact, and the server is required to
#       actually listen before anything is submitted.
#
# THIS SCRIPT MUST BE ABLE TO FAIL
#   Negative control used while writing it: force Run's stdout dispatch down
#   the raw path (make the `agentlog.HasDecoder(p.LogFormat)` branch in
#   runner/process.go unreachable), rebuild, re-run. Assertions 1-4 go red with
#   raw JSON in the log while the codex and shell probes stay green. Do NOT
#   stash the whole of runner/process.go for this: that file also carries the
#   stdin-pipe deletion, so stashing it makes the codex probe hang and the run
#   goes red for a reason that says nothing about the decoder.
#
# USAGE
#   scripts/e2e-oneshot-progress.sh
#   E2E_PORT=<port> scripts/e2e-oneshot-progress.sh   # pin the loopback port
set -uo pipefail

# The harness injects HARNESS_* into task shells; left set, they would point
# this script's runner and cli at the REAL server (and make harness-cli
# authenticate as an agent instead of an operator).
for v in $(env | grep -oE '^HARNESS_[A-Z_]+'); do unset "$v"; done
# Stable byte semantics for grep/cmp and a '.' decimal point in EPOCHREALTIME.
export LC_ALL=C

REPO="$(cd "$(dirname "$0")/.." && pwd)"; cd "$REPO" || exit 2
BIN="$REPO/bin"

PSK="e2e-oneshot-progress"
PROGRESS_LEAD_S=1     # rendered progress must precede terminal by this much
CLAUDE_DEADLINE_S=30  # hard per-task timeout (brief: 30s)
CODEX_DEADLINE_S=20   # the guarded bug is a 30-MINUTE hang; keep this in seconds
SHELL_DEADLINE_S=30
AGENT_STEP_S=3        # pause between the fake claude's writes

TMP=""; SERVER_PID=""; RUNNER_PID=""; WATCH_PID=""; FOLLOW_PID=""
FAILURES=0

cleanup() {
  [ -n "$FOLLOW_PID" ] && kill "$FOLLOW_PID" 2>/dev/null
  [ -n "$WATCH_PID" ]  && kill "$WATCH_PID"  2>/dev/null
  [ -n "$RUNNER_PID" ] && kill "$RUNNER_PID" 2>/dev/null
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  sleep 0.3
  [ -n "$FOLLOW_PID" ] && kill -9 "$FOLLOW_PID" 2>/dev/null
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
#
# EVERY fake agent sleeps briefly after its last write before exiting, and that
# sleep is load-bearing — do not delete it. Process.Run reads stdout/stderr from
# cmd.StdoutPipe()/cmd.StderrPipe() and calls cmd.Wait() BEFORE waiting for its
# scanner goroutines, which os/exec documents as incorrect: "Wait will close the
# pipe after seeing the command exit ... it is thus incorrect to call Wait
# before all reads from the pipe have completed." Whatever is still sitting in
# the pipe when the child exits is discarded, so an agent that writes several
# lines and exits immediately loses a variable-length tail of its own log.
# Measured on this repo: a 4-line burst followed by an immediate exit lands
# 33/66/99/132 bytes of 132 across runs. That defect is NOT what this script
# guards — it reproduces identically on the pre-plan runner (540b6df), i.e. it
# predates the progress-streaming work — and letting it into the probes would
# only make every assertion below intermittent. The sleep removes the race
# outright (the scanner has drained the pipe long before the child exits)
# rather than making it less likely.
EXIT_SETTLE_S=0.5
mkdir -p "$TMP/agents" "$TMP/repo" "$TMP/data" || setup_err "mkdir failed"
( cd "$TMP/repo" && git init -q && git commit -q --allow-empty -m init ) >/dev/null 2>&1

# stamp.sh prefixes each line it reads with its LOCAL arrival time. Both the
# log tail and the task-status stream go through it, so the two timestamps
# compared by assertion 1 come from the same clock on the same host.
cat >"$TMP/agents/stamp.sh" <<'STAMP'
#!/usr/bin/env bash
while IFS= read -r line; do printf '%s %s\n' "$EPOCHREALTIME" "$line"; done
STAMP

# claude-shaped: emits stream-json events with real pauses between them, so
# "rendered line reached the log before the task finished" is a measurable
# property and not an artifact of everything arriving at exit.
cat >"$TMP/agents/fake-claude.sh" <<CLAUDE
#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"e2e-fake-session"}'
sleep $AGENT_STEP_S
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}'
sleep $AGENT_STEP_S
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","duration_ms":4200,"total_cost_usd":0.01}'
sleep $EXIT_SETTLE_S
CLAUDE

# codex-shaped: `codex exec` reads stdin to EOF before doing anything. The
# leading `cat > /dev/null` reproduces the original hang exactly if a
# never-EOF stdin pipe is ever attached to oneshot tasks again.
cat >"$TMP/agents/fake-codex.sh" <<CODEX
#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"e2e-fake-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"command_execution","command":"echo hi","aggregated_output":"hi","exit_code":0}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"codex done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":20}}'
sleep $EXIT_SETTLE_S
CODEX

# shell-sandbox shaped: mirrors the `bash` preset's `{args} -c {prompt}` argv,
# i.e. run the prompt as a shell command. This profile declares no logFormat,
# so the runner must forward its stdout byte-for-byte.
cat >"$TMP/agents/fake-shell.sh" <<SHELLA
#!/bin/sh
sh "\$@"
rc=\$?
sleep $EXIT_SETTLE_S
exit \$rc
SHELLA
chmod +x "$TMP/agents"/*.sh || setup_err "chmod failed"

# The shell probe's prompt and the exact bytes its output must produce in the
# log. A CRLF line and an unterminated final line are deliberate: the raw path
# must preserve both, which a decode/render round-trip cannot.
SHELL_PROMPT="printf 'plain\nCRLF-line\r\nno-final-newline'"
printf '[out]plain\n[out]CRLF-line\r\n[out]no-final-newline' >"$TMP/shell-expected"

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

echo "e2e-oneshot-progress: starting server on loopback port $PORT"
"$BIN/harness-server" --listen "127.0.0.1:${PORT}" --psk "$PSK" --operator-psk "$PSK" \
  --data-dir "$TMP/data" >"$TMP/server.log" 2>&1 &
SERVER_PID=$!
listening=0
for _ in $(seq 1 40); do
  kill -0 "$SERVER_PID" 2>/dev/null || break
  if port_in_use "$PORT"; then listening=1; break; fi
  sleep 0.25
done
# A server that did not start is a SETUP ERROR, never a pass: with nothing
# listening the runner only sees "connection refused", every task stays queued,
# and "the codex task didn't hang" would be trivially true of an empty run.
[ "$listening" = 1 ] || { head -20 "$TMP/server.log" >&2; setup_err "server never listened on $PORT"; }

# ------------------------------------------------------------ runner -------
# Three profiles in one runner, in the flag shape scripts/agent_presets.py
# emits for `--agents claude,codex,bash`, with the bins pointed at the fakes:
#   default profile  = claude-shaped, --agent-log-format claude-stream-json
#   extra "codex"    = codex-shaped,  logFormat codex-jsonl
#   extra "shell"    = shell-sandbox, logFormat "" (raw passthrough)
# The default profile's NAME is the basename of --agent-bin (see
# cmd/agent-runner/main.go), hence "fake-claude.sh".
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
  "resumeInteractiveArgv":["resume","--last","{args}"],
  "logFormat":"codex-jsonl"},
 {"name":"shell","bin":"$TMP/agents/fake-shell.sh",
  "oneshotArgv":["{args}","-c","{prompt}"],
  "resumeOneshotArgv":["{args}","-c","{prompt}"],
  "resumeInteractiveArgv":["{args}"],
  "logFormat":""}]
JSON
)
echo "e2e-oneshot-progress: starting runner (profiles: fake-claude.sh, codex, shell)"
"$BIN/agent-runner" \
  --server-cid "$CID" --psk "$PSK" --roots "$TMP/repo" --no-worktree \
  --agent-bin "$TMP/agents/fake-claude.sh" \
  --agent-oneshot-argv '--output-format stream-json --verbose {args} -p {prompt}' \
  --agent-resume-oneshot-argv '--output-format stream-json --verbose {args} --continue -p {prompt}' \
  --agent-resume-interactive-argv '{args} --continue' \
  --agent-log-format claude-stream-json \
  --agent-profiles "$PROFILES_JSON" >"$TMP/runner.log" 2>&1 &
RUNNER_PID=$!

export HARNESS_PSK="$PSK"   # harness-cli falls back to this for the operator binder
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
# submit_probe <agent-profile-or-empty> <prompt> -> echoes "<task-id> <submit-ts>"
submit_probe() {
  local agent="$1" prompt="$2" out id ts
  ts="$(now)"
  if [ -n "$agent" ]; then
    out="$("$BIN/harness-cli" --server-cid "$CID" submit --repo "$TMP/repo" --agent "$agent" --task "$prompt" 2>&1)"
  else
    out="$("$BIN/harness-cli" --server-cid "$CID" submit --repo "$TMP/repo" --task "$prompt" 2>&1)"
  fi
  id="$(printf '%s\n' "$out" | tr -d '\r' | grep -E '^[0-9a-f]{32}$' | head -1)"
  [ -n "$id" ] || { echo "submit failed: $out" >&2; return 1; }
  printf '%s %s\n' "$id" "$ts"
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

# dump_log_stable <task-id> <outfile> — the historical log via GetTaskLog, read
# until two consecutive reads agree. TaskFinished and the last log chunks are
# separate messages, so a single read right after the status event can race the
# final append.
dump_log_stable() {
  local id="$1" out="$2"
  "$BIN/harness-cli" --server-cid "$CID" logs "$id" >"$out" 2>/dev/null
  for _ in $(seq 1 10); do
    sleep 0.4
    "$BIN/harness-cli" --server-cid "$CID" logs "$id" >"$out.next" 2>/dev/null
    if cmp -s "$out" "$out.next"; then rm -f "$out.next"; return 0; fi
    mv "$out.next" "$out"
  done
  return 0
}

status_of() { printf '%s\n' "$1" | grep -oE 'status=[A-Za-z]+' | head -1 | cut -d= -f2; }
ts_of()     { printf '%s\n' "$1" | awk '{print $1}'; }

echo
echo "e2e-oneshot-progress: [1/3] claude-shaped profile (claude-stream-json)"
read -r CLAUDE_ID CLAUDE_SUBMIT_TS < <(submit_probe "" "e2e claude probe") \
  || setup_err "submit to the default (claude-shaped) profile failed"
# Tail the log topic live, timestamped on arrival. Content assertions use the
# persisted log instead (below) — this capture exists only to timestamp the
# FIRST arrival of a rendered progress line.
"$BIN/harness-cli" --server-cid "$CID" logs --follow "$CLAUDE_ID" \
  > >("$TMP/agents/stamp.sh" >"$TMP/claude.follow.ts") 2>"$TMP/claude.follow.err" &
FOLLOW_PID=$!
CLAUDE_TERM_LINE="$(wait_terminal "$CLAUDE_ID" "$CLAUDE_DEADLINE_S")"
CLAUDE_TERM_RC=$?
kill "$FOLLOW_PID" 2>/dev/null; FOLLOW_PID=""
dump_log_stable "$CLAUDE_ID" "$TMP/claude.log"

if [ "$CLAUDE_TERM_RC" != 0 ]; then
  fail "claude probe never reached a terminal status within ${CLAUDE_DEADLINE_S}s"
else
  CLAUDE_TERM_TS="$(ts_of "$CLAUDE_TERM_LINE")"
  CLAUDE_STATUS="$(status_of "$CLAUDE_TERM_LINE")"
  echo "  (claude probe reached $CLAUDE_STATUS $(delta "$CLAUDE_SUBMIT_TS" "$CLAUDE_TERM_TS")s after submit)"
  if [ "$CLAUDE_STATUS" = Succeeded ]; then
    pass "claude probe finished Succeeded"
  else
    fail "claude probe finished $CLAUDE_STATUS (expected Succeeded): $CLAUDE_TERM_LINE"
  fi
  # ASSERTION 1 — the whole point of the feature.
  PROGRESS_TS="$(grep -m1 -E '^[0-9.]+ \[out\]→ ' "$TMP/claude.follow.ts" 2>/dev/null | awk '{print $1}')"
  if [ -z "$PROGRESS_TS" ]; then
    fail "no rendered progress line ('[out]→ ...') ever reached the log topic; live tail was:
$(sed -n '1,10p' "$TMP/claude.follow.ts" 2>/dev/null)"
  elif elapsed_ge "$PROGRESS_TS" "$CLAUDE_TERM_TS" "$PROGRESS_LEAD_S"; then
    pass "rendered progress line observed $(delta "$PROGRESS_TS" "$CLAUDE_TERM_TS")s BEFORE terminal status (need >= ${PROGRESS_LEAD_S}s)"
  else
    fail "rendered progress line observed only $(delta "$PROGRESS_TS" "$CLAUDE_TERM_TS")s before terminal status (need >= ${PROGRESS_LEAD_S}s) — progress is not reaching the log until the task ends"
  fi
fi

if grep -q '^\[out\]▶ session e2e-fake-session$' "$TMP/claude.log"; then
  pass "session-start event rendered"
else
  fail "rendered session-start line missing from the claude task log"
fi
if grep -q '^\[out\]done$' "$TMP/claude.log"; then
  pass "final assistant text present as '[out]done'"
else
  fail "final assistant text '[out]done' missing from the claude task log"
fi
if grep -q '"type":"' "$TMP/claude.log"; then
  fail "raw JSON leaked into the claude task log (found '\"type\":\"'):
$(grep -m3 '"type":"' "$TMP/claude.log")"
else
  pass "no raw JSON in the claude task log"
fi

echo
echo "e2e-oneshot-progress: [2/3] codex-shaped profile (codex-jsonl, reads stdin to EOF first)"
read -r CODEX_ID CODEX_SUBMIT_TS < <(submit_probe codex "e2e codex probe") \
  || setup_err "submit to the codex profile failed"
"$BIN/harness-cli" --server-cid "$CID" logs --follow "$CODEX_ID" \
  > >("$TMP/agents/stamp.sh" >"$TMP/codex.follow.ts") 2>"$TMP/codex.follow.err" &
FOLLOW_PID=$!
CODEX_TERM_LINE="$(wait_terminal "$CODEX_ID" "$CODEX_DEADLINE_S")"
CODEX_TERM_RC=$?
kill "$FOLLOW_PID" 2>/dev/null; FOLLOW_PID=""
dump_log_stable "$CODEX_ID" "$TMP/codex.log"

# ASSERTION 5 — the guarded bug is a 30-minute hang, so this deadline is
# deliberately in seconds: a generous one would make the guard useless.
if [ "$CODEX_TERM_RC" != 0 ]; then
  fail "codex probe did NOT reach a terminal status within ${CODEX_DEADLINE_S}s — it is hanging, most likely on a stdin pipe that never reaches EOF"
else
  CODEX_STATUS="$(status_of "$CODEX_TERM_LINE")"
  echo "  (codex probe reached $CODEX_STATUS $(delta "$CODEX_SUBMIT_TS" "$(ts_of "$CODEX_TERM_LINE")")s after submit)"
  if [ "$CODEX_STATUS" = Succeeded ]; then
    pass "codex probe reached a terminal status well inside ${CODEX_DEADLINE_S}s"
  else
    fail "codex probe finished $CODEX_STATUS (expected Succeeded) — reaching a terminal status by crashing is not what this asserts: $CODEX_TERM_LINE"
  fi
fi
if grep -q '^\[out\]← hi$' "$TMP/codex.log" && grep -q '^\[out\]codex done$' "$TMP/codex.log"; then
  pass "codex events decoded (agent ran past 'cat > /dev/null')"
else
  fail "codex task log missing its decoded tool/text lines — the agent did not get past its stdin read:
$(sed -n '1,10p' "$TMP/codex.log")"
fi

echo
echo "e2e-oneshot-progress: [3/3] shell-sandbox profile (no logFormat — raw passthrough)"
read -r SHELL_ID SHELL_SUBMIT_TS < <(submit_probe shell "$SHELL_PROMPT") \
  || setup_err "submit to the shell profile failed"
# No live tail here: byte-identity is a property of the WHOLE log, and the
# historical fetch is the byte-exact concatenation the server persisted. A live
# tail may legitimately attach mid-stream and would weaken, not strengthen, it.
SHELL_TERM_LINE="$(wait_terminal "$SHELL_ID" "$SHELL_DEADLINE_S")"
SHELL_TERM_RC=$?
dump_log_stable "$SHELL_ID" "$TMP/shell.log"
if [ "$SHELL_TERM_RC" != 0 ]; then
  fail "shell probe never reached a terminal status within ${SHELL_DEADLINE_S}s"
else
  SHELL_STATUS="$(status_of "$SHELL_TERM_LINE")"
  echo "  (shell probe reached $SHELL_STATUS $(delta "$SHELL_SUBMIT_TS" "$(ts_of "$SHELL_TERM_LINE")")s after submit)"
  if [ "$SHELL_STATUS" = Succeeded ]; then
    pass "shell probe finished Succeeded"
  else
    fail "shell probe finished $SHELL_STATUS (expected Succeeded): $SHELL_TERM_LINE"
  fi
fi
# ASSERTION 7 — byte-identical, CRLF and missing final newline included.
if cmp -s "$TMP/shell-expected" "$TMP/shell.log"; then
  pass "no-logFormat profile output is byte-identical to what the agent printed"
else
  fail "no-logFormat profile output is not byte-identical to what the agent printed
    expected: $(od -c "$TMP/shell-expected" | head -5)
    actual:   $(od -c "$TMP/shell.log" | head -5)"
fi

echo
if [ "$FAILURES" != 0 ]; then
  echo "e2e-oneshot-progress: FAIL — $FAILURES assertion(s) failed"
  echo "--- runner log (tail) ---"; tail -15 "$TMP/runner.log" 2>/dev/null
  exit 1
fi
echo "e2e-oneshot-progress: PASS — oneshot progress reaches the log topic before the task ends,"
echo "  a codex-shaped agent no longer waits on stdin, and a profile with no logFormat is untouched."
exit 0
