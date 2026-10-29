package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startInjector fronts upstream with the injector and returns its base URL.
func startInjector(t *testing.T, upstream *httptest.Server, key string) string {
	t.Helper()
	u, err := parseUpstream(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(newInjector(u, key, upstream.Client().Transport))
	t.Cleanup(front.Close)
	return front.URL
}

func TestInjectReplacesTheAgentsAuthorization(t *testing.T) {
	type seen struct{ auth, host, path, xff string }
	got := make(chan seen, 1)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.Host, r.URL.Path, r.Header.Get("X-Forwarded-For")}
	}))
	defer up.Close()
	base := startInjector(t, up, "real-key")

	req, _ := http.NewRequest("POST", base+"/api/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sandbox-placeholder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s := <-got
	if s.auth != "Bearer real-key" {
		t.Errorf("upstream saw Authorization %q, want the injected key", s.auth)
	}
	if want := strings.TrimPrefix(up.URL, "https://"); s.host != want {
		t.Errorf("upstream saw Host %q, want %q (not the loopback listener's)", s.host, want)
	}
	if s.path != "/api/v1/chat/completions" {
		t.Errorf("upstream saw path %q", s.path)
	}
	if s.xff != "" {
		t.Errorf("X-Forwarded-For %q reached the upstream", s.xff)
	}
}

// A streamed completion must reach the agent event by event, not when the
// upstream finishes.
func TestInjectStreamsResponses(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("data: second\n\n"))
	}))
	defer up.Close()
	defer close(release)
	base := startInjector(t, up, "k")

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: first\n" {
			t.Errorf("first line %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive while the upstream held the response open")
	}
}

func TestInjectUpstreamFailureIs502(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := startInjector(t, up, "k")
	up.Close()
	resp, err := http.Get(base + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}

func TestParseUpstream(t *testing.T) {
	for _, s := range []string{"https://openrouter.ai", "https://openrouter.ai/", "https://127.0.0.1:8443"} {
		u, err := parseUpstream(s)
		if err != nil {
			t.Errorf("%q: %v", s, err)
			continue
		}
		if u.Path != "" {
			t.Errorf("%q: path %q kept", s, u.Path)
		}
	}
	for _, s := range []string{
		"http://openrouter.ai",         // the key would cross the wire in clear
		"https://openrouter.ai/api/v1", // a path belongs in the agent's base URL
		"https://openrouter.ai?x=1",
		"https://u:p@openrouter.ai",
		"https://",
		"openrouter.ai",
	} {
		if _, err := parseUpstream(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
