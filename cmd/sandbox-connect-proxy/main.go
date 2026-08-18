// Command sandbox-connect-proxy is the allowlisting HTTPS CONNECT proxy for the
// podman sandbox container's --firewall-proxy mode (scripts/sandbox/).
//
// It runs INSIDE the container as a dedicated uid. The agent (claude) is
// L3-blocked from direct egress (iptables owner-match in
// init-firewall-proxy.sh) and pointed here via HTTPS_PROXY, so every API /
// WebFetch connection funnels through this allowlist. Injected code in the
// agent can neither open a raw socket (L3 denies its uid) nor reach a
// non-allowlisted host (this proxy refuses the CONNECT).
//
// Only CONNECT is served: HTTPS tunnelling, no MITM. TLS bodies are not
// inspected, so the allowlist sees the requested host and nothing more — it
// closes raw-socket and non-allowlisted exfil, not exfil to an allowlisted
// domain. Plain-HTTP proxying (GET through the proxy) is refused outright.
//
// This replaces a Python implementation that had no way to notice a dead peer:
// it selected on both sockets with no timeout and cleared every socket
// deadline once the tunnel was up, so a connection that died WITHOUT a FIN/RST
// — NAT idle expiry, a Wi-Fi drop, a suspend — left both the proxy thread and
// the agent waiting forever, and each such tunnel leaked a thread plus two fds
// for the life of the process. Every deadline below exists for that failure
// mode; none of them is decoration.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Budget for the CONNECT request line + headers. Separate from the tunnel
	// deadline: a client that opens a socket and says nothing must not hold a
	// slot.
	headerTimeout  = 30 * time.Second
	maxHeaderBytes = 64 << 10

	dialTimeout     = 15 * time.Second
	keepAlivePeriod = 30 * time.Second
)

// Shared dev hosts, needed whichever agent this proxy is fronting. The agent's
// own endpoints (api.anthropic.com and friends) arrive in SANDBOX_AGENT_DOMAINS
// from the wrapper's agent table, so this binary does not have to know which
// agent is running — and a codex container does not carry anthropic's API in
// its allowlist. Extend per task with SANDBOX_PROXY_ALLOW for WebFetch research
// domains.
var defaultAllow = []string{
	"github.com",            // + api. / codeload. via suffix
	"githubusercontent.com", // raw. / objects.
	"npmjs.org",             // registry.
	"pypi.org",
	"pythonhosted.org", // files.
}

type config struct {
	allow []string
	// idle bounds a tunnel by SILENCE, not by age: any byte in either
	// direction resets it, so a long-lived streaming response stays up while a
	// tunnel whose peer vanished is reclaimed.
	idle       time.Duration
	maxTunnels int
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[sandbox-proxy] "+format+"\n", a...)
}

// normalizeDomain lower-cases and strips the leading/trailing dots that make
// "example.com.", ".example.com" and "example.com" the same allowlist entry.
func normalizeDomain(d string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
}

func loadAllow(env string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d = normalizeDomain(d); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range defaultAllow {
		add(d)
	}
	for _, d := range strings.Split(env, ",") {
		add(d)
	}
	return out
}

// permitted reports whether host is an allowlisted domain or a subdomain of
// one. Suffix matching is on label boundaries: "notgithub.com" does not match
// "github.com".
func permitted(allow []string, host string) bool {
	h := normalizeDomain(host)
	for _, d := range allow {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logf("WARN %s=%q is not a positive duration — using %s", name, v, def)
		return def
	}
	return d
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		logf("WARN %s=%q is not a positive integer — using %d", name, v, def)
		return def
	}
	return n
}

