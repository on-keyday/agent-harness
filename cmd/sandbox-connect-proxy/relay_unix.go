//go:build unix

package main

// relay: the HOST half of claude's credential injection, started by
// agent-in-podman.sh for one container and gone with it.
//
//	sandbox-connect-proxy relay --kind claude --socket PATH --upstream https://api.anthropic.com \
//	    --credentials ~/.claude/.credentials.json --agent-bin BIN --parent-pid PID --lock FILE
//
// It serves plain HTTP on a unix socket the wrapper bind-mounts into the
// container, where the proxy's inject listener forwards the agent's requests
// to it. For each request it asks its credential source for the current
// token and sends the request on with it, so the credential is never in the
// container. --kind picks the source (how the credential file reads, and how
// the agent is made to refresh it); claude is the one there is.
//
// claude's source never refreshes the token itself. The refresh token rotates on every use
// (a second client refreshing without writing back logs the CLI out), so the
// CLI stays its only owner: when the token is within refreshBefore of expiry,
// the relay runs `claude auth status --json` once, which refreshes and writes
// the file back (measured 2026-10-29, claude 2.1.312: with expiresAt one minute
// past, and again with three minutes left, both tokens changed and expiresAt
// moved 480 minutes out). Without that, a token nobody on the host refreshed
// expires after 8 hours and every request is a 401.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// credSource is what a --kind provides: the token for one request, and a
// check run at start so a relay that cannot serve says so while the wrapper
// is still waiting.
type credSource interface {
	key() (string, error)
	check() error
}

var relayKinds = map[string]func(path, agentBin, lock string, refreshBefore time.Duration) credSource{
	"claude": func(path, agentBin, lock string, refreshBefore time.Duration) credSource {
		return &claudeCreds{path: path, bin: agentBin, lock: lock, refreshBefore: refreshBefore, now: time.Now}
	},
}

type claudeCreds struct {
	path          string // claude's .credentials.json
	bin           string // the host claude, for `auth status`
	lock          string // flock'd across relays so they refresh once between them
	refreshBefore time.Duration
	now           func() time.Time
	mu            sync.Mutex
}

func (c *claudeCreds) read() (string, time.Time, error) {
	b, err := os.ReadFile(c.path)
	if err != nil {
		return "", time.Time{}, err
	}
	var f struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", c.path, err)
	}
	if f.OAuth.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("%s holds no claudeAiOauth.accessToken (is claude logged in with /login?)", c.path)
	}
	return f.OAuth.AccessToken, time.UnixMilli(f.OAuth.ExpiresAt), nil
}

func (c *claudeCreds) check() error { _, _, err := c.read(); return err }

func (c *claudeCreds) fresh(exp time.Time) bool { return c.now().Add(c.refreshBefore).Before(exp) }

// key returns the current access token, having the CLI refresh it first when
// it is close to expiry. A failed refresh still returns the token read: the
// upstream's answer (a 401) is more informative to the agent than a 502.
func (c *claudeCreds) key() (string, error) {
	tok, exp, err := c.read()
	if err != nil || c.fresh(exp) {
		return tok, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if tok, exp, err = c.read(); err != nil || c.fresh(exp) {
		return tok, err
	}
	if err := c.refresh(); err != nil {
		logf("WARN relay: refresh: %v", err)
	}
	tok, exp, err = c.read()
	if err == nil && !c.now().Before(exp) {
		logf("WARN relay: the access token is still expired after a refresh attempt")
	}
	return tok, err
}

func (c *claudeCreds) refresh() error {
	lf, err := os.OpenFile(c.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	// Another relay may have refreshed while this one waited for the lock.
	if _, exp, err := c.read(); err == nil && c.fresh(exp) {
		return nil
	}
	cmd := exec.Command(c.bin, "auth", "status", "--json")
	// Not the worktree: the container can write there, and a CLI started in a
	// directory reads that directory's project config.
	cmd.Dir = "/"
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		logf("relay: ran %s auth status (%v)", c.bin, err)
		return err
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("claude auth status timed out after 60s")
	}
}

func runRelay(args []string) int {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	kind := fs.String("kind", "", "credential source: claude")
	sock := fs.String("socket", "", "unix socket to serve on")
	up := fs.String("upstream", "", "https origin to forward to")
	creds := fs.String("credentials", "", "the agent's credential file")
	bin := fs.String("agent-bin", "", "the host agent binary the source has refresh the credential")
	ppid := fs.Int("parent-pid", 0, "exit once this pid is gone")
	lock := fs.String("lock", "", "flock file shared by every relay of this user")
	before := fs.Duration("refresh-before", 5*time.Minute, "have the agent refresh this long before expiry")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *kind == "" || *sock == "" || *up == "" || *creds == "" || *bin == "" || *ppid == 0 || *lock == "" {
		logf("FATAL relay: --kind, --socket, --upstream, --credentials, --agent-bin, --parent-pid and --lock are all required")
		return 2
	}
	newSource, ok := relayKinds[*kind]
	if !ok {
		logf("FATAL relay: unknown --kind %q", *kind)
		return 2
	}
	upstream, unixPath, err := parseUpstream(*up)
	if err != nil || unixPath != "" {
		logf("FATAL relay: --upstream %q must be an https origin (%v)", *up, err)
		return 2
	}
	c := newSource(*creds, *bin, *lock, *before)
	// Refuse at start, while the wrapper is still waiting and can say why,
	// rather than 502 every request of a container that has already started.
	if err := c.check(); err != nil {
		logf("FATAL relay: %v", err)
		return 1
	}
	// The wrapper starts this as a plain background job of a shell that may
	// sit on the agent's PTY; a ^C or a hangup meant for the agent must not
	// end the relay under it. Its end is the parent's.
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)

	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		logf("FATAL relay: listen %s: %v", *sock, err)
		return 1
	}
	// Reached through the bind mount by the container's proxy uid, which is a
	// different host uid. Access is gated by the directories around it: the
	// host one is mktemp's 0700, the container one is chmod'ed to the proxy
	// uid by entrypoint.sh.
	if err := os.Chmod(*sock, 0o666); err != nil {
		logf("FATAL relay: chmod %s: %v", *sock, err)
		return 1
	}
	go func() {
		for syscall.Kill(*ppid, 0) == nil {
			time.Sleep(time.Second)
		}
		logf("relay: parent %d gone; exiting", *ppid)
		_ = os.Remove(*sock)
		os.Exit(0)
	}()
	logf("relay: serving %s -> %s, credentials %s", *sock, upstream, *creds)
	srv := &http.Server{
		Handler:           newInjector(upstream, c.key, injectTransport("")),
		ReadHeaderTimeout: headerTimeout,
	}
	logf("relay: stopped: %v", srv.Serve(ln))
	return 1
}
