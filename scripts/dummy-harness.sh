#!/usr/bin/env bash
# Stand up a throwaway server + runner from this checkout's bin/, for manual
# verification and debugging.
#
# Why this exists: several checks can only be done against a live harness —
# interactive wake, TUI reconnect, "does the operator actually see this" — and
# standing one up by hand has two traps that cost real time each rediscovery:
#
#   1. A task shell inherits HARNESS_* from the harness that spawned it.
#      HARNESS_AUTH_TICKET is a ticket for the LIVE server; against a dummy one
#      it fails as `psk auth: psk: server rejected: BadTicket`, which reads like
#      a PSK mistake rather than a stale-ticket one.
#   2. It also inherits CLAUDE_CODE_* / CLAUDECODE / AI_AGENT. A claude spawned
#      with those set decides it is a child session and silently disables
#      transcript saving, so `--continue` / `--resume` find nothing.
#
# Both are scrubbed here. Do not "simplify" that away.
#
# The live fleet is never touched: loopback only, an ephemeral port, a fresh
# PSK, and a temp data dir that goes away on teardown.
#
# Usage:
#   scripts/dummy-harness.sh up [--agent claude|fake] [--model NAME] [--detach] [--name N]
#   scripts/dummy-harness.sh env  [--name N]   # print `export` lines for an instance
#   scripts/dummy-harness.sh down [--name N]
#
# --name lets independent instances coexist. That is not a nicety: checking
# what a client does when its server restarts needs the window you are
# watching through to live on a DIFFERENT server than the one you restart.
# Run a `host` instance and a `target` instance, drive the client from a
# session on `host`, and bounce `target`.
#
#   # foreground (Ctrl-C to stop), real claude on the cheapest model:
#   scripts/dummy-harness.sh up
#
#   # detached, then drive it:
#   scripts/dummy-harness.sh up --detach
#   eval "$(scripts/dummy-harness.sh env)"
#   harness-cli --server-cid "$CID" ls
#   scripts/dummy-harness.sh down
#
# Every instance gets a `bash` profile alongside the agent one. A oneshot task
# submitted to it runs its prompt as a shell command with that task's
# HARNESS_* in the environment — which is how you exercise agent-side commands
# (`harness-cli agent send`, inbox, …) without hand-minting a ticket.
set -uo pipefail

for v in $(env | grep -oE '^(HARNESS|CLAUDE_CODE|CLAUDECODE|AI_AGENT)[A-Z_]*'); do unset "$v"; done

HERE=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$HERE/.." && pwd)
BIN="$REPO_ROOT/bin"
STATE_DIR="${TMPDIR:-/tmp}/harness-dummy-$(id -u)"
NAME=default
STATE=""   # set by set_state once --name is parsed

set_state() { STATE="$STATE_DIR/$NAME.env"; }

# --name may appear anywhere; pull it out before the per-command parsers run.
parse_name() {
  local -a rest=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --name) NAME="${2:?--name needs a value}"; shift 2 ;;
      *) rest+=("$1"); shift ;;
    esac
  done
  set_state
  ARGS=("${rest[@]}")
}

die() { echo "dummy-harness: $*" >&2; exit 1; }
setup_err() { echo "dummy-harness: SETUP: $*" >&2; exit 2; }

pick_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
}

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }

cmd_env() {
  [ -f "$STATE" ] || die "no instance (state file $STATE missing); run 'up --detach' first"
  # The scrub at the top of this script only cleans the script's own
  # environment. Whoever eval's this is a different shell — inside a harness
  # task it still carries HARNESS_AUTH_TICKET for the LIVE server, and
  # harness-cli prefers a ticket over the PSK, so every call would fail
  # BadTicket while looking like a PSK problem. Emit the unsets too.
  echo 'unset HARNESS_AUTH_TICKET HARNESS_TASK_ID HARNESS_RUNNER_ID HARNESS_SERVER_CID HARNESS_WS_PATH HARNESS_REPO_PATH HARNESS_HOSTNAME'
  cat "$STATE"
}

