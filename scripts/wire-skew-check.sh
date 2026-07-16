#!/usr/bin/env bash
# wire-skew-check.sh — verify a wire (.bgn) change degrades RECOVERABLY.
#
# WHY THIS EXISTS (2026-07-16 incident)
#   Landing an appended field on RunnerHello while the server still ran the old
#   binary made the server reject every runner with psk NoIdentity. That was
#   classified FATAL, so all 12 runner slots exited within ~1s and none returned
#   when the server was upgraded — every slot needed manual recovery.
#
# WHAT IS ASSERTED — and what deliberately is NOT
#   This project carries no cross-version compat shims: both ends are rebuilt
#   together, so a skew is EXPECTED to fail. Asserting "skew works" would push us
#   toward shims we do not want. What must hold is that the failure is
#   RECOVERABLE:
#     1. a NEW runner against an OLD server RETRIES (never exits), and
#     2. it SELF-HEALS once the server is upgraded — no manual respawn.
#
#   Unit tests (cli.PskRejectedError.Retryable) pin our CLASSIFICATION of a
#   status. They cannot tell us WHICH status a version-skewed server actually
#   returns — that is an empirical property of the decoder. A future wire change
#   could make the old server answer BadPsk, or drop the connection silently, and
#   our retryable classification would not cover it. Only running the two real
#   binaries against each other catches that. That is this script's whole point.
#
#   NOT asserted: OLD runner x NEW server. Pre-fix runners exit fatally by
#   construction; that is history and cannot be fixed retroactively.
#
# THIS SCRIPT MUST BE ABLE TO FAIL
#   Its first version passed on everything — the old server never started (a
#   fresh `git worktree add` has no webui/static/main.wasm, which the server
#   requires at startup), so the runner only ever saw "connection refused" and
#   "it retried" was trivially true. A guard that cannot fail is worse than none,
#   so this version PROVES the skew was exercised (an actual rejection reached
#   the runner) before it is allowed to report PASS. Verified against the real
#   incident: with OLD_REF=6b7b9ec a pre-fix runner reproduces
#   `runner exit err="psk auth: psk: server rejected: NoIdentity"` exactly.
#
# USAGE
#   scripts/wire-skew-check.sh [OLD_REF]
#     OLD_REF defaults to the merge-base with origin/main = what is deployed now.
#   Exit 0 = pass (or no wire change → skipped), 1 = FAIL, 2 = setup error.
set -uo pipefail

# The harness injects HARNESS_* into task shells; they would point our dummy
# runner/cli at the REAL server (and make harness-cli authenticate as an agent).
for v in $(env | grep -oE '^HARNESS_[A-Z_]+'); do unset "$v"; done

REPO="$(cd "$(dirname "$0")/.." && pwd)"; cd "$REPO" || exit 2
OLD_REF="${1:-$(git merge-base origin/main HEAD 2>/dev/null)}"
[ -n "$OLD_REF" ] || { echo "wire-skew-check: cannot resolve OLD_REF (pass one explicitly)"; exit 2; }
OLD_SHA="$(git rev-parse --short "$OLD_REF" 2>/dev/null)" || { echo "wire-skew-check: bad ref '$OLD_REF'"; exit 2; }
NEW_SHA="$(git rev-parse --short HEAD)"

if [ -z "$(git diff --name-only "$OLD_REF"...HEAD -- '*.bgn')" ]; then
  echo "wire-skew-check: no .bgn change between $OLD_SHA..$NEW_SHA — skipped (exit 0)"
  exit 0
fi
echo "wire-skew-check: wire changed $OLD_SHA -> $NEW_SHA; verifying the skew stays recoverable"

