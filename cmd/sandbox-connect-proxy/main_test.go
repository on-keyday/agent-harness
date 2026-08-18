package main

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPermitted(t *testing.T) {
	// api.anthropic.com is no longer a built-in default: it is claude's entry in
	// the wrapper's agent table, delivered through SANDBOX_AGENT_DOMAINS, which
	// this merged string stands in for.
	allow := loadAllow("api.anthropic.com,Example.COM, .research.example.org. , ")
	cases := []struct {
		host string
		want bool
	}{
		{"api.anthropic.com", true},
		{"API.Anthropic.COM", true},  // case-insensitive
		{"api.anthropic.com.", true}, // trailing root dot
		{"raw.githubusercontent.com", true},
		{"codeload.github.com", true},
		{"example.com", true}, // from the env list
		{"deep.research.example.org", true},
		{"notgithub.com", false}, // suffix must land on a label boundary
		{"github.com.evil.test", false},
		{"anthropic.com", false}, // parent of an entry is not an entry
		{"", false},
	}
	for _, c := range cases {
		if got := permitted(allow, c.host); got != c.want {
			t.Errorf("permitted(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestLoadAllowDedupesAndKeepsDefaults(t *testing.T) {
	allow := loadAllow("github.com,,  pypi.org  ")
	seen := map[string]int{}
	for _, d := range allow {
		seen[d]++
	}
	for d, n := range seen {
		if n != 1 {
			t.Errorf("%q appears %d times, want 1", d, n)
		}
	}
	// A shared default the env list did NOT name must survive it. (This used to
	// check api.anthropic.com, which is no longer a default — it is claude's
	// agent-table entry now, so it could not tell "extends the defaults" from
	// "the env happened to carry it".)
	if !permitted(allow, "files.pythonhosted.org") {
		t.Error("env list must extend the defaults, not replace them")
	}
}

// startProxy runs serve() on an ephemeral port and returns its address.
func startProxy(t *testing.T, cfg config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go serve(ln, cfg)
	return ln.Addr().String()
}

// connectVia opens a tunnel through the proxy and returns the client side plus
// the proxy's status line.
func connectVia(t *testing.T, proxyAddr, target string) (net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, strings.TrimSpace(status)
}

func TestConnectRefusals(t *testing.T) {
	addr := startProxy(t, config{allow: loadAllow(""), idle: time.Second, maxTunnels: 4})

	if _, status := connectVia(t, addr, "evil.test:443"); !strings.Contains(status, "403") {
		t.Errorf("non-allowlisted host: status = %q, want 403", status)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET http://github.com/ HTTP/1.1\r\nHost: github.com\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "405") {
		t.Errorf("plain-HTTP proxying: status = %q, want 405", status)
	}
}

// TestHalfCloseKeepsResponseFlowing is the regression the Python splice failed:
// a client that finishes sending must still receive the rest of the response.
func TestHalfCloseKeepsResponseFlowing(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	sawEOF := make(chan struct{})
	go func() {
		s, err := upstream.Accept()
		if err != nil {
			return
		}
		defer s.Close()
		if _, err := io.Copy(io.Discard, s); err != nil { // read until the client's half-close
			return
		}
		close(sawEOF)
		// The client is done sending; the response is still coming.
		_, _ = s.Write([]byte("late-response"))
	}()

	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	addr := startProxy(t, config{
		allow:      loadAllow("127.0.0.1"),
		idle:       5 * time.Second,
		maxTunnels: 4,
	})
	c, status := connectVia(t, addr, "127.0.0.1:"+port)
	if !strings.Contains(status, "200") {
		t.Fatalf("status = %q, want 200", status)
	}

	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the client's half-close: FIN was not forwarded")
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading after half-close: %v", err)
	}
	if string(got) != "late-response" {
		t.Errorf("got %q, want %q — the tunnel was torn down by the half-close", got, "late-response")
	}
}

// TestIdleTimeoutReclaimsSilentTunnel covers the stuck-forever case: a peer
// that stops speaking without FIN or RST must not hold the tunnel open.
func TestIdleTimeoutReclaimsSilentTunnel(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		s, err := upstream.Accept()
		if err != nil {
			return
		}
		// Hold the socket open and say nothing, ever.
		defer s.Close()
		<-time.After(30 * time.Second)
	}()

	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	addr := startProxy(t, config{
		allow:      loadAllow("127.0.0.1"),
		idle:       200 * time.Millisecond,
		maxTunnels: 4,
	})
	c, status := connectVia(t, addr, "127.0.0.1:"+port)
	if !strings.Contains(status, "200") {
		t.Fatalf("status = %q, want 200", status)
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("tunnel should have been closed by the idle deadline, got %v", err)
	}
}

func TestMaxTunnelsRefusesWithoutBlocking(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			s, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() { defer s.Close(); <-time.After(30 * time.Second) }()
		}
	}()

	_, port, _ := net.SplitHostPort(upstream.Addr().String())
	target := "127.0.0.1:" + port
	addr := startProxy(t, config{
		allow:      loadAllow("127.0.0.1"),
		idle:       10 * time.Second,
		maxTunnels: 1,
	})

	if _, status := connectVia(t, addr, target); !strings.Contains(status, "200") {
		t.Fatalf("first tunnel: status = %q, want 200", status)
	}
	if _, status := connectVia(t, addr, target); !strings.Contains(status, "503") {
		t.Errorf("second tunnel past the cap: status = %q, want 503", status)
	}
}

// TestLoadAllowMergesAgentAndTaskDomains pins that both env sources survive the
// merge. The failure this guards against is silent: if a per-task
// SANDBOX_PROXY_ALLOW replaced rather than extended the agent's list, the agent
// would lose its own API host the moment an operator named one WebFetch target,
// and the symptom would be a refused API call attributed to the firewall.
func TestLoadAllowMergesAgentAndTaskDomains(t *testing.T) {
	allow := loadAllow("api.anthropic.com" + "," + "research.example.org")
	for _, want := range []string{"api.anthropic.com", "research.example.org", "github.com"} {
		if !permitted(allow, want) {
			t.Errorf("loadAllow dropped %q; got %v", want, allow)
		}
	}
}
