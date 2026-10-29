#!/bin/bash
# Sandbox entrypoint. Used whenever the wrapper enables egress filtering (the
# default proxy mode, or --firewall): the container then starts as root
# (--user 0 + NET_ADMIN/NET_RAW) so this can apply the iptables rules, after
# which it drops to the agent user (the keep-id host uid) and execs the real
# command (claude ...).
#
# Under --no-firewall the wrapper runs claude directly as the keep-id user and
# this entrypoint is not used.
set -euo pipefail

# Capture HOME before gosu. gosu resets HOME from the target uid's /etc/passwd
# entry, and the node base image already owns uid 1000 as user `node`
# (home /home/node) — which collides with the keep-id host user and would send
# claude to an empty /home/node → "Not logged in". Force the wrapper's HOME back.
AGENT_HOME="${HOME:-/home/$(id -un 2>/dev/null || echo user)}"

if [ "${SANDBOX_FIREWALL_PROXY:-0}" = "1" ]; then
  # Proxy-broker mode: start the allowlisting CONNECT proxy as its own uid, then
  # apply the owner-match firewall (agent uid gets no raw egress), then point the
  # agent at the proxy. Fail CLOSED on firewall error.
  PROXY_UID="${SANDBOX_PROXY_UID:-1001}"
  PROXY_PORT="${SANDBOX_PROXY_PORT:-18080}"
  # Double-forked on purpose: this shell EXECs claude below, so a plain `... &`
  # leaves the proxy a direct child of the pid that becomes claude — and claude
  # only reaps pids it spawned itself, so a dead proxy would sit <defunct> for
  # the container's life, outside the reach of the wrapper's --init (which only
  # covers REPARENTED orphans). The subshell exits immediately, handing the
  # proxy to catatonit, which does reap it.
  # (Measured 2026-08-18: `&` -> ppid=claude, 1 zombie; `( & )` -> ppid=1, 0.)
  # Its DENY/BUSY lines go to stderr, which under an interactive session is the
  # agent's PTY; the wrapper points them at a bind-mounted host file unless
  # --no-proxy-log.
  # The redirect is opened here, as container root, so the file need not be
  # writable by the proxy uid.
  # A relay upstream (unix:<path>, claude) is a socket bind-mounted from the
  # host, where the credential is added. Under keep-id the socket's own mode
  # cannot tell the agent from the proxy, so the directory around it does:
  # only the proxy uid may enter it. Done before anything runs as the agent.
  case "${SANDBOX_INJECT_UPSTREAM:-}" in
    unix:*)
      relay_dir=$(dirname "${SANDBOX_INJECT_UPSTREAM#unix:}")
      chown "$PROXY_UID" "$relay_dir" && chmod 0700 "$relay_dir" || {
        echo "FATAL: cannot restrict $relay_dir to the proxy uid; refusing to expose the credential relay to the agent" >&2
        exit 1
      }
      ;;
  esac
  ( if [ -n "${SANDBOX_PROXY_LOG:-}" ]; then exec 2>>"$SANDBOX_PROXY_LOG"; fi
    gosu "$PROXY_UID" env SANDBOX_PROXY_PORT="$PROXY_PORT" \
      /usr/local/bin/sandbox-connect-proxy & )
  for _ in $(seq 1 50); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$PROXY_PORT") 2>/dev/null; then break; fi
    sleep 0.1
  done
  # Credential injection (--inject-auth): the same proxy process also serves a
  # loopback listener that adds the agent's credential to each request
  # (cmd/sandbox-connect-proxy/inject.go), or forwards to the host relay that
  # does. A credential handed in here reached the proxy through the
  # environment it inherited above, and is dropped from ours before anything
  # runs as the agent. Without the listener the agent could only fail on every
  # request, so its absence is fatal.
  if [ -n "${SANDBOX_INJECT_UPSTREAM:-}" ]; then
    INJECT_PORT="${SANDBOX_INJECT_PORT:-18081}"
    up=0
    for _ in $(seq 1 50); do
      if (exec 3<>"/dev/tcp/127.0.0.1/$INJECT_PORT") 2>/dev/null; then up=1; break; fi
      sleep 0.1
    done
    [ "$up" = 1 ] || {
      echo "FATAL: credential injector not listening on 127.0.0.1:$INJECT_PORT; refusing to start the agent without its credential" >&2
      exit 1
    }
  fi
  unset SANDBOX_INJECT_KEY
  /usr/local/bin/sandbox-init-firewall-proxy.sh || {
    echo "FATAL: proxy-broker firewall setup failed; refusing to run unconfined" >&2
    exit 1
  }
  P="http://127.0.0.1:$PROXY_PORT"
  export HTTPS_PROXY="$P" HTTP_PROXY="$P" https_proxy="$P" http_proxy="$P"
  export NO_PROXY="" no_proxy=""
  # The injector is reached directly, not through the CONNECT proxy, which
  # refuses loopback targets. Lower case too: undici (pi) reads no_proxy before
  # NO_PROXY, so an empty no_proxy hides any NO_PROXY (measured 2026-10-29: pi
  # tunnelled 127.0.0.1:18081 through the proxy and got 403).
  if [ -n "${SANDBOX_INJECT_UPSTREAM:-}" ]; then
    export NO_PROXY=127.0.0.1 no_proxy=127.0.0.1
  fi
fi

# Injection runs only inside the proxy; outside proxy mode a credential handed
# in here would simply reach the agent.
if [ -n "${SANDBOX_INJECT_KEY:-}" ]; then
  echo "FATAL: SANDBOX_INJECT_KEY outside the proxy mode; refusing to hand the agent a credential meant for the proxy" >&2
  exit 1
fi

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
