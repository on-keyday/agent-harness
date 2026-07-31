package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

// TestPortForwardE2E exercises the full client→server→runner→remoteHost
// port-forward path:
//  1. boots server + runner with fake-claude-slow.sh so the task stays Running
//  2. starts a local in-process echo TCP server
//  3. calls cli.RunForward in a goroutine and waits for the local listener to
//     come up
//  4. asserts a byte round-trip through the forward
//  5. asserts two concurrent connections are independent (concurrency)
//  6. asserts a forward to a definitely-closed port propagates EOF promptly
//  7. cancels the context and asserts the local listener stops accepting
func TestPortForwardE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18547"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "pf-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Wait until the runner has the task registered (worktree appears).
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}
	t.Logf("worktree ready at %s", worktree)

	// --- Echo server: accept loop doing io.Copy(conn, conn) ---
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn) //nolint:errcheck
			}()
		}
	}()
	t.Logf("echo server on port %d", echoPort)

	// --- Pick a free local port for the forward ---
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port listen: %v", err)
	}
	freePort := freeLn.Addr().(*net.TCPAddr).Port
	freeLn.Close()
	t.Logf("forward local port %d", freePort)

	// --- Pick another free port that will be closed (for dial-failure test) ---
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("closed port listen: %v", err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	closedLn.Close()

	// --- Pick a second free local port for the closed-remote forward ---
	freeLn2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port 2 listen: %v", err)
	}
	freePort2 := freeLn2.Addr().(*net.TCPAddr).Port
	freeLn2.Close()

	// Dial the server as a CLI client.
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Build two forward specs: one to the echo server, one to a closed port.
	specs := []cli.ForwardSpec{
		{BindAddr: "127.0.0.1", LocalPort: freePort, RemoteHost: "127.0.0.1", RemotePort: echoPort},
		{BindAddr: "127.0.0.1", LocalPort: freePort2, RemoteHost: "127.0.0.1", RemotePort: closedPort},
	}

	fwdCtx, fwdCancel := context.WithCancel(ctx)
	fwdDone := make(chan error, 1)
	go func() {
		fwdDone <- cli.RunForward(fwdCtx, c, taskID, specs, nil, nil)
	}()

	// Poll until the forward listener is up (retry-dial).
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort))
	deadline = time.Now().Add(5 * time.Second)
	var dialOK bool
	for time.Now().Before(deadline) {
		tc, err := net.DialTimeout("tcp", localAddr, 100*time.Millisecond)
		if err == nil {
			tc.Close()
			dialOK = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dialOK {
		t.Fatalf("forward listener on %s did not come up within 5s", localAddr)
	}
	t.Log("forward listener is up")

	// --- Assert 1: byte round-trip ---
	t.Run("roundtrip", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial forward: %v", err)
		}
		defer conn.Close()
		msg := []byte("ping\n")
		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("readfull: %v", err)
		}
		if string(buf) != string(msg) {
			t.Errorf("echo mismatch: got %q want %q", buf, msg)
		}
	})

	// --- Assert 2: concurrency (two independent connections) ---
	t.Run("concurrency", func(t *testing.T) {
		type result struct {
			got []byte
			err error
		}
		ch := make(chan result, 2)
		sendRecv := func(payload string) {
			conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
			if err != nil {
				ch <- result{err: err}
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(payload)); err != nil {
				ch <- result{err: err}
				return
			}
			buf := make([]byte, len(payload))
			_, err = io.ReadFull(conn, buf)
			ch <- result{got: buf, err: err}
		}
		go sendRecv("hello1\n")
		go sendRecv("hello2\n")
		for i := 0; i < 2; i++ {
			r := <-ch
			if r.err != nil {
				t.Errorf("concurrent conn %d: %v", i, r.err)
			}
		}
	})

	// --- Assert 3: dial-failure — forward to a closed remote port propagates EOF ---
	t.Run("dial_failure", func(t *testing.T) {
		closedAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort2))
		// The forward listener on freePort2 should be up now too (RunForward
		// starts all listeners before blocking). Poll until it is up.
		deadline2 := time.Now().Add(2 * time.Second)
		var listenerUp bool
		for time.Now().Before(deadline2) {
			tc, err := net.DialTimeout("tcp", closedAddr, 100*time.Millisecond)
			if err == nil {
				tc.Close()
				listenerUp = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !listenerUp {
			t.Fatalf("forward listener for closed-remote spec on %s did not come up within 2s", closedAddr)
		}
		conn, err := net.DialTimeout("tcp", closedAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial closed-remote forward: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		_, err = conn.Read(buf)
		if err == nil {
			t.Errorf("expected EOF/error from closed-remote forward, got nil")
		}
		// err should be io.EOF or a net error — either is acceptable.
	})

	// --- Assert 4: cancel stops the listener ---
	t.Run("cancel_stops_listener", func(t *testing.T) {
		fwdCancel()
		// Give RunForward time to close the listeners.
		select {
		case <-fwdDone:
		case <-time.After(3 * time.Second):
			t.Log("RunForward did not return within 3s of cancel (goroutine may be stuck)")
		}
		// A subsequent dial to the local forward address must now fail.
		deadline3 := time.Now().Add(2 * time.Second)
		var lastErr error
		for time.Now().Before(deadline3) {
			tc, err := net.DialTimeout("tcp", localAddr, 100*time.Millisecond)
			if err != nil {
				lastErr = err
				break
			}
			tc.Close()
			time.Sleep(50 * time.Millisecond)
		}
		if lastErr == nil {
			t.Errorf("forward listener still accepting after cancel")
		}
	})

	// Tear down.
	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}

