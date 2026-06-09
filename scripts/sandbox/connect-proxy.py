#!/usr/bin/env python3
"""Minimal allowlisting HTTPS CONNECT proxy for the sandbox container.

Runs INSIDE the container as a dedicated uid. The agent (claude) is L3-blocked
from direct egress (iptables owner-match in init-firewall-proxy.sh) and routed
here via HTTPS_PROXY, so every API / WebFetch connection funnels through this
allowlist. Injected code in the agent cannot open a raw socket (L3 denies its
uid) nor reach a non-allowlisted host (this proxy refuses the CONNECT).

Only the CONNECT method is handled (HTTPS tunnelling, no MITM — TLS bodies are
not inspected; SNI/host is all the allowlist sees). Plain-HTTP (port 80, GET via
proxy) is NOT forwarded. The allowlist is a suffix match; extend it with the
SANDBOX_PROXY_ALLOW env (comma-separated). Every decision is logged to stderr.
"""
import os
import select
import socket
import sys
import threading

# Essentials: api.anthropic.com (API + server-side WebSearch) + common dev
# hosts. Extend per task with SANDBOX_PROXY_ALLOW for WebFetch research domains.
DEFAULT_ALLOW = [
    "api.anthropic.com",
    "github.com",            # + api./codeload. via suffix
    "githubusercontent.com",  # raw./objects.
    "npmjs.org",             # registry.
    "pypi.org",
    "pythonhosted.org",      # files.
]


def load_allow():
    allow = set(DEFAULT_ALLOW)
    for d in os.environ.get("SANDBOX_PROXY_ALLOW", "").split(","):
        d = d.strip().lower().strip(".")
        if d:
            allow.add(d)
    return allow


ALLOW = load_allow()


def permitted(host):
    h = host.lower().rstrip(".")
    return any(h == d or h.endswith("." + d) for d in ALLOW)


def log(msg):
    sys.stderr.write("[sandbox-proxy] %s\n" % msg)
    sys.stderr.flush()


def splice(a, b):
    try:
        while True:
            r, _, _ = select.select([a, b], [], [])
            if a in r:
                data = a.recv(65536)
                if not data:
                    break
                b.sendall(data)
            if b in r:
                data = b.recv(65536)
                if not data:
                    break
                a.sendall(data)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.close()
            except OSError:
                pass


def handle(client):
    upstream = None
    try:
        client.settimeout(30)
        req = b""
        while b"\r\n\r\n" not in req:
            chunk = client.recv(4096)
            if not chunk:
                return
            req += chunk
            if len(req) > 65536:
                return
        line = req.split(b"\r\n", 1)[0].decode("latin1", "replace")
        parts = line.split()
        if len(parts) < 2 or parts[0].upper() != "CONNECT":
            client.sendall(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
            return
        host, _, port = parts[1].partition(":")
        try:
            port = int(port or "443")
        except ValueError:
            port = 443
        if not permitted(host):
            log("DENY  %s:%d" % (host, port))
            client.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
            return
        try:
            upstream = socket.create_connection((host, port), timeout=15)
        except OSError as e:
            log("FAIL  %s:%d (%s)" % (host, port, e))
            client.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
            return
        log("ALLOW %s:%d" % (host, port))
        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        client.settimeout(None)
        splice(client, upstream)
    except OSError:
        pass
    finally:
        for s in (client, upstream):
            if s is not None:
                try:
                    s.close()
                except OSError:
                    pass


def main():
    bind = os.environ.get("SANDBOX_PROXY_BIND", "127.0.0.1")
    port = int(os.environ.get("SANDBOX_PROXY_PORT", "18080"))
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((bind, port))
    srv.listen(128)
    log("listening on %s:%d; allow=%s" % (bind, port, ",".join(sorted(ALLOW))))
    while True:
        try:
            client, _ = srv.accept()
        except OSError:
            continue
        threading.Thread(target=handle, args=(client,), daemon=True).start()


if __name__ == "__main__":
    main()
