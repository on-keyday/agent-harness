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
#   scripts/restart.sh <slot>
#
# <slot> is the bin/.run/<slot>.pid name — i.e. the binary name for primary
# instances (`agent-runner`, `harness-server`) or `<binary>-<tag>` for
# additional instances spawned via `runner.sh up --as <tag>` etc.
# Flags, CWD, and the underlying binary are read from
# /proc/<pid>/{cmdline,cwd} so the new instance comes up identically to
# the one being replaced.
#
# Examples:
#   scripts/restart.sh agent-runner
#   scripts/restart.sh agent-runner-2     # restart the --as 2 instance
#   scripts/restart.sh harness-server
#
# Output and the restart sequence's stdout/stderr go to
# bin/.run/<slot>.restart.log; tail it to confirm completion.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

slot="${1:-}"
if [[ -z "$slot" ]]; then
    echo "usage: $0 <slot>   (e.g. agent-runner, agent-runner-2, harness-server)" >&2
    exit 2
fi

# Source _daemon.sh directly so we don't depend on the per-daemon helper
# scripts (whose names don't match the binary names: agent-runner ↔
# runner.sh, harness-server ↔ server.sh). daemon_down / daemon_up take
# (slot, bin) explicitly; we recover the bin name from /proc/<pid>/exe.
if [[ ! -r "$HERE/_daemon.sh" ]]; then
    echo "[$slot] missing $HERE/_daemon.sh" >&2
    exit 1
fi

pid_file="$ROOT/bin/.run/$slot.pid"
if [[ ! -f "$pid_file" ]]; then
    echo "[$slot] no pid file at $pid_file (daemon not currently managed); start it via the per-daemon helper first" >&2
    exit 1
fi

pid="$(cat "$pid_file")"
if ! kill -0 "$pid" 2>/dev/null; then
    echo "[$slot] pid file present but pid=$pid not running; clean up $pid_file and start fresh" >&2
    exit 1
fi

cmdline_file="/proc/$pid/cmdline"
cwd_link="/proc/$pid/cwd"
exe_link="/proc/$pid/exe"
if [[ ! -r "$cmdline_file" || ! -r "$cwd_link" || ! -r "$exe_link" ]]; then
    echo "[$slot] cannot read $cmdline_file / $cwd_link / $exe_link (Linux-only path)" >&2
    exit 1
fi

# /proc/<pid>/cmdline is NUL-separated argv. mapfile -d '' reads each
# element verbatim, preserving spaces, glob metacharacters, etc. Drop
# argv[0] (the binary path) — daemon_up adds it back from <bin_name>.
mapfile -d '' -t argv < "$cmdline_file"
flags=("${argv[@]:1}")

# Bin name is the basename of the running executable. Read /proc/<pid>/exe
# rather than parsing argv[0] so a daemon launched with a relative path or
# a renamed argv[0] still resolves correctly. Strip a trailing
# " (deleted)" marker that appears when the binary on disk has been
# replaced since the process started.
exe_path="$(readlink "$exe_link")"
bin_name="$(basename "${exe_path%% (deleted)}")"

orig_cwd="$(readlink "$cwd_link")"
log="$ROOT/bin/.run/$slot.restart.log"
mkdir -p "$(dirname "$log")"

# nohup + setsid: new session, ignore SIGHUP. The detached bash runs the
# down/up sequence after this script (and possibly its caller) is gone.
nohup setsid bash -c '
    set -u
    here="$1"; shift
    slot="$1"; shift
    bin_name="$1"; shift
    log="$1"; shift
    orig_cwd="$1"; shift
    cd "$orig_cwd"
    # shellcheck source=_daemon.sh
    . "$here/_daemon.sh"
    {
        printf "[%s] restart %s (bin=%s) begin (cwd=%s flags=%s)\n" \
            "$(date -Iseconds)" "$slot" "$bin_name" "$orig_cwd" "$*"
        daemon_down "$slot" "$bin_name"
        sleep 1
        daemon_up "$slot" "$bin_name" "$@"
        printf "[%s] restart %s end\n" "$(date -Iseconds)" "$slot"
    } >> "$log" 2>&1
' _ "$HERE" "$slot" "$bin_name" "$log" "$orig_cwd" "${flags[@]}" \
    >/dev/null 2>&1 < /dev/null & disown

echo "[$slot] detached restart kicked off (subshell pid=$!, bin=$bin_name, log=$log)"
echo "[$slot] follow with: tail -f $log"
