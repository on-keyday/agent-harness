# _daemon.sh — sourced by server.sh / runner.sh. Not a standalone script.
# Provides:
#   daemon_up   <name> [args...]   nohup-launch bin/<name> with passthrough args
#   daemon_down <name>             SIGTERM (escalating to SIGKILL after 5s)
#
# State files live under bin/.run/<name>.{pid,log}. bin/ is gitignored, so
# pid/log artifacts don't pollute the working tree.
#
# Linux-only: uses /proc/<pid>/exe to verify pid ownership before signalling,
# which protects against pid reuse after a reboot.

# shellcheck shell=bash

_DAEMON_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
_DAEMON_RUN_DIR="$_DAEMON_ROOT/bin/.run"

_daemon_verify_owner() {
    local pid="$1" name="$2"
    [[ -e "/proc/$pid/exe" ]] || return 1
    local exe
    exe="$(readlink "/proc/$pid/exe" 2>/dev/null || true)"
    case "$exe" in
        */"$name"|*/"$name"' (deleted)') return 0 ;;
    esac
    return 1
}

daemon_up() {
    local name="$1"; shift
    local bin_path="$_DAEMON_ROOT/bin/$name"
    local pid_file="$_DAEMON_RUN_DIR/$name.pid"
    local log_file="$_DAEMON_RUN_DIR/$name.log"

    mkdir -p "$_DAEMON_RUN_DIR"

    if [[ -f "$pid_file" ]]; then
        local prev
        prev="$(cat "$pid_file" 2>/dev/null || true)"
        if [[ -n "$prev" ]] && _daemon_verify_owner "$prev" "$name"; then
            echo "[$name] already running (pid=$prev, log=$log_file)"
            return 0
        fi
        rm -f "$pid_file"
    fi

    if [[ ! -x "$bin_path" ]]; then
        echo "[$name] binary missing: $bin_path" >&2
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
        echo "[$name] failed to start; tail of log:" >&2
        tail -n 20 "$log_file" >&2 || true
        rm -f "$pid_file"
        return 1
    fi
    echo "[$name] started pid=$pid log=$log_file"
}

daemon_down() {
    local name="$1"
    local pid_file="$_DAEMON_RUN_DIR/$name.pid"

    if [[ ! -f "$pid_file" ]]; then
        echo "[$name] not running (no pid file)"
        return 0
    fi
    local pid
    pid="$(cat "$pid_file" 2>/dev/null || true)"
    if [[ -z "$pid" ]] || ! kill -0 "$pid" 2>/dev/null; then
        echo "[$name] not running (stale pid file)"
        rm -f "$pid_file"
        return 0
    fi
    if ! _daemon_verify_owner "$pid" "$name"; then
        echo "[$name] pid $pid no longer belongs to $name; refusing to signal" >&2
        echo "        remove $pid_file manually if the binary was renamed" >&2
        return 1
    fi

    kill -TERM "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        echo "[$name] SIGTERM timeout after 5s, escalating to SIGKILL pid=$pid" >&2
        kill -KILL "$pid" 2>/dev/null || true
    fi
    rm -f "$pid_file"
    echo "[$name] stopped"
}
