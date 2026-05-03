# _daemon.sh — sourced by server.sh / runner.sh / restart.sh.
# Not a standalone script.
#
# Provides:
#   daemon_up   <slot> <bin> [args...]   nohup-launch bin/<bin> with passthrough args,
#                                        record state under <slot>
#   daemon_down <slot> <bin>             SIGTERM (escalating to SIGKILL after 5s)
#
# State files live under bin/.run/<slot>.{pid,log}. The slot is the *instance*
# identity (single-instance daemons use slot == bin; multi-instance daemons
# use a tagged slot like "agent-runner-2"). bin/ is gitignored, so pid/log
# artefacts don't pollute the working tree.
#
# Linux-only: uses /proc/<pid>/exe to verify that the recorded pid still
# belongs to the expected binary before signalling, which protects against
# pid reuse after a reboot. The verify step is keyed on the binary basename,
# not the slot, because /proc/<pid>/exe always shows the binary path.

# shellcheck shell=bash

_DAEMON_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
_DAEMON_RUN_DIR="$_DAEMON_ROOT/bin/.run"

_daemon_verify_owner() {
    local pid="$1" bin_name="$2"
    [[ -e "/proc/$pid/exe" ]] || return 1
    local exe
    exe="$(readlink "/proc/$pid/exe" 2>/dev/null || true)"
    case "$exe" in
        */"$bin_name"|*/"$bin_name"' (deleted)') return 0 ;;
    esac
    return 1
}

daemon_up() {
    local slot="$1"; shift
    local bin_name="$1"; shift
    local bin_path="$_DAEMON_ROOT/bin/$bin_name"
    local pid_file="$_DAEMON_RUN_DIR/$slot.pid"
    local log_file="$_DAEMON_RUN_DIR/$slot.log"

    mkdir -p "$_DAEMON_RUN_DIR"

    if [[ -f "$pid_file" ]]; then
        local prev
        prev="$(cat "$pid_file" 2>/dev/null || true)"
        if [[ -n "$prev" ]] && _daemon_verify_owner "$prev" "$bin_name"; then
            echo "[$slot] already running (pid=$prev, log=$log_file)"
            return 0
        fi
        rm -f "$pid_file"
    fi

    if [[ ! -x "$bin_path" ]]; then
        echo "[$slot] binary missing: $bin_path" >&2
        echo "        run 'make build' first" >&2
        return 1
    fi

    # setsid puts the daemon in its own session so SIGHUP from a closing
    # ssh/tty never reaches it; nohup is belt-and-suspenders for that signal.
    nohup setsid "$bin_path" "$@" >>"$log_file" 2>&1 < /dev/null &
    local pid=$!
    echo "$pid" > "$pid_file"

    # Catch immediate crashes (bad flag, port already bound, ...).
    sleep 0.5
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "[$slot] failed to start; tail of log:" >&2
        tail -n 20 "$log_file" >&2 || true
        rm -f "$pid_file"
        return 1
    fi
    echo "[$slot] started pid=$pid log=$log_file"
}

daemon_down() {
    local slot="$1"; shift
    local bin_name="$1"
    local pid_file="$_DAEMON_RUN_DIR/$slot.pid"

    if [[ ! -f "$pid_file" ]]; then
        echo "[$slot] not running (no pid file)"
        return 0
    fi
    local pid
    pid="$(cat "$pid_file" 2>/dev/null || true)"
    if [[ -z "$pid" ]] || ! kill -0 "$pid" 2>/dev/null; then
        echo "[$slot] not running (stale pid file)"
        rm -f "$pid_file"
        return 0
    fi
    if ! _daemon_verify_owner "$pid" "$bin_name"; then
        echo "[$slot] pid $pid no longer belongs to $bin_name; refusing to signal" >&2
        echo "        remove $pid_file manually if the binary was renamed" >&2
        return 1
    fi

    kill -TERM "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo "[$slot] SIGTERM timeout after 5s, escalating to SIGKILL pid=$pid" >&2
        kill -KILL "$pid" 2>/dev/null || true
    fi
    rm -f "$pid_file"
    echo "[$slot] stopped"
}
