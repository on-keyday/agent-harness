//go:build integration

package integration

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/sshgw"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"golang.org/x/crypto/ssh"
)

// runGateway starts one gateway on a free port and returns its address and the
// channel Serve's return value lands on. Unlike startGateway it does not
// register a cleanup that requires the serve loop to still be running: these
// tests are about the loop ENDING on its own.
func runGateway(t *testing.T, c *cli.Client) (string, context.CancelFunc, <-chan error) {
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
	t.Cleanup(cancel)
	eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "ssh gateway listener to accept")
	return addr, cancel, done
}

// A gateway is a local TCP listener plus a harness connection, and only the
// listener has an obvious owner. That is how it outlived the connection: the
// serve loop blocks in Accept() on a socket that knows nothing about objproto,
// so a dead harness connection left the port bound, `ssh-gateway status` still
// claiming to listen, and every attach through it failing against a client that
// can no longer reach the server — until the operator stopped it by hand.
//
// Every sibling long-lived client verb already dies with the connection,
// because each blocks on a STREAM that the connection carries (see the reconnect
// comment in tui/app.go: "they die with the connection that held their control
// streams"). The gateway holds no such stream, so the link has to be explicit.
func TestSSHGatewayStopsWhenTheHarnessConnectionCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)
	serverCID := startServer(t)

	// Not dialClient: this test closes the connection itself, mid-run.
	c, err := cli.Dial(context.Background(), serverCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	addr, _, done := runGateway(t, c)

	c.Close() // the objproto.Connection goes away

	select {
	case err := <-done:
		t.Logf("gateway returned %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway was still serving 10s after its harness connection closed")
	}
	// The port has to be actually released, not merely reported as stopped:
	// the operator's next `ssh-gateway start` binds the same address.
	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		t.Fatalf("the listener still holds %s after the harness connection closed: %v", addr, lerr)
	}
	ln.Close()
}

// Stopping a gateway must also drop the ssh connections it accepted. They are
// held by serveConn goroutines that block reading the ssh transport, which no
// ctx reaches, so before this they survived the stop — a client left sitting on
// a live ssh connection with a dead harness behind it.
func TestSSHGatewayStopDropsAcceptedSSHConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)
	serverCID := startServer(t)
	c := dialClient(t, serverCID)
	addr, cancel, done := runGateway(t, c)

	cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "00000000000000000000000000000000",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer cl.Close()

	waited := make(chan error, 1)
	go func() { waited <- cl.Wait() }()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10s of the stop")
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("an ssh connection accepted by the gateway outlived the stop")
	}
}
