#!/bin/bash
# Proxy-broker firewall (the --firewall-proxy mode). Runs as root inside the
# container via entrypoint.sh, AFTER the CONNECT proxy is started.
#
# L3 deny-all egress except:
#   1. loopback        — the agent reaches cmd/sandbox-connect-proxy here (HTTPS_PROXY).
#   2. the proxy's uid — it egresses to allowlisted domains; the L7 allowlist is
#                        enforced in cmd/sandbox-connect-proxy, so L3 trusts that uid.
#   3. the harness server, ONE port — harness-cli connects directly (it does NOT
#                        honor HTTPS_PROXY), so the agent uid is allowed to reach
#                        that ip:proto:port. Not the whole host: this rule was
#                        address-only until 2026-08-18 and a smoke test measured
#                        the server machine's sshd answering from inside the
#                        sandbox — a lateral-movement surface the proxy design
#                        otherwise closes.
#
# The agent uid therefore has NO arbitrary raw-socket egress: only loopback to
# the proxy + the (trusted, user-owned) harness server. Injected code cannot
# exfil over a raw socket (L3 denies it) nor through the proxy to a
# non-allowlisted host (the proxy refuses it).
set -uo pipefail
PROXY_UID="${SANDBOX_PROXY_UID:-1001}"

iptables -F; iptables -X 2>/dev/null || true
iptables -t nat -F 2>/dev/null || true
iptables -t mangle -F 2>/dev/null || true

iptables -A INPUT  -i lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A INPUT  -m state --state ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
# The proxy uid may egress (DNS + the allowlisted TLS connections it brokers).
iptables -A OUTPUT -m owner --uid-owner "$PROXY_UID" -j ACCEPT
# harness-cli (agent uid) talks directly to the harness server (no proxy) — at
# its own ip:proto:port, parsed by the wrapper out of HARNESS_SERVER_CID. No port
# means no carve-out: harness-cli then fails, which is the fail-closed direction.
if [ -n "${SANDBOX_SERVER_IP:-}" ]; then
  if [ -n "${SANDBOX_SERVER_PORT:-}" ]; then
    iptables -A OUTPUT -d "$SANDBOX_SERVER_IP/32" \
      -p "${SANDBOX_SERVER_PROTO:-tcp}" --dport "$SANDBOX_SERVER_PORT" -j ACCEPT
  else
    echo "WARN: harness server port unknown — NOT allowlisting $SANDBOX_SERVER_IP; harness-cli will be blocked" >&2
  fi
fi

iptables -P INPUT DROP   || exit 1
iptables -P FORWARD DROP || exit 1
iptables -P OUTPUT DROP  || exit 1
iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited || exit 1

# IPv6: allow only the proxy uid + loopback; deny the agent a v6 bypass.
ip6tables -F 2>/dev/null || true
ip6tables -P INPUT DROP 2>/dev/null || true
ip6tables -P FORWARD DROP 2>/dev/null || true
ip6tables -P OUTPUT DROP 2>/dev/null || true
ip6tables -A INPUT  -i lo -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -o lo -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -m owner --uid-owner "$PROXY_UID" -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true

echo "Proxy-broker firewall configured (agent egress -> proxy uid=$PROXY_UID + harness server ${SANDBOX_SERVER_IP:-none}:${SANDBOX_SERVER_PORT:-none} only)."
