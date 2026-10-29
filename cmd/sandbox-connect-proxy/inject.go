package main

// The credential injector: a plain-HTTP reverse proxy on loopback that
// forwards every request to ONE upstream with the provider credential put in
// its Authorization header. The agent is configured with this listener as its
// API base URL and a placeholder key, so the real credential is held only by
// processes the agent's uid cannot read: this one, under the proxy uid, and
// PID 1 (measured 2026-10-29: /proc/<pid>/environ of both refuse the agent
// uid; probe.sh's config_credential re-measures it).
//
// Or by neither: with a `unix:` upstream this listener adds nothing and
// forwards to a socket the wrapper bind-mounts from the HOST, where the same
// binary's relay mode (relay_unix.go) adds the credential and makes the TLS
// connection. claude is wired that way, because its credential is a short-lived
// token the host's own claude keeps refreshing, which nothing inside the
// container could read fresh.
//
// What it does NOT stop: the agent can still SPEND the credential for as long
// as the container runs, on any path of that upstream. What it stops is the
// credential leaving the container to be used afterwards or elsewhere — under
// mount auth the key file is readable and every allowlisted host (github.com
// among them) is a way out.
//
// Plain HTTP on loopback rather than CONNECT: a CONNECT tunnel carries TLS end
// to end, so a header can only be added by terminating that TLS (a MITM CA in
// the agent's trust store). Pointing the agent at http://127.0.0.1 keeps the
// TLS where it was — between this process (or the relay) and the upstream.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
)

// keyFunc returns the credential for one request. Called per request, so a
// source that changes underneath (the relay's credential file) is read fresh.
type keyFunc func() (string, error)

func staticKey(k string) keyFunc { return func() (string, error) { return k, nil } }

// relayHost is the Host the container side sends to the relay socket. The
// relay replaces it with the upstream's; it only has to be a valid name.
const relayHost = "sandbox-relay"

// parseUpstream accepts an https origin, or `unix:<absolute path>` for the
// host relay, and nothing else. A path would be joined in front of every
// request path, which nothing here needs (the agent's base URL carries the
// path), and plain http to a remote host would put the credential on the wire
// in the clear. For unix it returns the socket path and an http target whose
// host is relayHost.
func parseUpstream(s string) (target *url.URL, unixPath string, err error) {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutPrefix(s, "unix:"); ok {
		if !filepath.IsAbs(p) {
			return nil, "", errors.New("unix: needs an absolute socket path")
		}
		return &url.URL{Scheme: "http", Host: relayHost}, p, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, "", err
	}
	switch {
	case u.Scheme != "https":
		return nil, "", errors.New("scheme must be https (or unix:<path> for the relay)")
	case u.Host == "":
		return nil, "", errors.New("no host")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.User != nil:
		return nil, "", errors.New("must be an origin (https://host[:port]) alone")
	}
	u.Path = ""
	return u, "", nil
}

// newInjector forwards to upstream through rt. With key nil it forwards the
// request with the agent's own Authorization removed (the relay adds the real
// one); otherwise it sets Authorization: Bearer <key()> on each request.
func newInjector(upstream *url.URL, key keyFunc, rt http.RoundTripper) http.Handler {
	return &httputil.ReverseProxy{
		// Rewrite, not Director: it adds no X-Forwarded-* headers, so the
		// upstream learns nothing about the container, and SetURL also sets
		// the outbound Host to the upstream's.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("X-Api-Key")
		},
		// No FlushInterval: ReverseProxy already flushes every write of a
		// text/event-stream or unknown-length response, which is what a
		// streamed completion is (measured: setting it changed nothing).
		Transport: authTransport{rt, key},
		ModifyResponse: func(resp *http.Response) error {
			logf("INJECT %s %s %s -> %d", resp.Request.Method, upstream.Host, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logf("FAIL  inject %s %s %s (%v)", r.Method, upstream.Host, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// authTransport adds the credential at send time rather than in Rewrite, so a
// key source that fails (an unreadable credential file) fails the request with
// a 502 and a logged reason instead of sending it without one.
type authTransport struct {
	base http.RoundTripper
	key  keyFunc
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.key == nil {
		return t.base.RoundTrip(r)
	}
	k, err := t.key()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+k)
	return t.base.RoundTrip(r)
}

// injectTransport dials tcp4 for the reason handle() does (a v6 attempt burns
// the whole connect timeout first), or the relay's socket when unixPath is
// set. There is no response or idle deadline: a model may think for minutes
// before its first byte. A peer that vanished without a FIN is caught by TCP
// keepalive instead.
func injectTransport(unixPath string) *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlivePeriod}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			if unixPath != "" {
				return d.DialContext(ctx, "unix", unixPath)
			}
			return d.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:   unixPath == "",
		TLSHandshakeTimeout: dialTimeout,
	}
}

func serveInject(ln net.Listener, upstream *url.URL, unixPath string, key keyFunc) {
	srv := &http.Server{
		Handler:           newInjector(upstream, key, injectTransport(unixPath)),
		ReadHeaderTimeout: headerTimeout,
	}
	logf("inject: stopped: %v", srv.Serve(ln))
}
