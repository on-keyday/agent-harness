package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// httpFetchMaxBody bounds the body handed back to a caller that must hold the
// whole thing in memory (the WebUI preview does). It is deliberately NOT
// PREVIEW_MAX_BYTES: that is a file-preview limit for bytes shown as text, and
// an API response is a different thing with a different sane bound.
const httpFetchMaxBody = 8 << 20

// httpFetchMaxRaw bounds what is accumulated off the wire, not just what is
// parsed out of it. Without it a far end that streams forever grows the buffer
// without limit. The slack above the body cap covers the status line and
// headers.
const httpFetchMaxRaw = httpFetchMaxBody + 64<<10

// HTTPFetchResult is one parsed HTTP response. Headers keep wire order and
// allow repeats, so Set-Cookie and friends survive; a map would not.
type HTTPFetchResult struct {
	Status     int
	StatusText string
	Headers    [][2]string
	Body       []byte
	Truncated  bool
}

// fetchControlledHeaders are the names the caller of HTTPFetch may not supply.
// Host and Content-Length are derived from the forward and from the body;
// Connection must stay "close" because the read below runs to EOF and a
// keep-alive response would park it until the far end times out;
// X-Harness-Preview is a marker this side stamps, so letting it be overridden
// would make it worthless.
var fetchControlledHeaders = []string{"host", "connection", "content-length", "x-harness-preview"}

// sanitizeFetchHeaders drops the controlled names. It filters rather than
// erroring: these headers arrive from a previewed page's fetch() init, not
// from an operator typing them, so there is nobody to show a message to.
func sanitizeFetchHeaders(in []string) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		if name, _, ok := strings.Cut(h, ":"); ok {
			lower := strings.ToLower(strings.TrimSpace(name))
			controlled := false
			for _, c := range fetchControlledHeaders {
				if lower == c {
					controlled = true
					break
				}
			}
			if controlled {
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

// parseHTTPFetchResponse turns raw response bytes into a value.
//
// The synthetic request is load-bearing: given a nil *http.Request,
// ReadResponse cannot know the method and frames a HEAD response as though the
// Content-Length it advertises introduced a real body.
func parseHTTPFetchResponse(method string, raw []byte) (*HTTPFetchResult, error) {
	req, err := http.NewRequest(method, "http://forward/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// One byte past the cap, so a body that exactly fills it is not mislabelled
	// as truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpFetchMaxBody+1))
	if err != nil {
		return nil, err
	}
	truncated := false
	if len(body) > httpFetchMaxBody {
		body = body[:httpFetchMaxBody]
		truncated = true
	}

	headers := make([][2]string, 0, len(resp.Header))
	for name, values := range resp.Header {
		for _, v := range values {
			headers = append(headers, [2]string{name, v})
		}
	}

	return &HTTPFetchResult{
		Status:     resp.StatusCode,
		StatusText: strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode))),
		Headers:    headers,
		Body:       body,
		Truncated:  truncated,
	}, nil
}

// buildFetchRequest renders the request bytes and returns the method the
// response has to be framed against. Shared so a pin-scoped fetch and a one-off
// HTTPFetch cannot drift into sending different bytes for the same spec.
func buildFetchRequest(spec HTTPRequestSpec, host string, port int) ([]byte, string, error) {
	spec.Headers = append(sanitizeFetchHeaders(spec.Headers), "X-Harness-Preview: 1")
	req, err := BuildHTTPRequest(spec, host, port)
	if err != nil {
		return nil, "", err
	}
	method := spec.Method
	if method == "" {
		method = "GET"
	}
	return req, method, nil
}

// readFetchResponse accumulates the reply until the far end closes and parses
// it. recv has the shape of both RawConn.Recv and trsf's ReadDirectContext, so
// the two callers differ only in which stream they hand over.
func readFetchResponse(method string, recv func() ([]byte, bool, error)) (*HTTPFetchResult, error) {
	var buf bytes.Buffer
	for {
		data, eof, rerr := recv()
		if len(data) > 0 {
			if buf.Len() >= httpFetchMaxRaw {
				break
			}
			buf.Write(data)
		}
		if eof {
			break
		}
		if rerr != nil {
			// Nothing arrived at all: report the read error rather than a parse
			// failure, which would blame the far end's HTTP for a transport
			// problem.
			if buf.Len() == 0 {
				return nil, rerr
			}
			break
		}
	}
	return parseHTTPFetchResponse(method, buf.Bytes())
}

// HTTPFetch sends one request over a raw forward and returns the parsed
// response. It is the portable counterpart to RunHTTPRequestForward, which
// copies raw bytes to an io.Writer for the CLI: the two share a build step and
// differ in every part that matters, so they stay separate rather than sharing
// a helper across the js / !js build-tag boundary.
//
// The spec is validated and built before anything is dialled, so a bad request
// costs an error message rather than a forward that is established, registered
// and then torn down.
func HTTPFetch(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec) (*HTTPFetchResult, error) {
	req, method, err := buildFetchRequest(spec, host, port)
	if err != nil {
		return nil, err
	}
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if err := rc.Send(req); err != nil {
		return nil, err
	}
	return readFetchResponse(method, func() ([]byte, bool, error) { return rc.Recv(ctx) })
}
