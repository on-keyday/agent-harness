//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// pathFront is one TLS listener that routes /a/ws and /b/ws to two
// listen-mode runners — several runners on one IP:port, told apart only by
// what the dial sends, as behind a CDN edge or a virtual host
// (docs/superpowers/specs/2026-11-21-named-websocket-dial-targets-design.md).
type pathFront struct {
	ts   *httptest.Server
	mu   sync.Mutex
	seen []string // "<Host> <SNI> <path>" per request
}

func newPathFront(t *testing.T, routes map[string]string) *pathFront {
	t.Helper()
	f := &pathFront{}
	proxies := map[string]*httputil.ReverseProxy{}
	for prefix, upstream := range routes {
		u, err := url.Parse("http://" + upstream)
		if err != nil {
			t.Fatal(err)
		}
		p := httputil.NewSingleHostReverseProxy(u)
		prefix := prefix
		inner := p.Director
		p.Director = func(r *http.Request) {
			inner(r)
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		}
		proxies[prefix] = p
	}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Host+" "+r.TLS.ServerName+" "+r.URL.Path)
		f.mu.Unlock()
		for prefix, p := range proxies {
			if strings.HasPrefix(r.URL.Path, prefix+"/") {
				p.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *pathFront) port() string {
	_, p, _ := strings.Cut(f.ts.Listener.Addr().String(), ":")
	return p
}

func (f *pathFront) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func TestNamedDialRecordsBehindOneAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash")
	}
	serverAddr, addrA, addrB := freePort(t), freePort(t), freePort(t)
	dataDir := t.TempDir()
	dialsPath := filepath.Join(dataDir, "runner-dials.json")
	repo := tempRepo(t)

	startListen := func(addr, host string) context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			_ = runner.ListenAndServe(ctx, runner.ListenConfig{
				Config: runner.Config{RunnerKey: testRunnerKey, RunnerID: runner.NewRunnerID(),
					Tasks: runner.NewTaskRegistry(), ProcessCtx: ctx,
					AllowedRoots: []string{repo}, MaxTasks: 1, Hostname: host,
					Profiles: singleAgentProfile(fakeClaudePath(t)),
					OnLeave:  func(string) { cancel() }}, // what agent-runner's main does on a kick
				WSListen: addr,
			})
		}()
		return cancel
	}
	cancelA := startListen(addrA, "named-a")
	defer cancelA()
	cancelB := startListen(addrB, "named-b")
	defer cancelB()

	front := newPathFront(t, map[string]string{"/a": addrA, "/b": addrB})
	urlA := "wss://localhost:" + front.port() + "/a/ws"
	urlB := "wss://localhost:" + front.port() + "/b/ws"

	dials, err := server.LoadRunnerDialStore(dialsPath)
	if err != nil {
		t.Fatal(err)
	}
	key := testRunnerKey.Public().(ed25519.PublicKey) // both runners sign with it
	for _, u := range []string{urlA, urlB} {
		if _, err := dials.Add(server.RunnerDialRecord{Candidates: []string{u}, Key: key}); err != nil {
			t.Fatal(err)
		}
	}

	startServer := func() (*server.Server, *server.RunnerDialStore, func()) {
		st, err := server.LoadRunnerDialStore(dialsPath)
		if err != nil {
			t.Fatal(err)
		}
		s := server.New(server.Config{RunnerKeys: testRunnerKeys(), RunnerDials: st, Addr: serverAddr, DataDir: dataDir})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		return s, st, func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("server did not stop")
			}
		}
	}
	runnerOn := func(s *server.Server, host string) (server.RunnerEntry, bool) {
		for _, e := range s.RegisteredRunners() {
			if e.Hostname == host {
				return e, true
			}
		}
		return server.RunnerEntry{}, false
	}
	bothRegistered := func(s *server.Server) bool {
		a, okA := runnerOn(s, "named-a")
		b, okB := runnerOn(s, "named-b")
		return okA && okB && a.Link == protocol.RunnerLink_DialRecord && b.Link == protocol.RunnerLink_DialRecord
	}

	s1, _, stop1 := startServer()
	eventually(t, func() bool { return bothRegistered(s1) }, 20*time.Second, 100*time.Millisecond,
		"both records connect their own runner behind one address")

	// 1. Two runners, two connections, each with its own URL.
	a, _ := runnerOn(s1, "named-a")
	b, _ := runnerOn(s1, "named-b")
	if a.ID == b.ID {
		t.Fatalf("both runners share cid %s", a.ID)
	}
	if a.LinkTarget != urlA || b.LinkTarget != urlB {
		t.Fatalf("link targets a=%q b=%q, want %q and %q", a.LinkTarget, b.LinkTarget, urlA, urlB)
	}
	if _, named := objproto.NamedFromAddrPort(a.ID.Addr); !named || a.ID.Transport != "wss" {
		t.Fatalf("runner a's cid %s is not a named wss address", a.ID)
	}

	// 2. The front saw the host name, not an IP, in Host and SNI.
	for _, r := range front.requests() {
		if !strings.HasPrefix(r, "localhost:"+front.port()+" localhost /") {
			t.Fatalf("front saw %q, want Host localhost:%s and SNI localhost", r, front.port())
		}
	}

	// 3. A task runs on each.
	c := dialClient(t, objproto.MustParseConnectionID("ws:"+serverAddr+"-1"))
	for _, host := range []string{"named-a", "named-b"} {
		sel, err := cli.BuildSelector(cli.SelectorOpts{Host: host})
		if err != nil {
			t.Fatal(err)
		}
		id, err := openOneshotWith(context.Background(), c, repo, "echo on "+host, cli.SessionOpts{Selector: sel})
		if err != nil {
			t.Fatalf("oneshot on %s: %v", host, err)
		}
		waitTaskTerminal(t, c, id, 30*time.Second)
		if st := getTask(t, c, id).Status; st != protocol.TaskStatus_Succeeded {
			t.Fatalf("task on %s: %v, want Succeeded", host, st)
		}
	}
	c.Close()

	// 4. A restarted server re-registers both URLs and dials them again.
	stop1()
	s2, st2, stop2 := startServer()
	defer stop2()
	eventually(t, func() bool { return bothRegistered(s2) }, 20*time.Second, 100*time.Millisecond,
		"the restarted server dials both records again")
	a2, _ := runnerOn(s2, "named-a")
	b2, _ := runnerOn(s2, "named-b")
	if a2.LinkTarget != urlA || b2.LinkTarget != urlB {
		t.Fatalf("after restart link targets a=%q b=%q", a2.LinkTarget, b2.LinkTarget)
	}
	if a2.ID.Addr != a.ID.Addr || b2.ID.Addr != b.ID.Addr {
		t.Fatalf("a URL's address changed across a restart: a %s→%s b %s→%s", a.ID, a2.ID, b.ID, b2.ID)
	}

	// 5. One runner going away does not take the other's connection.
	cancelB()
	eventually(t, func() bool { _, ok := runnerOn(s2, "named-b"); return !ok }, 90*time.Second, 200*time.Millisecond,
		"runner b deregisters")
	if _, ok := runnerOn(s2, "named-a"); !ok {
		t.Fatal("runner a went with runner b")
	}

	// 6. Kicking runner a removes its record only.
	c2 := dialClient(t, objproto.MustParseConnectionID("ws:"+serverAddr+"-2"))
	defer c2.Close()
	status, _, err := cli.RunnerKickWith(context.Background(), c2, a2.Identity.Hex(), false)
	if err != nil || status != protocol.RunnerKickStatus_Left {
		t.Fatalf("kick a: %v %v", status, err)
	}
	var left []string
	for _, r := range st2.List() {
		left = append(left, r.Candidates...)
	}
	if len(left) != 1 || left[0] != urlB {
		t.Fatalf("records after kicking a: %q, want only %q", left, urlB)
	}
}
