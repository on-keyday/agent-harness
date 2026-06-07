//go:build integration

package integration

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// freeUDPPort asks the OS for an available UDP port on localhost. Mirror
// of freePort (TCP) used by other integration tests.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("freeUDPPort: %v", err)
	}
	addr := conn.LocalAddr().String()
	conn.Close()
	return addr
}

// startUDPServer brings up an in-process server listening on UDP only
// (no WS leg). Returns the peer CID a runner / client can dial against,
// plus a cancel func.
func startUDPServer(t *testing.T) (objproto.ConnectionID, context.CancelFunc) {
	t.Helper()
	addr := freeUDPPort(t)
	peerCID, err := objproto.ParseConnectionID("udp:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse udp cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{
		UDPAddr: addr,
		DataDir: t.TempDir(),
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Log("udp server did not exit within 3s of cancel")
		}
	})

	time.Sleep(300 * time.Millisecond)
	return peerCID, cancel
}

// startDualStackServer brings up an in-process server listening on both
// WS (TCP) and UDP. Returns both peer CIDs (caller picks the one the
// dialer should use) and a cancel func.
func startDualStackServer(t *testing.T) (wsCID, udpCID objproto.ConnectionID, cancelFn context.CancelFunc) {
	t.Helper()
	wsAddr := freePort(t)
	udpAddr := freeUDPPort(t)

	parse := func(prefix, addr string) objproto.ConnectionID {
		c, err := objproto.ParseConnectionID(prefix+":"+addr+"-*",
			objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
		if err != nil {
			t.Fatalf("parse %s cid: %v", prefix, err)
		}
		return c
	}
	wsCID = parse("ws", wsAddr)
	udpCID = parse("udp", udpAddr)

	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{
		Addr:    wsAddr,
		UDPAddr: udpAddr,
		DataDir: t.TempDir(),
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Log("dualstack server did not exit within 3s of cancel")
		}
	})

	time.Sleep(300 * time.Millisecond)
	return wsCID, udpCID, cancel
}

// startRunnerWithCID starts a runner using the supplied serverCID; its
// transport is whatever the CID encodes ("ws", "wss", "udp"). This is a
// thin variant of startRunner that doesn't assume WS.
func startRunnerWithCID(t *testing.T, serverCID objproto.ConnectionID, repo, hostname string) *runnerHandle {
	t.Helper()
	claudeBin, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatalf("resolve fake-claude.sh: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, runner.Config{
			ServerCID:    serverCID,
			AllowedRoots: []string{repo},
			MaxTasks:     1,
			Hostname:     hostname,
			ClaudeBin:    claudeBin,
		})
	}()

	h := &runnerHandle{
		cancel:   cancel,
		done:     done,
		hostname: hostname,
		roots:    []string{repo},
	}
	t.Cleanup(h.Close)
	time.Sleep(500 * time.Millisecond)
	return h
}

func waitForRegisteredRunner(t *testing.T, c *cli.Client, repo string, deadline time.Duration) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		snap, err := c.Snapshot(context.Background())
		if err == nil {
			for _, r := range snap.Runners {
				for _, ar := range r.AllowedRoots {
					if string(ar.Path) == repo {
						return true
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// TestUDPRunner_RegistersAndCompletesTask brings up a UDP-only server,
// dials a UDP runner against it, then submits a task via a UDP-dialed
// CLI client. End-to-end smoke that exercises the entire transport
// substitution chain (server.Run UDP path, runner.Connect udp branch,
// cli.Dial udp branch).
func TestUDPRunner_RegistersAndCompletesTask(t *testing.T) {
	t.Parallel()
	serverCID, _ := startUDPServer(t)

	repo := tempRepo(t)
	_ = startRunnerWithCID(t, serverCID, repo, "udp-runner-host")

	c, err := cli.Dial(context.Background(), serverCID)
	if err != nil {
		t.Fatalf("cli.Dial(udp): %v", err)
	}
	defer c.Close()

	if !waitForRegisteredRunner(t, c, repo, 5*time.Second) {
		t.Fatalf("runner did not register over UDP within 5s")
	}

	taskID := mustSubmit(t, c, repo, "echo hello-udp")
	waitTaskTerminal(t, c, taskID, 10*time.Second)

	ti := getTask(t, c, taskID)
	if ti.Status != protocol.TaskStatus_Succeeded {
		t.Errorf("task %s status = %v, want Succeeded", taskID, ti.Status)
	}
}

// TestDualStackServer_AcceptsBothLegs brings up a server listening on
// both WS and UDP, attaches a WS runner and a UDP runner each pinned to
// its own repo, and verifies both legs deliver tasks.
func TestDualStackServer_AcceptsBothLegs(t *testing.T) {
	t.Parallel()
	wsCID, udpCID, _ := startDualStackServer(t)

	repoWS := tempRepo(t)
	repoUDP := tempRepo(t)

	_ = startRunnerWithCID(t, wsCID, repoWS, "ws-leg-host")
	_ = startRunnerWithCID(t, udpCID, repoUDP, "udp-leg-host")

	cWS, err := cli.Dial(context.Background(), wsCID)
	if err != nil {
		t.Fatalf("cli.Dial(ws): %v", err)
	}
	defer cWS.Close()

	if !waitForRegisteredRunner(t, cWS, repoWS, 5*time.Second) {
		t.Fatalf("WS runner did not register within 5s")
	}
	if !waitForRegisteredRunner(t, cWS, repoUDP, 5*time.Second) {
		t.Fatalf("UDP runner did not register within 5s")
	}

	idWS := mustSubmit(t, cWS, repoWS, "echo ws-side")
	idUDP := mustSubmit(t, cWS, repoUDP, "echo udp-side")

	waitTaskTerminal(t, cWS, idWS, 10*time.Second)
	waitTaskTerminal(t, cWS, idUDP, 10*time.Second)

	if ti := getTask(t, cWS, idWS); ti.Status != protocol.TaskStatus_Succeeded {
		t.Errorf("ws-leg task %s status = %v, want Succeeded", idWS, ti.Status)
	}
	if ti := getTask(t, cWS, idUDP); ti.Status != protocol.TaskStatus_Succeeded {
		t.Errorf("udp-leg task %s status = %v, want Succeeded", idUDP, ti.Status)
	}
}
