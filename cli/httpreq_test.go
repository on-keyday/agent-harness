package cli

import (
	"strings"
	"testing"
)

func TestBuildHTTPRequestDefaults(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{Path: "/healthz"}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	want := "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nConnection: close\r\n\r\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildHTTPRequestBodyAndContentLength(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{
		Method: "POST", Path: "/api", Body: []byte(`{"a":1}`),
	}, "h", 80)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "Content-Length: 7\r\n") {
		t.Errorf("missing Content-Length:\n%s", s)
	}
	// Nothing may follow the body — an appended CRLF is exactly what stops
	// Content-Length matching what arrives.
	if !strings.HasSuffix(s, "\r\n\r\n"+`{"a":1}`) {
		t.Errorf("body must be last and unterminated:\n%q", s)
	}
	if !strings.Contains(s, "Host: h:80\r\n") {
		t.Errorf("port must be written even when 80:\n%s", s)
	}
}

func TestBuildHTTPRequestUserHeaderWins(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{
		Path:    "/x",
		Headers: []string{"host: example.com", "Connection: keep-alive", "Content-Length: 0"},
		Body:    []byte("ignored-by-length"),
	}, "127.0.0.1", 9)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "Host: 127.0.0.1:9") {
		t.Errorf("automatic Host overrode the user's:\n%s", s)
	}
	if strings.Count(s, "Connection:") != 1 || !strings.Contains(s, "Connection: keep-alive") {
		t.Errorf("automatic Connection was added anyway:\n%s", s)
	}
	if strings.Count(s, "Content-Length:") != 1 {
		t.Errorf("automatic Content-Length was added anyway:\n%s", s)
	}
}

func TestBuildHTTPRequestKeepsHeaderOrder(t *testing.T) {
	got, err := BuildHTTPRequest(HTTPRequestSpec{
		Path:    "/",
		Headers: []string{"B: 2", "A: 1", "", "C: 3"},
	}, "h", 1)
	if err != nil {
		t.Fatalf("BuildHTTPRequest: %v", err)
	}
	s := string(got)
	b, a, c := strings.Index(s, "B: 2"), strings.Index(s, "A: 1"), strings.Index(s, "C: 3")
	if !(b < a && a < c) {
		t.Errorf("header order not preserved (B=%d A=%d C=%d):\n%s", b, a, c, s)
	}
}

func TestBuildHTTPRequestRejectsInjection(t *testing.T) {
	cases := map[string]HTTPRequestSpec{
		"method":      {Method: "GET\r\nX: 1", Path: "/"},
		"path":        {Path: "/x\r\nX: 1"},
		"headervalue": {Path: "/", Headers: []string{"X: 1\r\nY: 2"}},
		"headername":  {Path: "/", Headers: []string{"X Y: 1"}},
		"nocolon":     {Path: "/", Headers: []string{"no-colon"}},
		"relpath":     {Path: "healthz"},
		"emptypath":   {Path: ""},
	}
	for name, spec := range cases {
		if got, err := BuildHTTPRequest(spec, "h", 1); err == nil {
			t.Errorf("%s: want an error, got none (built %q)", name, got)
		}
	}
}