TMP="$(mktemp -d)"; PORT="${WIRE_SKEW_PORT:-18899}"; CID="ws:127.0.0.1:${PORT}-*"; PSK="wire-skew-check"
SPID=""; RPID=""
cleanup() {
  [ -n "$RPID" ] && kill "$RPID" 2>/dev/null
  [ -n "$SPID" ] && kill "$SPID" 2>/dev/null
  sleep 0.3
  git worktree remove --force "$TMP/old" 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT
fail(){ echo; echo "wire-skew-check: FAIL — $1"; echo "--- runner log (tail) ---"; tail -15 "$TMP/runner.log" 2>/dev/null; exit 1; }

echo "  building NEW ($NEW_SHA) server+runner+cli..."
go build -o "$TMP/new-server" ./cmd/harness-server || exit 2
go build -o "$TMP/new-runner" ./cmd/agent-runner  || exit 2
go build -o "$TMP/new-cli"    ./cmd/harness-cli   || exit 2
echo "  building OLD ($OLD_SHA) server in a detached worktree (main checkout untouched)..."
git worktree add --detach "$TMP/old" "$OLD_REF" >/dev/null 2>&1 || exit 2
# A fresh worktree has no webui/static/main.wasm (gitignored build artifact) and
# harness-server REFUSES TO START without it. Borrow the current one: the WebUI
# is not under test, but the server must actually run or this check measures
# nothing (see "THIS SCRIPT MUST BE ABLE TO FAIL" above).
cp webui/static/main.wasm "$TMP/old/webui/static/main.wasm" 2>/dev/null \
  || { echo "wire-skew-check: webui/static/main.wasm missing — run 'make webui-build' first"; exit 2; }
( cd "$TMP/old" && go build -o "$TMP/old-server" ./cmd/harness-server ) || exit 2

mkdir -p "$TMP/repo" "$TMP/data"
( cd "$TMP/repo" && git init -q && git commit -q --allow-empty -m init ) 2>/dev/null

start_server(){ # $1 = binary
  "$1" --listen "127.0.0.1:${PORT}" --psk "$PSK" --operator-psk "$PSK" --data-dir "$TMP/data" >"$TMP/server.log" 2>&1 &
  SPID=$!
  for _ in $(seq 1 30); do
    grep -qi "server exited" "$TMP/server.log" && return 1
    (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null && return 0
    sleep 0.3
  done
  return 1
}
runner_alive(){ [ -n "$RPID" ] && kill -0 "$RPID" 2>/dev/null; }

# --- 1) NEW runner vs OLD server: skew must be EXERCISED, then survived ------
echo
echo "  [1/2] NEW runner -> OLD server (skew): must be rejected, retry, not exit"
start_server "$TMP/old-server" || { echo "wire-skew-check: OLD server ($OLD_SHA) failed to listen — setup error, NOT a pass"; head -3 "$TMP/server.log"; exit 2; }
"$TMP/new-runner" --server-cid "$CID" --psk "$PSK" --roots "$TMP/repo" --no-worktree \
  --agent-bin /bin/true >"$TMP/runner.log" 2>&1 &
RPID=$!
sleep 8   # several backoff cycles

# POSITIVE CONTROL FIRST: prove the skew actually happened. Without this, a dead
# server / refused connection would sail through as "it retried!".
if ! grep -qiE "server rejected|NoIdentity|BadPsk|BadTicket|psk auth failed" "$TMP/runner.log"; then
  if grep -qi "connection refused" "$TMP/runner.log"; then
    fail "the OLD server was not reachable — skew never exercised (setup broken, not a pass)"
  fi
  fail "no handshake rejection reached the runner — the skew was NOT exercised, so this check proves nothing. Did the old server accept the new hello? Investigate before landing."
fi
echo "        skew exercised: $(grep -oiE 'server rejected: [A-Za-z]+' "$TMP/runner.log" | head -1)"

runner_alive || fail "runner EXITED against the old server — a wire-skew rejection is being classified FATAL again (see cli.PskRejectedError.Retryable). A landing would wipe the fleet."
grep -q "runner exit" "$TMP/runner.log" && fail "runner logged 'runner exit' (fatal path taken on skew)"
echo "  [1/2] PASS — rejected, stayed alive, kept retrying"

# --- 2) upgrade the server: runner must SELF-HEAL ----------------------------
echo
echo "  [2/2] upgrade server to NEW: runner must self-heal (no manual restart)"
before="$(grep -c "persist: connected" "$TMP/runner.log" 2>/dev/null)"; before="${before:-0}"
kill "$SPID" 2>/dev/null; sleep 1
start_server "$TMP/new-server" || { echo "wire-skew-check: NEW server failed to listen — setup error"; head -3 "$TMP/server.log"; exit 2; }

# Assert self-heal from the RUNNER's own log, not via harness-cli: the cli would
# add its own auth path as a failure mode and tell us nothing about the runner.
# The runner's backoff can already be several seconds wide by now, so wait long
# enough to clear it (it doubles up to --reconnect-max).
healed=0
for _ in $(seq 1 60); do
  now="$(grep -c "persist: connected" "$TMP/runner.log" 2>/dev/null)"; now="${now:-0}"
  [ "$now" -gt "$before" ] && { healed=1; break; }
  sleep 0.5
done
runner_alive || fail "runner died while the server was being upgraded"
[ "$healed" = 1 ] || fail "runner did NOT reconnect within 30s of the server upgrade — no self-heal, so a real landing would strand the fleet"
echo "  [2/2] PASS — runner re-registered on its own after the upgrade"

echo
echo "wire-skew-check: PASS — the $OLD_SHA -> $NEW_SHA wire change degrades recoverably."
echo "  (Still restart the SERVER first when deploying: a skew then costs a reconnect, not a wipe.)"
exit 0
