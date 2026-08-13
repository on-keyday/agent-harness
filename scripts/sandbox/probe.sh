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
# The parent of the mounted repo is the interesting one: under confinement it
# must NOT list your other checkouts.
add "siblings_listing" "$(ls -1 "$(dirname "$repo")" 2>&1 | tr '\n' ',')"
add "home_listing" "$(ls -1 "$HOME" 2>&1 | tr '\n' ',')"
add "repo_writable" "$(touch "$repo/.probe_write" 2>&1 && echo yes-and-cleaned && rm -f "$repo/.probe_write" || echo NO)"
add "etc_passwd_host_users" "$(getent passwd 2>/dev/null | wc -l) entries"
add "claude_home_mounted" "$([ -d "$HOME/.claude" ] && echo yes-mount-auth || echo no-token-auth)"

# --- processes ---------------------------------------------------------------
add "proc_total" "$(ps -e 2>/dev/null | wc -l) procs"
add "host_proc_leak" "$(ps -e -o comm 2>/dev/null | grep -Ec 'agent-runner|claude-in-pod') matches (expect 0)"

# --- network: one allowlisted target, one that must be refused ---------------
add "egress_allowed" "$(curl -sS -m 6 https://api.github.com/zen 2>&1 | head -c 90)"
add "egress_blocked_ctl" "$(curl -sS -m 6 -o /dev/null -w '%{http_code}' https://example.com 2>&1 | head -c 90)"

# --- control plane (the OTHER confinement layer: server-enforced caps) -------
add "harness_cli" "$(command -v harness-cli 2>&1 || echo MISSING)"
add "harness_ls" "$(harness-cli ls 2>&1 | grep -c .) lines (or err above)"
add "harness_whoami" "$(harness-cli whoami 2>&1 | tr '\n' ' ' | head -c 200)"

report="=== SANDBOX CAPABILITY PROBE RESULT ===
${R}=== END ==="

printf '%s\n' "$report"
if [ -n "$topic" ]; then
	printf '%s\n' "$report" | harness-cli agent send --topic "$topic" --data - && echo "probe sent to $topic"
fi
