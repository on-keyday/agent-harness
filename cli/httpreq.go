package cli

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// HTTPRequestSpec is what a surface collects from the operator before handing
// it to BuildHTTPRequest. Every surface builds through that one function, so
// the bytes on the wire cannot differ between the CLI, the TUI and the WebUI.
type HTTPRequestSpec struct {
	Method  string   // empty = GET
	Path    string   // "/healthz"; must start with "/" (or be "*")
	Headers []string // "Name: value", in order; suppresses the matching automatic header
	Body    []byte
}

// BuildHTTPRequest renders spec as HTTP/1.1 request bytes addressed to
// host:port — the target of the raw forward the bytes will be written to.
//
// Host, Content-Length and Connection are supplied automatically, and a
// user-supplied header of the same name always wins. Host carries the port even
// when it is 80: writing it unconditionally removes a "which form did it send?"
// question from the debugging sessions this exists for.
//
// CR or LF anywhere in the method, path, a header name or a header value is an
// error and nothing is built. That is the header-injection / request-smuggling
// boundary, and it is validation rather than sanitisation so the operator sees
// a message instead of silently different bytes.
//
// The returned bytes are complete: nothing may be appended to them, or
// Content-Length stops matching what the far end reads.
func BuildHTTPRequest(spec HTTPRequestSpec, host string, port int) ([]byte, error) {
	method := spec.Method
	if method == "" {
		method = "GET"
	}
	if !isHTTPToken(method) {
		return nil, fmt.Errorf("http: bad method %q", method)
	}
	if spec.Path != "*" && !strings.HasPrefix(spec.Path, "/") {
		return nil, fmt.Errorf("http: path %q must start with / (or be *)", spec.Path)
	}
	if strings.ContainsAny(spec.Path, "\r\n") {
		return nil, fmt.Errorf("http: path contains CR or LF")
	}

	type header struct{ name, value string }
	headers := make([]header, 0, len(spec.Headers))
	seen := make(map[string]bool, len(spec.Headers))
	for _, raw := range spec.Headers {
		if strings.TrimSpace(raw) == "" {
			continue // blank lines in a textarea are not headers
		}
		if strings.ContainsAny(raw, "\r\n") {
			return nil, fmt.Errorf("http: header %q contains CR or LF", raw)
		}
		name, value, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("http: header %q is not \"Name: value\"", raw)
		}
		name = strings.TrimSpace(name)
		if !isHTTPToken(name) {
			return nil, fmt.Errorf("http: bad header name %q", name)
		}
		headers = append(headers, header{name, strings.TrimSpace(value)})
		seen[strings.ToLower(name)] = true
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, spec.Path)
	if !seen["host"] {
		fmt.Fprintf(&b, "Host: %s:%d\r\n", host, port)
	}
	if len(spec.Body) > 0 && !seen["content-length"] {
		b.WriteString("Content-Length: " + strconv.Itoa(len(spec.Body)) + "\r\n")
	}
	if !seen["connection"] {
		b.WriteString("Connection: close\r\n")
	}
	for _, h := range headers {
		b.WriteString(h.name + ": " + h.value + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(spec.Body)
	return b.Bytes(), nil
}

// isHTTPToken reports whether s is a non-empty RFC 9110 token, which is what a
// method and a header name have to be.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	const extra = "!#$%&'*+-.^_`|~"
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(extra, r):
		default:
			return false
		}
	}
	return true
}