// TestRemotePortForwardE2E exercises the full ssh -R path: the runner listens,
// and a connection to its bound port is dialed back out by the client to a
// client-side echo server.
//  1. boots server + runner with fake-claude-slow.sh so the task stays Running
//  2. starts a client-side echo TCP server (the dial target)
//  3. registers a remote forward (runner binds a free port) via cli.RunRemoteForward
//  4. dials the runner-bound port and asserts a byte round-trip through the tunnel
func TestRemotePortForwardE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18548"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "rpf-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	// Client-side echo server = the dial target.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn) //nolint:errcheck
			}()
		}
	}()

	// A free port for the runner to listen on.
	bindLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind port listen: %v", err)
	}
	runnerPort := bindLn.Addr().(*net.TCPAddr).Port
	bindLn.Close()

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	spec := cli.RemoteForwardSpec{BindAddr: "127.0.0.1", RunnerPort: runnerPort, DialHost: "127.0.0.1", DialPort: echoPort}
	fwdCtx, fwdCancel := context.WithCancel(ctx)
	defer fwdCancel()
	fwdDone := make(chan error, 1)
	go func() { fwdDone <- cli.RunRemoteForward(fwdCtx, c, taskID, []cli.RemoteForwardSpec{spec}, nil) }()

	// Poll until the runner has bound its listener (registration round-trips
	// through server→runner, then the runner binds).
	runnerAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(runnerPort))
	deadline = time.Now().Add(8 * time.Second)
	var up bool
	for time.Now().Before(deadline) {
		tc, err := net.DialTimeout("tcp", runnerAddr, 100*time.Millisecond)
		if err == nil {
			tc.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatalf("runner listener on %s did not come up within 8s", runnerAddr)
	}

	t.Run("roundtrip", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", runnerAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial runner port: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		msg := []byte("ping\n")
		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("readfull: %v", err)
		}
		if string(buf) != string(msg) {
			t.Errorf("echo mismatch through remote forward: got %q want %q", buf, msg)
		}
	})

	// --- A bind failure on the runner must surface to the client (not a silent
	// success): occupy the port on the runner host so net.Listen fails, then
	// register → expect a BindFailed error from OpenRemoteForward.
	t.Run("bind_failure_surfaces", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("occupy listen: %v", err)
		}
		defer occupied.Close()
		occPort := occupied.Addr().(*net.TCPAddr).Port
		_, _, err = c.OpenRemoteForward(ctx, taskID, cli.RemoteForwardSpec{
			BindAddr: "127.0.0.1", RunnerPort: occPort, DialHost: "127.0.0.1", DialPort: echoPort,
		})
		if err == nil {
			t.Fatal("expected an error when the runner port is already in use, got nil (silent success)")
		}
		if !strings.Contains(err.Error(), "bind") {
			t.Errorf("error %q should mention the bind failure", err)
		}
	})

	// --- A second forward failing must NOT break the first one ("巻き添え"). The
	// first forward (still running on runnerAddr) must still round-trip after the
	// failed registration above.
	t.Run("first_survives_second_failure", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", runnerAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial first forward after second failed: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte("pong\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("first forward broke after second's bind failure: %v", err)
		}
		if string(buf) != "pong\n" {
			t.Errorf("echo mismatch: got %q", buf)
		}
	})

	// --- A dead dial target must close the runner-side connection promptly,
	// not hang: connect to forward B (whose client-side target is down) and the
	// read must return an error well before the deadline.
	t.Run("dial_failure_closes_promptly", func(t *testing.T) {
		dl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		deadPort := dl.Addr().(*net.TCPAddr).Port
		dl.Close() // nothing listens here now → client dial will be refused

		bl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		bindPort2 := bl.Addr().(*net.TCPAddr).Port
		bl.Close()

		bCtx, bCancel := context.WithCancel(ctx)
		defer bCancel()
		var logmu sync.Mutex
		var logs []string
		go cli.RunRemoteForward(bCtx, c, taskID, []cli.RemoteForwardSpec{
			{BindAddr: "127.0.0.1", RunnerPort: bindPort2, DialHost: "127.0.0.1", DialPort: deadPort},
		}, func(s string) { logmu.Lock(); logs = append(logs, s); logmu.Unlock() })

		bAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(bindPort2))
		deadline := time.Now().Add(8 * time.Second)
		var up bool
		for time.Now().Before(deadline) {
			if tc, e := net.DialTimeout("tcp", bAddr, 100*time.Millisecond); e == nil {
				tc.Close()
				up = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !up {
			t.Fatal("forward B listener did not come up")
		}

		// Fire many connections concurrently to expose any close-propagation
		// race (a single connection always closes promptly in-process; the real
		// hang is timing-dependent).
		const n = 40
		var wg sync.WaitGroup
		elapsed := make([]time.Duration, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn, err := net.DialTimeout("tcp", bAddr, 3*time.Second)
				if err != nil {
					elapsed[i] = -1
					return
				}
				defer conn.Close()
				_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
				conn.SetReadDeadline(time.Now().Add(12 * time.Second))
				start := time.Now()
				buf := make([]byte, 64)
				_, rerr := conn.Read(buf)
				if rerr == nil {
					elapsed[i] = -2 // got data unexpectedly
					return
				}
				elapsed[i] = time.Since(start)
			}(i)
		}
		wg.Wait()
		var worst time.Duration
		for i, e := range elapsed {
			if e == -1 {
				t.Errorf("conn %d failed to dial the runner port", i)
				continue
			}
			if e == -2 {
				t.Errorf("conn %d got data from a dead target", i)
				continue
			}
			if e > worst {
				worst = e
			}
		}
		if worst > 3*time.Second {
			logmu.Lock()
			ls := append([]string{}, logs...)
			logmu.Unlock()
			t.Errorf("slowest of %d conns hung %v before closing (want prompt close). logs=%v", n, worst, ls)
		} else {
			t.Logf("all %d dead-target conns closed; slowest %v", n, worst)
		}
	})

	// --- Stopping the forward (ctx cancel) must release the RUNNER listener ---
	// (not just stop the client): cancel → control stream closes → server sends
	// ClosePortForward → runner closes its listener. Regression for the leak
	// where the client blocked on a ctx-less read and never closed the control
	// stream.
	t.Run("cancel_stops_runner_listener", func(t *testing.T) {
		fwdCancel()
		select {
		case <-fwdDone:
		case <-time.After(3 * time.Second):
			t.Fatal("RunRemoteForward did not return within 3s of cancel (goroutine leak)")
		}
		// The runner listener should now be closed: a dial must fail.
		deadline := time.Now().Add(3 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			tc, err := net.DialTimeout("tcp", runnerAddr, 100*time.Millisecond)
			if err != nil {
				lastErr = err
				break
			}
			tc.Close()
			time.Sleep(50 * time.Millisecond)
		}
		if lastErr == nil {
			t.Error("runner listener still accepting after forward cancel (listener leaked on runner)")
		}
	})

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}

