package main

// The credential injector: a plain-HTTP reverse proxy on loopback that
// forwards every request to ONE upstream with the provider credential put in
// its Authorization header. The agent is configured with this listener as its
// API base URL and a placeholder key, so the real credential is held only by
// processes the agent's uid cannot read: this one, under the proxy uid, and
// PID 1 (measured 2026-10-29: /proc/<pid>/environ of both refuse the agent
// uid; probe.sh's config_credential re-measures it).
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
// TLS where it was — between this process and the upstream.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// parseUpstream accepts an https origin and nothing else. A path would be
// joined in front of every request path, which nothing here needs (the agent's
// base URL carries the path), and plain http would put the credential on the
// wire in the clear.
func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("scheme must be https")
	case u.Host == "":
		return nil, errors.New("no host")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.User != nil:
		return nil, errors.New("must be an origin (https://host[:port]) alone")
	}
	u.Path = ""
	return u, nil
}

func newInjector(upstream *url.URL, key string, rt http.RoundTripper) http.Handler {
	return &httputil.ReverseProxy{
		// Rewrite, not Director: it adds no X-Forwarded-* headers, so the
		// upstream learns nothing about the container, and SetURL also sets
		// the outbound Host to the upstream's.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header.Set("Authorization", "Bearer "+key)
		},
		// No FlushInterval: ReverseProxy already flushes every write of a
		// text/event-stream or unknown-length response, which is what a
		// streamed completion is (measured: setting it changed nothing).
		Transport: rt,
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

// injectTransport dials tcp4 for the reason handle() does (a v6 attempt burns
// the whole connect timeout first). There is no response or idle deadline: a
// model may think for minutes before its first byte. A peer that vanished
// without a FIN is caught by TCP keepalive instead.
func injectTransport() *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlivePeriod}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: dialTimeout,
	}
}

func serveInject(ln net.Listener, upstream *url.URL, key string) {
	srv := &http.Server{
		Handler:           newInjector(upstream, key, injectTransport()),
		ReadHeaderTimeout: headerTimeout,
	}
	logf("inject: stopped: %v", srv.Serve(ln))
}
