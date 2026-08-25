//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
	"golang.org/x/crypto/ssh"
)

// TestSSHGatewayE2E drives the gateway with golang.org/x/crypto/ssh AS A CLIENT,
// so the suite depends on no `ssh` binary being installed, against a real
// server + runner + detachable session.
//
// The sub-tests run in order and share one gateway and one task; each closes
// its ssh connection so the control seat it may have taken is released.
func TestSSHGatewayE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude scripts require bash — skipping on Windows")
	}
	clearAgentEnv(t)

	serverCID := startServer(t)
	repo := tempRepo(t)
	startRunner(t, serverCID, runnerOpts{
		MaxTasks:  2,
		Roots:     []string{repo},
		ClaudeBin: fakeClaudeSlowPath(t),
	})

	c := dialClient(t, serverCID)
	taskID := openThenDetachSession(t, c, repo)
	gwAddr := startGateway(t, c)

	t.Run("attach_replays_the_ring", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, stdout, _ := startShell(t, cl, 30, 100)
		defer sess.Close()

		// fake-claude-slow echoes a starting line, so the replay is non-empty.
		got := readSome(t, stdout, 3*time.Second)
		if len(got) == 0 {
			t.Error("no replay bytes arrived on the ssh channel")
		}
	})

	t.Run("resize_applies_while_the_seat_is_empty", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, stdout, _ := startShell(t, cl, 30, 100)
		defer sess.Close()
		readSome(t, stdout, 2*time.Second)

		// A bare user name is a cowrite attach, and an observer's resize is
		// honoured while no control client holds the seat — which is the state
		// of an ordinary detached session.
		rows, cols := sessionSize(t, c, taskID)
		if rows != 30 || cols != 100 {
			t.Errorf("session size = %dx%d (rows x cols), want 30x100", rows, cols)
		}
	})

	t.Run("resize_is_dropped_while_a_controller_holds_the_seat", func(t *testing.T) {
		// Take the seat with an ordinary harness control attach, the way the
		// TUI does.
		ctrl, _, _, err := c.AttachSession(context.Background(), taskID, protocol.AttachMode_Control)
		if err != nil {
			t.Fatalf("control attach: %v", err)
		}
		defer ctrl.Close()
		go func() { _, _ = io.Copy(io.Discard, ctrl.Stdout()) }()

		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, stdout, _ := startShell(t, cl, 20, 50)
		defer sess.Close()
		readSome(t, stdout, 2*time.Second)

		// Asserting that the frame was WRITTEN would pass either way. The size
		// the session reports is the only thing that distinguishes the rule
		// from its absence.
		rows, cols := sessionSize(t, c, taskID)
		if rows == 20 && cols == 50 {
			t.Error("the ssh cowriter's resize took effect while a controller held the seat")
		}
		if rows != 30 || cols != 100 {
			t.Errorf("session size = %dx%d, want the controller-era 30x100 to be unchanged", rows, cols)
		}
	})

	t.Run("second_cowrite_is_accepted", func(t *testing.T) {
		cl1 := dialSSH(t, gwAddr, taskID)
		defer cl1.Close()
		sess1, stdout1, _ := startShell(t, cl1, 30, 100)
		defer sess1.Close()
		readSome(t, stdout1, 2*time.Second)

		cl2 := dialSSH(t, gwAddr, taskID)
		defer cl2.Close()
		sess2, err := cl2.NewSession()
		if err != nil {
			t.Fatalf("a second cowrite session on the same task must be accepted: %v", err)
		}
		sess2.Close()
	})

	t.Run("second_control_is_refused", func(t *testing.T) {
		cl1 := dialSSH(t, gwAddr, taskID+".control")
		defer cl1.Close()
		sess1, stdout1, _ := startShell(t, cl1, 30, 100)
		defer sess1.Close()
		readSome(t, stdout1, 2*time.Second)

		cl2 := dialSSH(t, gwAddr, taskID+".control")
		defer cl2.Close()
		if _, err := cl2.NewSession(); err == nil {
			t.Fatal("a second .control session must be refused, not allowed to take the seat")
		} else if !strings.Contains(err.Error(), taskID) {
			t.Errorf("rejection %q does not name the task", err)
		}
	})

	t.Run("detach_key_ends_the_ssh_session_and_leaves_the_task", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, stdout, stdin := startShell(t, cl, 30, 100)
		readSome(t, stdout, 2*time.Second)

		if _, err := stdin.Write([]byte{0x1d}); err != nil {
			t.Fatalf("write detach key: %v", err)
		}

		rest := readUntilEOF(t, stdout, 10*time.Second)
		if err := sess.Wait(); err != nil {
			t.Errorf("ssh session ended with %v, want a clean exit after a detach", err)
		}

		// The reset is the last thing written before the channel closes; a
		// client whose terminal is left on the alternate screen has no other
		// way back.
		wantSuffix := agentexec.ScreenModeReset + agentexec.InputModeReset
		if !bytes.HasSuffix(rest, []byte(wantSuffix)) {
			t.Errorf("channel did not end with the terminal reset (last 64 bytes: %q)", tail(rest, 64))
		}

		// Detaching is not ending: the task must still be there.
		eventually(t, func() bool {
			st := getTask(t, c, taskID).Status
			return st == protocol.TaskStatus_Detached || st == protocol.TaskStatus_Running
		}, 5*time.Second, 100*time.Millisecond, "task to survive the ssh detach")
	})

	t.Run("unknown_user_name_is_refused_at_channel_open", func(t *testing.T) {
		// Not at authentication: failing there makes ssh retry keys and then
		// report a credentials problem, pointing the operator at the wrong
		// thing entirely.
		cl := dialSSH(t, gwAddr, "root")
		defer cl.Close()
		_, err := cl.NewSession()
		if err == nil {
			t.Fatal("want a rejection for a user name that is not a task id")
		}
		if !strings.Contains(err.Error(), ".control") || !strings.Contains(err.Error(), ".view") {
			t.Errorf("rejection %q does not name the accepted forms", err)
		}
	})

	// `ssh host cmd` runs the command in the task's worktree. This used to be an
	// accepted-then-refused request, because the only command surface was
	// `session exec` — which types into the session's foreground shell and
	// merges the two output streams, nothing like what ssh promises. It now maps
	// to `exec`, which has exactly ssh's semantics.
	t.Run("exec_runs_the_command", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, err := cl.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		var stdout, stderr bytes.Buffer
		sess.Stdout = &stdout
		sess.Stderr = &stderr

		err = sess.Run("echo hi 1>&2; exit 3")

		// The command's own exit code becomes ssh's, which is what makes this
		// usable from a script on the other end.
		var ee *ssh.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("Run = %v, want an *ssh.ExitError carrying the command's status", err)
		}
		if ee.ExitStatus() != 3 {
			t.Errorf("exit status = %d, want 3", ee.ExitStatus())
		}
		// Separate, all the way out to the ssh client. A test that read one
		// merged buffer would pass against `session exec` too, which is the
		// thing this replaced.
		if got := strings.TrimSpace(stderr.String()); !strings.Contains(got, "hi") {
			t.Errorf("stderr = %q, want the redirected line", got)
		}
		if strings.Contains(stdout.String(), "hi") {
			t.Errorf("stdout = %q, must NOT carry what the command sent to stderr", stdout.String())
		}
	})

	// The shell form is the point: `ssh host cmd` sends ONE string it expects a
	// shell to interpret, so quoting and redirection have to survive. Splitting
	// it on whitespace here would break every one of them.
	t.Run("exec_keeps_the_shell_quoting", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		sess, err := cl.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		out, err := sess.Output(`echo "one two"`)
		if err != nil {
			t.Fatalf("Output: %v", err)
		}
		if strings.TrimSpace(string(out)) != "one two" {
			t.Errorf("output = %q, want the quoted argument intact", out)
		}
	})

	// An interrupted `ssh host cmd` stops the command. Ctrl-C at the far
	// terminal kills the ssh client, and the only signal this end gets is the
	// channel closing — the gateway's OWN harness connection stays up, so the
	// server's disconnect sweep never fires for it and nothing else would
	// notice. Measured before the fix: the ssh client gone, the exec still in
	// `exec ls`, the child still running on the runner.
	t.Run("closing_the_ssh_connection_stops_the_exec", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, taskID)
		sess, err := cl.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		if err := sess.Start("sleep 30"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		eventually(t, func() bool {
			execs, lerr := c.ExecRunListWith(context.Background(), "")
			return lerr == nil && len(execs) == 1
		}, 10*time.Second, 100*time.Millisecond, "the ssh exec to register")

		// The whole client, not just the session: that is what an interrupted
		// `ssh` leaves behind.
		_ = cl.Close()

		eventually(t, func() bool {
			execs, lerr := c.ExecRunListWith(context.Background(), "")
			return lerr == nil && len(execs) == 0
		}, 15*time.Second, 200*time.Millisecond, "the abandoned exec to be stopped")
	})

	// An exec must not take the control seat. It never attaches, so holding one
	// for the length of the command would lock a real attach out for no reason —
	// and `.control` is the form a script reaching for a command would type.
	t.Run("exec_does_not_hold_the_control_seat", func(t *testing.T) {
		// The seat an EARLIER sub-test took is given back when its ssh
		// connection closes, which the gateway observes asynchronously. Waiting
		// for that here keeps this case measuring its own claim rather than the
		// previous one's cleanup.
		cl := dialSSH(t, gwAddr, taskID+".control")
		defer cl.Close()
		sess := controlSessionWhenFree(t, cl)
		if _, err := sess.Output("true"); err != nil {
			t.Fatalf("Output: %v", err)
		}
		sess.Close()

		// A second .control session must be accepted IMMEDIATELY — no waiting.
		// The exec released the seat before it ran, so there is nothing to
		// wait for, and being patient here would hide the defect.
		cl2 := dialSSH(t, gwAddr, taskID+".control")
		defer cl2.Close()
		sess2, err := cl2.NewSession()
		if err != nil {
			t.Fatalf("the exec kept the control seat: %v", err)
		}
		defer sess2.Close()
		if _, err := sess2.Output("true"); err != nil {
			t.Errorf("second exec after a .control exec: %v", err)
		}
	})

	// `ssh -L` opens a direct-tcpip channel per accepted connection and `ssh -W`
	// opens one; the RUNNER dials the target either way. The channel type was
	// refused for one release on the grounds that `harness-cli forward` already
	// does this and a second path would drift — but it is not a second path, it
	// is the same OpenRawForward the `-W` verb calls, reached through a door an
	// ssh client can find. What the refusal cost was every client that does its
	// own forwarding.
	t.Run("direct_tcpip_reaches_a_listener", func(t *testing.T) {
		target := startEchoListener(t)
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()

		conn, err := cl.Dial("tcp", target)
		if err != nil {
			t.Fatalf("direct-tcpip dial %s: %v", target, err)
		}
		defer conn.Close()

		want := []byte("through the runner\n")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := readFullWithin(t, conn, len(want), 10*time.Second); !bytes.Equal(got, want) {
			t.Errorf("echoed %q, want %q", got, want)
		}
	})

	// The ssh client's own -L listener is invisible to the harness — it lives in
	// a process on the other side of the gateway — so this per-connection row is
	// the ONLY place a forward opened through the gateway can be seen or
	// stopped. A forward running with no entry in `forward ls` is exactly the
	// state OpenRawForward's registration exists to prevent.
	t.Run("direct_tcpip_registers_a_killable_forward", func(t *testing.T) {
		// The previous sub-test's connection closes asynchronously, so wait for
		// a quiet baseline rather than assuming one: counting from a stale row
		// would make this pass or fail on the neighbour's timing.
		eventually(t, func() bool {
			fs, lerr := c.PortForwardListWith(context.Background(), taskID)
			return lerr == nil && len(fs) == 0
		}, 10*time.Second, 100*time.Millisecond, "the previous forward to deregister")

		target := startEchoListener(t)
		cl := dialSSH(t, gwAddr, taskID)
		defer cl.Close()
		conn, err := cl.Dial("tcp", target)
		if err != nil {
			t.Fatalf("direct-tcpip dial: %v", err)
		}

		eventually(t, func() bool {
			fs, lerr := c.PortForwardListWith(context.Background(), taskID)
			return lerr == nil && len(fs) == 1
		}, 10*time.Second, 100*time.Millisecond, "the gateway's forward to be listed")

		_ = conn.Close()
		eventually(t, func() bool {
			fs, lerr := c.PortForwardListWith(context.Background(), taskID)
			return lerr == nil && len(fs) == 0
		}, 10*time.Second, 100*time.Millisecond, "the forward to deregister when the channel closes")
	})

	// The user name selects the task for a forward exactly as it does for a
	// session, and an unusable one is refused at channel open where the reason
	// rides the rejection and the client prints it.
	t.Run("direct_tcpip_on_an_unknown_user_is_refused", func(t *testing.T) {
		cl := dialSSH(t, gwAddr, "root")
		defer cl.Close()
		if _, err := cl.Dial("tcp", "127.0.0.1:9"); err == nil {
			t.Fatal("want a rejection for a user name that is not a task id")
		} else if !strings.Contains(err.Error(), ".control") || !strings.Contains(err.Error(), ".view") {
			t.Errorf("rejection %q does not name the accepted forms", err)
		}
	})
}