func main() {
	bind := os.Getenv("SANDBOX_PROXY_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := os.Getenv("SANDBOX_PROXY_PORT")
	if port == "" {
		port = "18080"
	}
	cfg := config{
		// Merged, not one-or-the-other: a per-task SANDBOX_PROXY_ALLOW must not
		// be able to drop the agent's own API host by replacing the list.
		allow:      loadAllow(os.Getenv("SANDBOX_AGENT_DOMAINS") + "," + os.Getenv("SANDBOX_PROXY_ALLOW")),
		idle:       envDuration("SANDBOX_PROXY_IDLE", 5*time.Minute),
		maxTunnels: envInt("SANDBOX_PROXY_MAX_TUNNELS", 256),
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		logf("FATAL listen %s:%s: %v", bind, port, err)
		os.Exit(1)
	}
	logf("listening on %s:%s; idle=%s max=%d; allow=%s",
		bind, port, cfg.idle, cfg.maxTunnels, strings.Join(cfg.allow, ","))
	serve(ln, cfg)
}

func serve(ln net.Listener, cfg config) {
	// Bounded, not unbounded: the Python version spawned a thread per
	// connection and never reclaimed the stuck ones, so exhaustion arrived
	// silently as "the proxy stopped answering". Refusing at the cap with a
	// logged 503 makes that state visible instead.
	slots := make(chan struct{}, cfg.maxTunnels)
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			logf("accept: %v — stopping", err)
			return
		}
		go handle(conn, cfg, slots)
	}
}

func handle(client net.Conn, cfg config, slots chan struct{}) {
	defer client.Close()

	_ = client.SetDeadline(time.Now().Add(headerTimeout))
	head, err := readHead(client)
	if err != nil {
		return
	}
	line, _, _ := strings.Cut(head, "\r\n")
	parts := strings.Fields(line)
	if len(parts) < 2 || !strings.EqualFold(parts[0], "CONNECT") {
		_, _ = client.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	host, port := splitHostPort(parts[1])
	if !permitted(cfg.allow, host) {
		logf("DENY  %s:%s", host, port)
		_, _ = client.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		logf("BUSY  %s:%s — %d tunnels already open", host, port, cfg.maxTunnels)
		_, _ = client.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	// tcp4 on purpose: the agent's IPv6 is blocked by the firewall anyway, and
	// dual-stack dialing tries the AAAA address first, burning the whole
	// connect timeout before falling back (observed on registry.npmjs.org at
	// 15s). It also skips the AAAA lookup, so cold names resolve faster.
	upstream, err := (&net.Dialer{Timeout: dialTimeout}).Dial("tcp4", net.JoinHostPort(host, port))
	if err != nil {
		logf("FAIL  %s:%s (%v)", host, port, err)
		_, _ = client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer upstream.Close()

	logf("ALLOW %s:%s", host, port)
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	keepAlive(client)
	keepAlive(upstream)
	tunnel(client, upstream, cfg.idle)
}

// readHead reads up to the end of the request headers, bounded by
// maxHeaderBytes so a client cannot stream headers forever.
func readHead(c net.Conn) (string, error) {
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if i := strings.Index(string(buf), "\r\n\r\n"); i >= 0 {
			return string(buf[:i]), nil
		}
		if err != nil {
			return "", err
		}
		if len(buf) > maxHeaderBytes {
			return "", errors.New("header too large")
		}
	}
}

func splitHostPort(target string) (host, port string) {
	if h, p, err := net.SplitHostPort(target); err == nil {
		return h, p
	}
	return target, "443"
}

func keepAlive(c net.Conn) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	// Belt to the idle deadline's braces: the kernel probes a silent peer and
	// fails the socket, which surfaces as a read error rather than waiting out
	// the full idle window.
	_ = tcp.SetKeepAlive(true)
	_ = tcp.SetKeepAlivePeriod(keepAlivePeriod)
}

type closeWriter interface{ CloseWrite() error }

// tunnel splices both directions until both are done or one goes silent for
// longer than idle.
func tunnel(client, upstream net.Conn, idle time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		if err := copyIdle(dst, src, idle); err != nil {
			// A timeout or a hard error means nothing more will arrive on
			// either side worth waiting for: close both so the opposite
			// direction's Read returns instead of hanging.
			client.Close()
			upstream.Close()
			return
		}
		// EOF in ONE direction is a half-close, not the end of the tunnel: the
		// peer may still be streaming a response back. Forward the FIN and let
		// the other direction finish. (The Python version tore both sockets
		// down here, truncating responses to a client that had finished
		// sending.)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	wg.Wait()
}

// copyIdle copies until EOF (nil) or the first error, refreshing the deadline
// on every read and write so idle measures SILENCE rather than total duration.
func copyIdle(dst, src net.Conn, idle time.Duration) error {
	buf := make([]byte, 32<<10)
	for {
		if err := src.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := dst.SetWriteDeadline(time.Now().Add(idle)); err != nil {
				return err
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}