// TestLocalForwardRegisterListKill exercises the payoff of Task 4: a -L
// forward, whose listener lives entirely inside the client process, now
// registers itself with the server so a SECOND, independent client can list
// and kill it — and killing it makes the owning RunForward call return.
func TestLocalForwardRegisterListKill(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18549"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "lfwd-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Wait until the runner has the task registered (worktree appears).
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}
	t.Logf("worktree ready at %s", worktree)

	// The forward client: holds the listener, like a `harness-cli forward` terminal.
	fwdClient, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer fwdClient.Close()

	fwdCtx, cancelFwd := context.WithCancel(ctx)
	defer cancelFwd()
	done := make(chan error, 1)
	go func() {
		// LocalPort 0 asks the kernel for a free port, which is exactly the case
		// that proves RunForward registers the port it actually bound.
		done <- cli.RunForward(fwdCtx, fwdClient, taskID,
			[]cli.ForwardSpec{{BindAddr: "127.0.0.1", LocalPort: 0, RemoteHost: "127.0.0.1", RemotePort: 9}},
			func(s string) { t.Logf("forward: %s", s) }, nil)
	}()

	// A SECOND, independent client must be able to see and kill it.
	observer, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer observer.Close()

	var fs []protocol.PortForwardInfo
	var lastErr error
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fs, lastErr = observer.PortForwardListWith(ctx, "")
		if len(fs) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 1 {
		t.Fatalf("observer saw %d forwards, want 1 (last list error: %v)", len(fs), lastErr)
	}
	if fs[0].Direction != protocol.PortForwardDirection_Local {
		t.Fatalf("direction = %v, want Local", fs[0].Direction)
	}
	if fs[0].BindPort == 0 {
		t.Fatal("BindPort is 0 — the kernel-assigned port was not registered")
	}

	if err := observer.KillPortForwardWith(ctx, fs[0].ForwardId); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunForward returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunForward did not return after the forward was killed")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fs, lastErr = observer.PortForwardListWith(ctx, ""); len(fs) == 0 {
			cancel()
			select {
			case <-serverDone:
			case <-time.After(2 * time.Second):
				t.Log("server did not exit within 2s of cancel")
			}
			select {
			case <-runnerDone:
			case <-time.After(2 * time.Second):
				t.Log("runner did not exit within 2s of cancel")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("registration survived the kill: %+v (last list error: %v)", fs, lastErr)
}

