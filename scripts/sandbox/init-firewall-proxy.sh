#!/bin/bash
# Proxy-broker firewall (the --firewall-proxy mode). Runs as root inside the
# container via entrypoint.sh, AFTER the CONNECT proxy is started.
#
# L3 deny-all egress except:
#   1. loopback        — the agent reaches connect-proxy.py here (HTTPS_PROXY).
#   2. the proxy's uid — it egresses to allowlisted domains; the L7 allowlist is
#                        enforced in connect-proxy.py, so L3 trusts that uid.
#   3. the harness server — harness-cli connects directly (it does NOT honor
#                        HTTPS_PROXY), so the agent uid is allowed to reach it.
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
# harness-cli (agent uid) talks directly to the harness server (no proxy).
if [ -n "${SANDBOX_SERVER_IP:-}" ]; then
  iptables -A OUTPUT -d "$SANDBOX_SERVER_IP" -j ACCEPT
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

echo "Proxy-broker firewall configured (agent egress -> proxy uid=$PROXY_UID only)."
