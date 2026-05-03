#!/usr/bin/env bash
# restart.sh — detached restart of a daemon managed by scripts/<name>.sh,
# preserving the running instance's flags and CWD.
#
# Why "detached": when invoked from inside a child process of the daemon
# itself (e.g. a Claude Code agent restarting its own agent-runner parent),
# the daemon's SIGTERM cascade kills the invoking shell mid-script. Doing
# down/up inside `nohup setsid` puts the restart sequence in its own session
# so it survives the cascade.
#
# Usage:
#   scripts/restart.sh <name>
#
# <name> must have a matching scripts/<name>.sh helper and a live pid under
# bin/.run/<name>.pid. Flags and CWD are read from /proc/<pid>/{cmdline,cwd}
# so the new instance comes up identically to the one being replaced.
#
# Examples:
#   scripts/restart.sh agent-runner
#   scripts/restart.sh harness-server
#
# Output and the restart sequence's stdout/stderr go to
# bin/.run/<name>.restart.log; tail it to confirm completion.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

name="${1:-}"
if [[ -z "$name" ]]; then
    echo "usage: $0 <name>   (binary name: agent-runner, harness-server, ...)" >&2
    exit 2
fi

# We don't depend on a per-daemon helper script (scripts/runner.sh,
# scripts/server.sh, etc.) because the binary-name → helper-name mapping
# is asymmetric (agent-runner ↔ runner.sh, harness-server ↔ server.sh).
# Source _daemon.sh directly and use daemon_down / daemon_up — the same
# primitives the helper scripts wrap.
if [[ ! -r "$HERE/_daemon.sh" ]]; then
    echo "[$name] missing $HERE/_daemon.sh" >&2
    exit 1
fi

pid_file="$ROOT/bin/.run/$name.pid"
if [[ ! -f "$pid_file" ]]; then
    echo "[$name] no pid file at $pid_file (daemon not currently managed); start it via the per-daemon helper first" >&2
    exit 1
fi

pid="$(cat "$pid_file")"
if ! kill -0 "$pid" 2>/dev/null; then
    echo "[$name] pid file present but pid=$pid not running; clean up bin/.run/$name.pid and start fresh" >&2
    exit 1
fi

cmdline_file="/proc/$pid/cmdline"
cwd_link="/proc/$pid/cwd"
if [[ ! -r "$cmdline_file" || ! -r "$cwd_link" ]]; then
    echo "[$name] cannot read $cmdline_file / $cwd_link (Linux-only path)" >&2
    exit 1
fi

# /proc/<pid>/cmdline is NUL-separated argv. mapfile -d '' reads each
# element verbatim, preserving spaces, glob metacharacters, etc. Drop
# argv[0] (the binary path) — the helper's `up` provides it.
mapfile -d '' -t argv < "$cmdline_file"
flags=("${argv[@]:1}")

orig_cwd="$(readlink "$cwd_link")"
log="$ROOT/bin/.run/$name.restart.log"
mkdir -p "$(dirname "$log")"

# nohup + setsid: new session, ignore SIGHUP. The detached bash runs the
# down/up sequence after this script (and possibly its caller) is gone.
nohup setsid bash -c '
    set -u
    here="$1"; shift
    name="$1"; shift
    log="$1"; shift
    orig_cwd="$1"; shift
    cd "$orig_cwd"
    # shellcheck source=_daemon.sh
    . "$here/_daemon.sh"
    {
        printf "[%s] restart %s begin (cwd=%s flags=%s)\n" \
            "$(date -Iseconds)" "$name" "$orig_cwd" "$*"
        daemon_down "$name"
        sleep 1
        daemon_up "$name" "$@"
        printf "[%s] restart %s end\n" "$(date -Iseconds)" "$name"
    } >> "$log" 2>&1
' _ "$HERE" "$name" "$log" "$orig_cwd" "${flags[@]}" \
    >/dev/null 2>&1 < /dev/null & disown

echo "[$name] detached restart kicked off (subshell pid=$!, log=$log)"
echo "[$name] follow with: tail -f $log"
