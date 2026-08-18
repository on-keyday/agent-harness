#!/usr/bin/env bash
# Confinement capability probe — runs INSIDE the podman sandbox container.
#
# Measures what this kit's README *claims* (filesystem / process / network /
# control-plane confinement) instead of trusting it. Run it as the task of a
# task assigned to a sandbox-preset runner slot:
#
#   harness-cli submit --repo <sandbox-root> \
#     --task 'bash scripts/sandbox/probe.sh --topic chat.<your-task-id>'
#
# The report always goes to stdout (so it lands in the task log). With --topic
# it is ALSO published to that agentboard topic, so a supervising agent gets it
# without reading logs.
#
# Flags:
#   --topic T   agentboard topic to publish the report to (default: stdout only)
#   --repo DIR  the bind-mounted worktree to test writability of (default: $PWD)
set -u

topic=""
repo="$PWD"
while [ $# -gt 0 ]; do
	case "$1" in
	--topic) topic="${2:-}"; shift 2 ;;
	--topic=*) topic="${1#*=}"; shift ;;
	--repo) repo="${2:-}"; shift 2 ;;
	--repo=*) repo="${1#*=}"; shift ;;
	-h | --help) sed -n '2,18p' "$0"; exit 0 ;;
	*) echo "probe.sh: unknown arg: $1" >&2; exit 2 ;;
	esac
done

R=""
add() { R="${R}${1}: ${2}"$'\n'; }

# --- identity / am I even in a container -------------------------------------
add "id" "$(id 2>&1)"
add "in_container" "$({ [ -f /run/.containerenv ] || [ -f /.dockerenv ]; } && echo yes || echo NO)"

# --- filesystem: what leaks in from the host ---------------------------------
# host_mounts is the authoritative answer to "what of the host is in here" — a
# listing is cwd-dependent and says nothing about what else was bind-mounted.
add "host_mounts" "$(awk '$5 ~ /^\/(home|root|mnt|media|opt|srv)/ {print $5}' /proc/self/mountinfo 2>/dev/null | sort -u | tr '\n' ',')"
add "siblings_listing" "$(ls -1a "$(dirname "$repo")" 2>&1 | tr '\n' ',')"
add "home_listing" "HOME=$HOME -> $(ls -1a "$HOME" 2>&1 | tr '\n' ',')"
add "repo_writable" "$(touch "$repo/.probe_write" 2>&1 && echo yes-and-cleaned && rm -f "$repo/.probe_write" || echo NO)"
add "etc_passwd_host_users" "$(getent passwd 2>/dev/null | wc -l) entries"
# Token auth leaves an EPHEMERAL ~/.claude in the image's own home, so a bare
# -d test reports the host login as present when it is not. Only a bind mount
# proves the host's ~/.claude is exposed.
add "claude_home_mounted" "$(grep -qE ' /[^ ]*/\.claude(\.json)?( |$)' /proc/self/mountinfo 2>/dev/null && echo yes-HOST-mount-auth || echo no-token-auth-or-ephemeral)"

# --- processes ---------------------------------------------------------------
# Read /proc directly: the image ships no procps, and `ps` missing made the
# leak count read 0 — indistinguishable from a real pass.
add "proc_total" "$(ls -d /proc/[0-9]* 2>/dev/null | wc -l) procs (via /proc)"
add "host_proc_leak" "$(cat /proc/[0-9]*/comm 2>/dev/null | grep -Ec 'agent-runner|claude-in-pod') matches (expect 0)"

# --- network: one allowlisted target, one that must be refused ---------------
# The second one is a control: it is EXPECTED to succeed when the slot runs
# without --firewall / --firewall-proxy. It only proves confinement when one of
# those is on — read it together with the wrapper's "firewall=" log line.
add "egress_allowed" "$(curl -sS -m 6 https://api.github.com/zen 2>&1 | head -c 90)"
add "egress_nonallowlisted" "$(curl -sS -m 6 -o /dev/null -w '%{http_code}' https://example.com 2>&1 | head -c 90) (refusal expected ONLY under --firewall*)"

# The harness-server carve-out must name ONE port, not the whole machine. Both
# firewall modes allowed it by address until 2026-08-18, and a smoke run reached
# that host's sshd from inside the sandbox — lateral movement the rest of the
# design closes. curl can't see this (it's L4 on a non-HTTP port), so probe the
# socket directly: the server's own port must connect, any other must not.
srv="${HARNESS_SERVER_CID:-}"; srv="${srv#*:}"; srv="${srv%-*}"
srv_ip="${srv%:*}"; srv_port="${srv##*:}"
tcp_probe() {  # host port -> open | refused-or-blocked (DROP shows as the timeout)
	timeout 6 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null && echo open || echo refused-or-blocked
}
if [ -n "$srv_ip" ] && [ -n "$srv_port" ]; then
	add "server_own_port" "$srv_ip:$srv_port -> $(tcp_probe "$srv_ip" "$srv_port") (expect open: harness-cli needs it)"
	add "server_other_port" "$srv_ip:22 -> $(tcp_probe "$srv_ip" 22) (expect refused-or-blocked under --firewall*; open = the carve-out is host-wide again)"
else
	add "server_own_port" "HARNESS_SERVER_CID unparsable ('${HARNESS_SERVER_CID:-unset}') — server probes skipped"
fi

# --- control plane (the OTHER confinement layer: server-enforced caps) -------
add "harness_cli" "$(command -v harness-cli 2>&1 || echo MISSING)"
add "harness_ls" "$(harness-cli ls 2>&1 | grep -c .) lines (or err above)"
# 2>/dev/null on purpose: harness-cli logs two INFO connection lines to stderr,
# and merging them ate the caps/scope answer inside the 200-char budget.
add "harness_whoami" "$(harness-cli whoami 2>/dev/null | tr '\n' ' ' | head -c 300 || true)"

report="=== SANDBOX CAPABILITY PROBE RESULT ===
${R}=== END ==="

printf '%s\n' "$report"
if [ -n "$topic" ]; then
	printf '%s\n' "$report" | harness-cli agent send --topic "$topic" --data - && echo "probe sent to $topic"
fi
