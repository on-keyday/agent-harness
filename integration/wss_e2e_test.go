//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// loopbackTLSConfig is a throwaway self-signed certificate for 127.0.0.1.
// Clients skip verification (cli.ClientTLSConfig), so only the handshake
// matters; nothing is written to disk.
func loopbackTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "harness-wss-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func startWSSServer(t *testing.T) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	cid, err := objproto.ParseConnectionID("wss:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir(), TLS: loopbackTLSConfig(t)})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return cid
}

// The whole wss path in one pass: the CLI and a runner reach a TLS server
// over wss:, a task runs, and the agent is handed a wss: HARNESS_SERVER_CID
// (read off the live connection, so it is wss: only if the per-connection
// label reached the runner).
func TestWSS_CLIRunnerAndAgentCID(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)
	cid := startWSSServer(t)

	agent := filepath.Join(t.TempDir(), "print-server-cid.sh")
	if err := os.WriteFile(agent, []byte("#!/bin/bash\necho \"server_cid=$HARNESS_SERVER_CID\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := tempRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(cid),
			AllowedRoots:     []string{repo},
			MaxTasks:         1,
			Hostname:         "wss-runner-host",
			Profiles:         singleAgentProfile(agent),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(3 * time.Second):
		}
	})

	c, err := cli.Dial(context.Background(), cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("cli.Dial(wss): %v", err)
	}
	defer c.Close()
	if !waitForRegisteredRunner(t, c, repo, 10*time.Second) {
		t.Fatal("runner did not register over wss within 10s")
	}

	id := mustSubmit(t, c, repo, "print the server cid")
	waitTaskTerminal(t, c, id, 15*time.Second)
	if ti := getTask(t, c, id); ti.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("task status = %v, want Succeeded", ti.Status)
	}
	var logs bytes.Buffer
	if err := c.Logs(context.Background(), id, &logs, false); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs.String(), "server_cid=wss:127.0.0.1:") {
		t.Errorf("agent's HARNESS_SERVER_CID is not wss:; logs:\n%s", logs.String())
	}
}