cmd_down() {
  [ -f "$STATE" ] || { echo "dummy-harness: nothing to stop"; return 0; }
  # shellcheck disable=SC1090
  . "$STATE"
  for pid in "${RUNNER_PID:-}" "${SERVER_PID:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  sleep 0.5
  for pid in "${RUNNER_PID:-}" "${SERVER_PID:-}"; do
    [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
  done
  [ -n "${TMP:-}" ] && [ -d "$TMP" ] && rm -rf "$TMP"
  rm -f "$STATE"
  echo "dummy-harness: stopped"
}

cmd_up() {
  local agent=claude model=claude-haiku-4-5-20251001 detach=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --agent) agent="${2:?--agent needs a value}"; shift 2 ;;
      --model) model="${2:?--model needs a value}"; shift 2 ;;
      --detach|-d) detach=1; shift ;;
      *) die "unknown flag: $1" ;;
    esac
  done

  # make build, not go build: harness-server embeds webui/static/main.wasm and
  # refuses to start without it. A server that never starts makes every
  # subsequent check trivially "pass", which is worse than a loud failure.
  ( cd "$REPO_ROOT" && make build >/dev/null ) || setup_err "make build failed"
  for b in harness-server agent-runner harness-cli; do
    [ -x "$BIN/$b" ] || setup_err "$BIN/$b missing after make build"
  done

  [ -f "$STATE" ] && die "an instance is already recorded at $STATE; run 'down' first"
  mkdir -p "$STATE_DIR"

  local tmp port psk cid
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/harness-dummy.XXXXXX")
  port=$(pick_port); [ -n "$port" ] || setup_err "could not find a free loopback port"
  psk="dummy-$(head -c 12 /dev/urandom | base64 | tr -d '/+=')"
  cid="ws:127.0.0.1:${port}-*"

  mkdir -p "$tmp/repo" "$tmp/data"
  git -C "$tmp/repo" init -q
  git -C "$tmp/repo" -c user.email=dummy@example.invalid -c user.name=dummy \
      commit -q --allow-empty -m "dummy repo"

  "$BIN/harness-server" --listen "127.0.0.1:${port}" --psk "$psk" --operator-psk "$psk" \
    --data-dir "$tmp/data" >"$tmp/server.log" 2>&1 &
  local server_pid=$!

  for _ in $(seq 1 40); do listening "$port" && break; sleep 0.25; done
  listening "$port" || { head -20 "$tmp/server.log" >&2; setup_err "server never listened on $port"; }

  # A `bash` profile rides along with every instance: submitting a oneshot task
  # to it runs the prompt as a shell command with that task's HARNESS_* set,
  # which is the only way to exercise agent-side commands without minting a
  # ticket by hand.
  local profiles
  profiles=$(cat <<JSON
[{"name":"bash","bin":"bash",
  "oneshotArgv":["{args}","-c","{prompt}"],
  "resumeOneshotArgv":["{args}","-c","{prompt}"],
  "resumeInteractiveArgv":["{args}"],
  "logFormat":""}]
JSON
)

  # --max-tasks 4, not the default 1: an interactive session holds a slot for
  # its whole life, so with one slot every follow-up task sits Queued forever
  # and the symptom is silence, not an error.
  local -a runner_args=(
    --server-cid "$cid" --psk "$psk" --roots "$tmp/repo" --no-worktree
    --max-tasks 4
    --agent-profiles "$profiles"
  )
  case "$agent" in
    claude)
      runner_args+=(
        --agent-bin claude
        --claude-args "--model $model"
        --agent-oneshot-argv '--output-format stream-json --verbose {args} -p {prompt}'
        --agent-resume-oneshot-argv '--output-format stream-json --verbose {args} --continue -p {prompt}'
        --agent-resume-interactive-argv '{args} --continue'
        --agent-log-format claude-stream-json
      )
      ;;
    fake)
      # Deterministic stand-in: emits claude's stream-json shapes with pauses,
      # for checks that must not depend on a model or a network.
      cat > "$tmp/fake-claude.sh" <<'FAKE'
#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"dummy-session"}'
sleep 1
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}'
sleep 1
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","content":"hi"}]}}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","duration_ms":2000,"total_cost_usd":0}'
FAKE
      chmod +x "$tmp/fake-claude.sh"
      runner_args+=(
        --agent-bin "$tmp/fake-claude.sh"
        --agent-oneshot-argv '--output-format stream-json --verbose {args} -p {prompt}'
        --agent-resume-oneshot-argv '--output-format stream-json --verbose {args} --continue -p {prompt}'
        --agent-resume-interactive-argv '{args} --continue'
        --agent-log-format claude-stream-json
      )
      ;;
    *) die "unknown --agent: $agent (want claude or fake)" ;;
  esac

  "$BIN/agent-runner" "${runner_args[@]}" >"$tmp/runner.log" 2>&1 &
  local runner_pid=$!

  export HARNESS_PSK="$psk"   # harness-cli's operator binder falls back to this
  local registered=0
  for _ in $(seq 1 40); do
    kill -0 "$runner_pid" 2>/dev/null || break
    if "$BIN/harness-cli" --server-cid "$cid" ls 2>/dev/null | grep -q "agent="; then
      registered=1; break
    fi
    sleep 0.25
  done
  if [ "$registered" != 1 ]; then
    tail -20 "$tmp/runner.log" >&2
    kill "$runner_pid" "$server_pid" 2>/dev/null
    setup_err "runner never registered"
  fi

  cat > "$STATE" <<EOF
export HARNESS_PSK='$psk'
export CID='$cid'
export TMP='$tmp'
export BIN='$BIN'
export REPO='$tmp/repo'
export SERVER_PID='$server_pid'
export RUNNER_PID='$runner_pid'
export SERVER_PORT='$port'
EOF

  echo "dummy-harness: up  name=$NAME  agent=$agent  port=$port  repo=$tmp/repo"
  echo "dummy-harness: eval \"\$($0 env)\" to drive it; '$0 down' to stop"

  if [ "$detach" = 1 ]; then
    return 0
  fi
  trap 'cmd_down; exit 0' INT TERM
  echo "dummy-harness: foreground; Ctrl-C to stop"
  while kill -0 "$server_pid" 2>/dev/null; do sleep 2; done
}

sub="${1:-}"; shift 2>/dev/null
ARGS=()
parse_name "$@"
case "$sub" in
  up)   cmd_up "${ARGS[@]+"${ARGS[@]}"}" ;;
  env)  cmd_env ;;
  down) cmd_down ;;
  *)    sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
