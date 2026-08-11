package cli

import (
	"strconv"
	"strings"
	"testing"
)

// The page controls spec.Headers. BuildHTTPRequest lets a user-supplied header
// win over the automatic one, so anything that would displace Host /
// Connection / Content-Length, or forge the preview marker, has to be dropped
// before it gets there.
func TestSanitizeFetchHeadersDropsControlledNames(t *testing.T) {
	got := sanitizeFetchHeaders([]string{
		"Host: evil.example",
		"connection: keep-alive",
		"Content-Length: 999",
		"x-harness-preview: 0",
		"X-App: keep",
	})
	if len(got) != 1 || got[0] != "X-App: keep" {
		t.Fatalf("want only the app header, got %q", got)
	}
}

func TestParseHTTPFetchResponseContentLength(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.StatusText != "OK" {
		t.Fatalf("status = %d %q", res.Status, res.StatusText)
	}
	if string(res.Body) != "{}" {
		t.Fatalf("body = %q", res.Body)
	}
	var ct string
	for _, h := range res.Headers {
		if strings.EqualFold(h[0], "Content-Type") {
			ct = h[1]
		}
	}
	if ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
}

// Chunked must be de-chunked, not handed to the page verbatim.
func TestParseHTTPFetchResponseChunked(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "hello" {
		t.Fatalf("body = %q", res.Body)
	}
}

// A HEAD response has no body however its headers are framed. Parsing it with
// a nil request makes ReadResponse believe a body is coming and the read
// mis-frames, so the method must be carried into the parse.
func TestParseHTTPFetchResponseHEADHasNoBody(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n"
	res, err := parseHTTPFetchResponse("HEAD", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 0 {
		t.Fatalf("HEAD body = %q, want empty", res.Body)
	}
}

func TestParseHTTPFetchResponseNoContent(t *testing.T) {
	res, err := parseHTTPFetchResponse("GET", []byte("HTTP/1.1 204 No Content\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 204 || len(res.Body) != 0 {
		t.Fatalf("got %d / %q", res.Status, res.Body)
	}
	if res.StatusText != "No Content" {
		t.Fatalf("status text = %q", res.StatusText)
	}
}

// Oversize bodies are truncated rather than refused: a page that asks for
// something huge should see the prefix and a flag, not lose the response.
func TestParseHTTPFetchResponseTruncatesOversizeBody(t *testing.T) {
	body := strings.Repeat("a", httpFetchMaxBody+100)
	raw := "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != httpFetchMaxBody || !res.Truncated {
		t.Fatalf("len=%d truncated=%v", len(res.Body), res.Truncated)
	}
}

// A body that exactly fills the cap is complete, not truncated.
func TestParseHTTPFetchResponseExactCapIsNotTruncated(t *testing.T) {
	body := strings.Repeat("a", httpFetchMaxBody)
	raw := "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	res, err := parseHTTPFetchResponse("GET", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != httpFetchMaxBody || res.Truncated {
		t.Fatalf("len=%d truncated=%v", len(res.Body), res.Truncated)
	}
}

// A bad spec must be rejected before anything is dialled — the same property
// TestRunHTTPRequestForwardValidatesBeforeDial asserts for the CLI path. A nil
// *Client is safe only while validation runs first; if it stops running first
// this dereferences and the test fails loudly.
func TestHTTPFetchValidatesBeforeDial(t *testing.T) {
	if _, err := HTTPFetch(t.Context(), nil, "task", "h", 1, HTTPRequestSpec{Path: "relative"}); err == nil {
		t.Fatal("want a validation error, got nil")
	}
}