// startEchoListener runs a loopback TCP echo server, standing in for whatever
// service the operator wants to reach beside their agent. The runner in this
// suite is on this machine, so 127.0.0.1 from the runner's side is this
// listener.
func startEchoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// readFullWithin reads exactly n bytes or fails the test. The timeout is
// imposed from outside the read because an ssh channel's net.Conn does not
// support SetReadDeadline — x/crypto/ssh answers that call with an error rather
// than honouring it, so a deadline set on it would silently do nothing.
func readFullWithin(t *testing.T, r io.Reader, n int, d time.Duration) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, rerr := io.ReadFull(r, buf)
		ch <- result{buf, rerr}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read %d bytes: %v", n, res.err)
		}
		return res.b
	case <-time.After(d):
		t.Fatalf("did not receive %d bytes within %v", n, d)
		return nil
	}
}

// controlSessionWhenFree opens a session channel on a .control connection,
// retrying while the seat is still held by a previous sub-test's connection
// that has closed but whose release the gateway has not yet observed.
func controlSessionWhenFree(t *testing.T, cl *ssh.Client) *ssh.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess, err := cl.NewSession()
		if err == nil {
			return sess
		}
		if time.Now().After(deadline) {
			t.Fatalf("control seat never came free: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// openThenDetachSession opens a detachable session, waits for it to run, then
// closes the local stream so the task is Detached and the control seat is free
// — the ordinary state of a session an operator reaches later.
func openThenDetachSession(t *testing.T, c *cli.Client, repo string) string {
	t.Helper()
	sel := protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}
	stream, taskID, err := c.OpenInteractive(context.Background(), repo, cli.SessionOpts{
		Selector: sel, InitialRows: 24, InitialCols: 80,
	})
	if err != nil {
		t.Fatalf("OpenInteractive: %v", err)
	}
	drained := make(chan struct{})
	go func() { defer close(drained); _, _ = io.Copy(io.Discard, stream.Stdout()) }()

	eventually(t, func() bool {
		return getTask(t, c, taskID).Status == protocol.TaskStatus_Running
	}, 15*time.Second, 100*time.Millisecond, "task to reach Running")

	stream.Close()
	<-drained
	eventually(t, func() bool {
		return getTask(t, c, taskID).Status == protocol.TaskStatus_Detached
	}, 10*time.Second, 100*time.Millisecond, "task to reach Detached")
	return taskID
}

// startGateway runs a gateway on a free loopback port against the given client
// and waits for the listener to accept.
func startGateway(t *testing.T, c *cli.Client) string {
	t.Helper()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sshgw.Run(ctx, c, sshgw.Options{
			Listen:      addr,
			HostKeyPath: filepath.Join(t.TempDir(), "ssh_host_ed25519_key"),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("ssh gateway returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Log("ssh gateway did not exit within 3s of cancel")
		}
	})
	eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "ssh gateway listener to accept")
	return addr
}

