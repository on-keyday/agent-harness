package runner

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/peer"
)

// TestListenAcceptsIncomingDial drives a Listen() runner with a client
// peer.Dial from the same process. Verifies the runner reaches the
// post-ECDH wrap stage (endpoint construction + accept loop wires up the
// inbound connection) before ctx cancel. The test does not exchange a
// PSK — that would require the client to know the runner's PSK byte,
// which is covered by Task 7's integration test.
func TestListenAcceptsIncomingDial(t *testing.T) {
	const listenAddr = "127.0.0.1:18542"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := ListenConfig{
		Config: Config{
			AllowedRoots: []string{t.TempDir()},
			MaxTasks:     1,
			Hostname:     "test-runner",
		},
		WSListen: listenAddr,
	}

	listenDone := make(chan error, 1)
	go func() {
		listenDone <- ListenAndServe(ctx, cfg)
	}()
	time.Sleep(200 * time.Millisecond) // give listener a moment to bind

	clientCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}

	clientEP, err := cli.BuildClientEndpoint(clientCID)
	if err != nil {
		t.Fatalf("build client EP: %v", err)
	}
	go objproto.AutoGarbageCollect(clientEP, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pc, err := peer.Dial(ctx, clientEP, clientCID, peer.DialConfig{})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer pc.Close()

	time.Sleep(500 * time.Millisecond)
	cancel()

	if err := <-listenDone; err != nil && err != context.Canceled {
		t.Fatalf("listen returned: %v", err)
	}
}