// TestLocalForwardKillDropsConnection covers the acceptLoop per-connection
// watcher added alongside registration (cli/port_forward.go's acceptLoop,
// stop/ctx.Done goroutine): killing a -L forward must drop connections
// already spliced through it, not just stop accepting new ones. Establishes a
// real byte round-trip through the forward first (so the splice is
// confirmed live), then kills it from a second client and asserts the
// existing connection's Read unblocks with an error/EOF promptly.
func TestLocalForwardKillDropsConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18554"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "lfwd-drop-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	// Echo server: the remote target the forward relays to.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn) //nolint:errcheck
			}()
		}
	}()

	fwdClient, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer fwdClient.Close()

	fwdCtx, cancelFwd := context.WithCancel(ctx)
	defer cancelFwd()
	done := make(chan error, 1)
	go func() {
		done <- cli.RunForward(fwdCtx, fwdClient, taskID,
			[]cli.ForwardSpec{{BindAddr: "127.0.0.1", LocalPort: 0, RemoteHost: "127.0.0.1", RemotePort: echoPort}},
			func(s string) { t.Logf("forward: %s", s) }, nil)
	}()

	observer, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer observer.Close()

	var fs []protocol.PortForwardInfo
	var lastErr error
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fs, lastErr = observer.PortForwardListWith(ctx, "")
		if len(fs) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 1 {
		t.Fatalf("observer saw %d forwards, want 1 (last list error: %v)", len(fs), lastErr)
	}
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(fs[0].BindPort)))

	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	// Confirm the splice is actually live before killing it.
	msg := []byte("ping\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("readfull (pre-kill roundtrip): %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch pre-kill: got %q want %q", buf, msg)
	}

	if err := observer.KillPortForwardWith(ctx, fs[0].ForwardId); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// The already-established connection must be dropped, not just the
	// listener stopped from accepting new ones. A read timeout is NOT
	// evidence of that — it means the per-connection ctx.Done watcher
	// regressed and Read simply blocked until the deadline, which is exactly
	// the bug this test exists to catch. Fail explicitly on it instead of
	// letting os.ErrDeadlineExceeded satisfy the "rerr != nil" check below.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, rerr := conn.Read(buf)
	if errors.Is(rerr, os.ErrDeadlineExceeded) {
		t.Fatal("connection was not dropped — read timed out instead")
	}
	if rerr == nil {
		t.Fatalf("expected the connection to close after kill, got %d more bytes with no error", n)
	}

	select {
	case rfErr := <-done:
		if rfErr != nil {
			t.Fatalf("RunForward returned %v, want nil", rfErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunForward did not return after the forward was killed")
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}

// TestLocalForwardMultiSpecIndependentKill covers the multi-spec path that
// Finding 1 (RunForward abandoning already-started specs on a mid-loop
// failure) lives on: two -L specs in one RunForward call register two
// independent forwards. Killing one must NOT affect the other (still relays
// bytes) and must NOT make RunForward return early (it returns only once
// every spec has stopped).
func TestLocalForwardMultiSpecIndependentKill(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18555"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "lfwd-multi-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn) //nolint:errcheck
			}()
		}
	}()

	// Two fixed, independently-reserved local ports so list entries can be
	// matched back to the spec that produced them.
	reserve := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		return p
	}
	portA := reserve()
	portB := reserve()

	fwdClient, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer fwdClient.Close()

	fwdCtx, cancelFwd := context.WithCancel(ctx)
	defer cancelFwd()
	done := make(chan error, 1)
	go func() {
		done <- cli.RunForward(fwdCtx, fwdClient, taskID, []cli.ForwardSpec{
			{BindAddr: "127.0.0.1", LocalPort: portA, RemoteHost: "127.0.0.1", RemotePort: echoPort},
			{BindAddr: "127.0.0.1", LocalPort: portB, RemoteHost: "127.0.0.1", RemotePort: echoPort},
		}, func(s string) { t.Logf("forward: %s", s) }, nil)
	}()

	observer, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer observer.Close()

	var fs []protocol.PortForwardInfo
	var lastErr error
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fs, lastErr = observer.PortForwardListWith(ctx, "")
		if len(fs) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 2 {
		t.Fatalf("observer saw %d forwards, want 2 (last list error: %v)", len(fs), lastErr)
	}

	var idA, idB uint64
	for _, fi := range fs {
		switch int(fi.BindPort) {
		case portA:
			idA = fi.ForwardId
		case portB:
			idB = fi.ForwardId
		default:
			t.Fatalf("unexpected bind port %d in %+v", fi.BindPort, fs)
		}
	}
	if idA == 0 || idB == 0 {
		t.Fatalf("could not match both specs to registrations: %+v", fs)
	}

	// Kill A only.
	if err := observer.KillPortForwardWith(ctx, idA); err != nil {
		t.Fatalf("kill A: %v", err)
	}

	// RunForward must NOT have returned yet: B is still running.
	select {
	case rfErr := <-done:
		t.Fatalf("RunForward returned early (err=%v) after killing only one of two specs", rfErr)
	case <-time.After(1 * time.Second):
	}

	// The list must now show only B.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fs, lastErr = observer.PortForwardListWith(ctx, "")
		if len(fs) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 1 || fs[0].ForwardId != idB {
		t.Fatalf("after killing A, want only B (id %d) listed, got %+v (last list error: %v)", idB, fs, lastErr)
	}

	// B must still actually forward bytes.
	addrB := net.JoinHostPort("127.0.0.1", strconv.Itoa(portB))
	conn, err := net.DialTimeout("tcp", addrB, 2*time.Second)
	if err != nil {
		t.Fatalf("dial B after killing A: %v", err)
	}
	defer conn.Close()
	msg := []byte("still-here\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write B: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("readfull B: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch on B: got %q want %q", buf, msg)
	}
	conn.Close()

	// Now kill B too and confirm RunForward finally returns.
	if err := observer.KillPortForwardWith(ctx, idB); err != nil {
		t.Fatalf("kill B: %v", err)
	}
	select {
	case rfErr := <-done:
		if rfErr != nil {
			t.Fatalf("RunForward returned %v, want nil", rfErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunForward did not return after both specs were killed")
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}

// TestLocalForwardPartialFailureDeregistersStartedSpecs is the regression
// test for Finding 1: a mid-loop failure in RunForward (net.Listen or
// RegisterPortForward, both funnel through the same abort() path in
// cli/port_forward.go) must deregister every spec that had already succeeded,
// promptly — not leave it listed until the client's peer connection eventually
// times out. The second spec is forced to fail net.Listen deterministically
// (both specs request the same fixed local port, so the second's bind
// collides with the first's already-open listener); Local registration has no
// server-side failure mode reachable without racing task lifecycle, but both
// error paths in RunForward call the identical abort(), so exercising either
// proves the fix. fwdClient is deliberately kept open (not closed) for the
// whole assertion window, so a pass cannot be attributed to the old
// "cleaned up only when the client disconnects" behavior.
func TestLocalForwardPartialFailureDeregistersStartedSpecs(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18556"
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "lfwd-partial-fail-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}

	dupLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dup port: %v", err)
	}
	dupPort := dupLn.Addr().(*net.TCPAddr).Port
	dupLn.Close()

	fwdClient, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer fwdClient.Close()

	specs := []cli.ForwardSpec{
		// Spec 0 binds dupPort and registers successfully.
		{BindAddr: "127.0.0.1", LocalPort: dupPort, RemoteHost: "127.0.0.1", RemotePort: 9},
		// Spec 1 requests the SAME port: its net.Listen deterministically fails
		// with "address already in use" once spec 0 already holds it, since
		// RunForward's spec loop runs sequentially.
		{BindAddr: "127.0.0.1", LocalPort: dupPort, RemoteHost: "127.0.0.1", RemotePort: 9},
	}

	done := make(chan error, 1)
	go func() {
		done <- cli.RunForward(ctx, fwdClient, taskID, specs, func(s string) { t.Logf("forward: %s", s) }, nil)
	}()

	var rfErr error
	select {
	case rfErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunForward did not return after a mid-loop failure (leak reproduced: it hung instead of aborting)")
	}
	if rfErr == nil {
		t.Fatal("RunForward returned nil, want an error from the colliding second spec")
	}
	t.Logf("RunForward returned expected error: %v", rfErr)

	observer, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial observer: %v", err)
	}
	defer observer.Close()

	// This is the regression assertion: spec 0's registration must disappear
	// promptly. Bound to 3s, well under the ~15s peer-connection timeout that
	// used to be the only thing that ever cleaned this up — and fwdClient is
	// still open throughout, so that old fallback path cannot be what passes
	// this assertion.
	var fs []protocol.PortForwardInfo
	var lastErr error
	promptDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(promptDeadline) {
		fs, lastErr = observer.PortForwardListWith(ctx, "")
		if len(fs) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fs) != 0 {
		t.Fatalf("spec 0's registration survived the partial failure (still connected, %.0fs later): %+v (last list error: %v)",
			3.0, fs, lastErr)
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}

// TestRawForwardRoundTripListKill covers the whole in-process endpoint path with
// no socket on the client side: bytes reach an echo listener the runner dials,
// the registration is listable as (in-process), and a kill from a second client
// tears the connection down.
func TestRawForwardRoundTripListKill(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	// --- echo listener the runner will dial ---
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	// --- harness + running task: copied from TestLocalForwardRegisterListKill,
	// ending with taskID and a connected *cli.Client named c. ---
	repo := initRepo(t)
	fakeClaude, err := filepath.Abs("../testdata/fake-claude-slow.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18557"
	serverCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir()})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    serverCID,
			AllowedRoots: []string{repo},
			Profiles:     singleAgentProfile(fakeClaude),
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, serverCID, repo, "rawfwd-test")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Wait until the runner has the task registered (worktree appears).
	worktree := filepath.Join(repo, ".harness-worktrees", taskID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree did not appear: %v", err)
	}
	t.Logf("worktree ready at %s", worktree)

	c, err := cli.Dial(ctx, serverCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial forward client: %v", err)
	}
	defer c.Close()
	// --- end of copied setup block ---

	// The open context is cancelled the instant the open returns — exactly what
	// the TUI's DoStartRawForward does with its connect timeout. The forward
	// must survive it: ctx bounds the OPEN, not the connection. It did not:
	// the control watcher was started on this ctx and its deferred onEnd
	// closed the data stream, so every TUI raw connect died the moment it was
	// established ("connection closed", no reason given).
	openCtx, openCancel := context.WithCancel(context.Background())
	rc, err := cli.OpenRawForward(openCtx, c, taskID, "127.0.0.1", port, func(string) {})
	openCancel()
	if err != nil {
		t.Fatalf("OpenRawForward: %v", err)
	}
	if err := rc.Send([]byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := make([]byte, 0, 4)
	deadlineRT := time.Now().Add(10 * time.Second)
	for len(got) < 4 && time.Now().Before(deadlineRT) {
		data, eof, rerr := rc.Recv(context.Background())
		got = append(got, data...)
		if eof || rerr != nil {
			break
		}
	}
	if string(got) != "ping" {
		t.Fatalf("echo round-trip = %q, want \"ping\"", got)
	}

	forwards, err := cli.PortForwardList(context.Background(), serverCID, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *protocol.PortForwardInfo
	for i := range forwards {
		if forwards[i].ForwardId == rc.ForwardID() {
			found = &forwards[i]
		}
	}
	if found == nil {
		t.Fatalf("raw forward %d absent from the listing (%d entries)", rc.ForwardID(), len(forwards))
	}
	if found.ClientEndpoint != protocol.ClientEndpointKind_InProcess {
		t.Fatalf("listed endpoint = %v, want InProcess", found.ClientEndpoint)
	}
	if spec := cli.PortForwardSpecString(found); !strings.HasPrefix(spec, "(in-process) -> ") {
		t.Fatalf("spec = %q, want an (in-process) prefix", spec)
	}

	if err := cli.KillPortForward(context.Background(), serverCID, rc.ForwardID()); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// The kill must reach the data stream, not just the registry — a
	// registration whose lifetime drifts from its connection's lifetime is
	// exactly the bug class this feature can introduce.
	killDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(killDeadline) {
		if _, eof, rerr := rc.Recv(context.Background()); eof || rerr != nil {
			cancel()
			select {
			case <-serverDone:
			case <-time.After(2 * time.Second):
				t.Log("server did not exit within 2s of cancel")
			}
			select {
			case <-runnerDone:
			case <-time.After(2 * time.Second):
				t.Log("runner did not exit within 2s of cancel")
			}
			return
		}
	}
	t.Fatal("data stream still live 10s after the forward was killed")
}