// dialSSH connects to the gateway as user. The gateway binds loopback with no
// authorized-keys file, so the "none" method is what the handshake uses.
func dialSSH(t *testing.T, addr, user string) *ssh.Client {
	t.Helper()
	cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial as %q: %v", user, err)
	}
	return cl
}

// startShell opens a session channel, requests a PTY of the given size and
// starts a shell — the sequence an interactive `ssh` performs.
func startShell(t *testing.T, cl *ssh.Client, rows, cols int) (*ssh.Session, io.Reader, io.WriteCloser) {
	t.Helper()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	return sess, stdout, stdin
}

// sessionSize reports the PTY size the server replays to a fresh observer,
// which is what the session actually renders at.
func sessionSize(t *testing.T, c *cli.Client, taskID string) (uint16, uint16) {
	t.Helper()
	raw, err := c.CollectRaw(context.Background(), taskID, 1200*time.Millisecond, true)
	if err != nil {
		t.Fatalf("CollectRaw: %v", err)
	}
	if !raw.HasSize {
		t.Fatal("the session reports no size at all")
	}
	return raw.Rows, raw.Cols
}

// readSome reads whatever arrives within d, returning early once anything has.
func readSome(t *testing.T, r io.Reader, d time.Duration) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, err := r.Read(buf)
		ch <- result{buf[:n], err}
	}()
	select {
	case res := <-ch:
		return res.b
	case <-time.After(d):
		return nil
	}
}

// readUntilEOF drains r until it ends or d elapses.
func readUntilEOF(t *testing.T, r io.Reader, d time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- b
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(d):
		t.Fatalf("channel did not end within %v of the detach key", d)
		return nil
	}
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
