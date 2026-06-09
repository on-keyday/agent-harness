#!/bin/bash
# Sandbox entrypoint. Used ONLY when the wrapper enables egress filtering
# (--firewall): the container then starts as root (--user 0 + NET_ADMIN/NET_RAW)
# so this can apply the iptables allowlist, after which it drops to the agent
# user (the keep-id host uid) and execs the real command (claude ...).
#
# Without --firewall the wrapper runs claude directly as the keep-id user and
# this entrypoint is not used.
set -euo pipefail

# Capture HOME before gosu. gosu resets HOME from the target uid's /etc/passwd
# entry, and the node base image already owns uid 1000 as user `node`
# (home /home/node) — which collides with the keep-id host user and would send
# claude to an empty /home/node → "Not logged in". Force the wrapper's HOME back.
AGENT_HOME="${HOME:-/home/$(id -un 2>/dev/null || echo user)}"

if [ "${SANDBOX_FIREWALL:-0}" = "1" ]; then
  # Fail CLOSED: if the egress allowlist can't be applied, refuse to run claude
  # unconfined — a firewall that silently fails open is worse than none.
  /usr/local/bin/sandbox-init-firewall.sh || {
    echo "FATAL: egress firewall setup failed; refusing to run unconfined" >&2
    exit 1
  }
fi

# Drop from container-root to the agent user so claude runs non-root and its
# worktree writes land as the host user (keep-id). gosu takes a numeric uid:gid
# directly; re-assert HOME afterwards (see above).
exec gosu "${DROP_UID:-1000}:${DROP_GID:-1000}" env HOME="$AGENT_HOME" "$@"
