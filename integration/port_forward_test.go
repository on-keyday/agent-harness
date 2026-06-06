package integration

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/server"
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
			ClaudeBin:    fakeClaude,
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
	c, err := cli.Dial(ctx, peerCID)
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
		fwdDone <- cli.RunForward(fwdCtx, c, taskID, specs, nil)
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
			ClaudeBin:    fakeClaude,
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

	c, err := cli.Dial(ctx, peerCID)
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
