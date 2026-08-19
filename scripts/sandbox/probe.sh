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

# --- which launch configuration this run measured ----------------------------
# FIRST, because every finding below is conditional on it, and the modes differ
# in ways a mode-less record hides. Two that already bit us: the ip allowlist
# leaves udp/53 open to any host while the proxy mode does not, so "DNS blocked"
# measured under --firewall-proxy says nothing about --firewall; and upstream's
# blanket ssh rule existed only in ip mode, so a proxy-mode run structurally
# could not reach it. Likewise ~/.claude is a host-persistent write surface under
# mount auth and does not exist at all under token auth. Without these two lines
# a partial sweep reads later as a full one.
if [ "${SANDBOX_FIREWALL_PROXY:-0}" = "1" ]; then fw="proxy (deny-all + CONNECT proxy)"
elif [ "${SANDBOX_FIREWALL:-0}" = "1" ]; then fw="ip (iptables+ipset allowlist)"
else fw="none (open egress)"; fi
# Auth mode is MEASURED, not declared: only a bind mount proves a host config
# dir is in here (token auth leaves an ephemeral one in the image's home, which
# a bare -d test cannot tell apart). The pattern covers every agent the wrapper
# supports — a claude-only regex reported "token-or-ephemeral" for a codex
# container that had ~/.codex bind-mounted rw, i.e. it understated the exposure.
# opencode is the one that does NOT live in a single dotdir: its four mounts sit
# under the XDG parents (~/.config, ~/.cache, ~/.local/share, ~/.local/state) as
# a plain `opencode` component, so it is matched via those parents rather than by
# a bare `opencode` alternative — which would also match any mounted worktree
# that happens to be named opencode.
cfg_re=' /[^ ]*/(\.claude(\.json)?|\.codex|\.gemini|(\.config|\.cache|\.local/(share|state))/opencode)( |$)'
if grep -qE "$cfg_re" /proc/self/mountinfo 2>/dev/null; then
	auth="mount (host agent config bind-mounted rw; persists past the container)"
else
	auth="token-or-ephemeral (no host agent config mount)"
fi
add "config_firewall" "$fw [HTTPS_PROXY=${HTTPS_PROXY:-unset}]"
add "config_auth" "$auth"
# WHICH agent this container is running. Same reason as the two lines above: a
# report that does not name its subject reads later as a claim about all of
# them, and the agents differ in exactly what config_auth measures.
add "config_agent" "${SANDBOX_AGENT:-unset (pre-generic wrapper — assume claude)}"

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
# The mount lines themselves, as evidence behind config_auth above.
add "agent_config_mount_lines" "$(grep -oE "$cfg_re" /proc/self/mountinfo 2>/dev/null | tr -d ' ' | tr '\n' ',')"

# --- processes ---------------------------------------------------------------
# Read /proc directly: the image ships no procps, and `ps` missing made the
# leak count read 0 — indistinguishable from a real pass.
add "proc_total" "$(ls -d /proc/[0-9]* 2>/dev/null | wc -l) procs (via /proc)"
add "host_proc_leak" "$(cat /proc/[0-9]*/comm 2>/dev/null | grep -Ec 'agent-runner|agent-in-pod') matches (expect 0)"

# --- network: one allowlisted target, one that must be refused ---------------
# The second one is a control: it is EXPECTED to succeed when the slot runs
# without --firewall / --firewall-proxy. It only proves confinement when one of
# those is on — read it together with the wrapper's "firewall=" log line.
add "egress_allowed" "$(curl -sS -m 6 https://api.github.com/zen 2>&1 | head -c 90)"
add "egress_nonallowlisted" "$(curl -sS -m 6 -o /dev/null -w '%{http_code}' https://example.com 2>&1 | head -c 90) (refusal expected ONLY under --firewall*; read against config_firewall)"

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
