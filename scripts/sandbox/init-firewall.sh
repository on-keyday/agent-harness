#!/bin/bash
# Egress allowlist for the sandbox container — adapted from Anthropic's official
# .devcontainer/init-firewall.sh (github.com/anthropics/claude-code). Runs as
# root INSIDE the container (via entrypoint.sh) before dropping to the agent user.
#
# Default-deny OUTPUT; allow only DNS/SSH/loopback/established, a GitHub IP-range
# set (api.github.com/meta), a fixed domain allowlist, the container's
# default-route /24, and the harness server.
#
# Deltas vs upstream:
#   - dropped the docker-bridge DNS (127.0.0.11) save/restore — not used under
#     rootless podman / pasta;
#   - allow $SANDBOX_SERVER_IP (the harness server is on the LAN, NOT on the
#     container's default-route /24) so the bridged harness-cli keeps working;
#   - added the PyPI domains (the image ships python3 + pip);
#   - **fail-closed, not fail-open**: a single unresolvable domain or a failed
#     GitHub fetch only DROPS that allowlist entry — the script still reaches the
#     default-DROP, so a partial allowlist means *more* blocked, never open.
#     (Upstream aborts on the first resolution failure, which under our
#     entrypoint would have left the container unconfined.)
set -uo pipefail   # NOT -e: allowlist-building steps are individually tolerant.

iptables -F; iptables -X
iptables -t nat -F; iptables -t nat -X
iptables -t mangle -F; iptables -t mangle -X
ipset destroy allowed-domains 2>/dev/null || true

# DNS + SSH + loopback (added before the default-DROP below)
iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
iptables -A INPUT  -p udp --sport 53 -j ACCEPT
iptables -A OUTPUT -p tcp --dport 22 -j ACCEPT
iptables -A INPUT  -p tcp --sport 22 -m state --state ESTABLISHED -j ACCEPT
iptables -A INPUT  -i lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT

ipset create allowed-domains hash:net 2>/dev/null || true

# GitHub IP ranges (web + api + git), aggregated — best-effort.
echo "Fetching GitHub IP ranges..."
gh_ranges=$(curl -s --connect-timeout 6 https://api.github.com/meta || true)
if echo "$gh_ranges" | jq -e '.web and .api and .git' >/dev/null 2>&1; then
  while read -r cidr; do
    [[ "$cidr" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}/[0-9]{1,2}$ ]] || continue
    ipset add allowed-domains "$cidr" 2>/dev/null || true
  done < <(echo "$gh_ranges" | jq -r '(.web + .api + .git)[]' | aggregate -q 2>/dev/null)
else
  echo "WARN: GitHub meta unavailable — github ranges NOT allowlisted" >&2
fi

# Fixed domain allowlist (resolve A records → ipset). A miss skips that domain.
for domain in \
    registry.npmjs.org \
    api.anthropic.com \
    sentry.io \
    statsig.anthropic.com \
    statsig.com \
    pypi.org \
    files.pythonhosted.org; do
  ips=$(dig +noall +answer +nocomments A "$domain" 2>/dev/null | awk '$4 == "A" {print $5}')
  if [ -z "$ips" ]; then
    echo "WARN: could not resolve $domain — skipping" >&2
    continue
  fi
  while read -r ip; do
    [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] && ipset add allowed-domains "$ip" 2>/dev/null || true
  done < <(echo "$ips")
done

# Harness server (LAN address — NOT on the container's default-route /24)
if [ -n "${SANDBOX_SERVER_IP:-}" ]; then
  echo "Allowing harness server $SANDBOX_SERVER_IP"
  ipset add allowed-domains "$SANDBOX_SERVER_IP" 2>/dev/null || true
fi

# Container default-route /24 (upstream behaviour)
HOST_IP=$(ip route | awk '/^default/ {print $3; exit}')
if [ -n "$HOST_IP" ]; then
  HOST_NET=$(echo "$HOST_IP" | sed 's/\.[0-9]*$/.0\/24/')
  echo "Allowing default-route network $HOST_NET"
  iptables -A INPUT  -s "$HOST_NET" -j ACCEPT
  iptables -A OUTPUT -d "$HOST_NET" -j ACCEPT
fi

# Default DROP, then established + allowlist, REJECT the rest. These are the
# load-bearing rules — if any fails, exit non-zero so the entrypoint fails closed.
iptables -P INPUT DROP   || exit 1
iptables -P FORWARD DROP || exit 1
iptables -P OUTPUT DROP  || exit 1
iptables -A INPUT  -m state --state ESTABLISHED,RELATED -j ACCEPT || exit 1
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT || exit 1
iptables -A OUTPUT -m set --match-set allowed-domains dst -j ACCEPT || exit 1
iptables -A OUTPUT -j REJECT --reject-with icmp-admin-prohibited || exit 1

# Block IPv6 egress entirely. The allowlist (ipset hash:net) is IPv4-only, so an
# IPv6 path bypasses it (curl happy-eyeballs prefers v6 — this is how example.com
# leaked through in testing). Every allowlisted service is reachable over IPv4.
# Best-effort: if ip6tables/IPv6 is absent on the host it's simply moot.
ip6tables -F 2>/dev/null || true
ip6tables -P INPUT DROP 2>/dev/null || true
ip6tables -P FORWARD DROP 2>/dev/null || true
ip6tables -P OUTPUT DROP 2>/dev/null || true
ip6tables -A INPUT  -i lo -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -o lo -j ACCEPT 2>/dev/null || true
ip6tables -A INPUT  -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
ip6tables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true

echo "Firewall configured."

# Warn-only verification (never block claude on a flaky probe)
curl --connect-timeout 5 -s https://example.com >/dev/null 2>&1 \
  && echo "WARN: firewall check — example.com reachable (expected blocked)" >&2 || true
curl --connect-timeout 5 -s https://api.github.com/zen >/dev/null 2>&1 \
  || echo "WARN: firewall check — api.github.com unreachable (expected allowed)" >&2
exit 0
