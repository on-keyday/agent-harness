//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// The server asking a CLIENT for its own transport state, which is the only
// direction on this connection that goes server -> client.
//
// It exists because a client's transport is reachable from nowhere else: it is
// neither the server nor a runner, and the drop that matters most to a udp
// forward -- a datagram refused at the congestion gate INSIDE trsf's run loop,
// after SendDatagram already returned nil -- never reaches the client's own
// code as an error either. Measured before this existed: a forward row read
// queued=0 while the client's own counter stood at 4,417.
func TestClientTrsfStateIsReadableThroughTheServer(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	addr := "127.0.0.1:18577"
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

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// The cid as the SERVER sees it, read off the connection list exactly as an
	// operator would. Not c.Conn().ConnectionID(): each side names a connection
	// by the OTHER's address, so the client's own view of its id is not the key
	// the server holds it under.
	conns, err := c.ConnListWith(ctx)
	if err != nil {
		t.Fatalf("conns: %v", err)
	}
	own := ""
	for i := range conns {
		if conns[i].Role == protocol.ConnRole_Cli {
			own = string(conns[i].Cid)
		}
	}
	if own == "" {
		t.Fatal("no cli connection in the list; nothing to ask about")
	}

	rows, sampled, err := c.TrsfStateOn(ctx, cli.TrsfPeer{
		Target: protocol.TrsfTarget_Client,
		CID:    own,
	})
	if err != nil {
		t.Fatalf("TrsfStateOn(client): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the one connection the client holds", len(rows))
	}
	if sampled == 0 {
		t.Error("sampled_unix_ns is zero: the row's counters are read as rates over an interval, and the answerer's clock is what measures it")
	}
	row := rows[0]
	if row.Role != protocol.ConnRole_Cli {
		t.Errorf("role = %v, want cli: the row must say whose transport this is", row.Role)
	}
	if got := string(row.Cid); got == "" {
		t.Error("the row carries no cid: nothing says whose transport this is")
	}
	// A connection that has completed a handshake has an MTU. This is the
	// cheapest proof the row carries the client's REAL state rather than a
	// zero value the server made up.
	if mtu, ok := row.Counter(protocol.TrsfCounterKey_Mtu); !ok || mtu == 0 {
		t.Errorf("mtu counter = %d (present=%v), want the client's own non-zero MTU", mtu, ok)
	}

	cancel()
	<-serverDone
}

// Naming a connection that is not there is a stale id, not a broken server, and
// the caller is told which.
func TestClientTrsfStateOnAnUnknownConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)

	addr := "127.0.0.1:18579"
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

	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_, _, err = c.TrsfStateOn(ctx, cli.TrsfPeer{
		Target: protocol.TrsfTarget_Client,
		CID:    "ws:127.0.0.1:1-2",
	})
	if err == nil {
		t.Fatal("asking about a connection that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "no such runner is registered") {
		t.Logf("error was %v", err)
	}

	cancel()
	<-serverDone
}

// --runner and --client name different peers. The refusal belongs to the verb,
// so it is one rule rather than three surfaces each remembering it.
func TestTrsfPeerRefusesBothFlags(t *testing.T) {
	if _, err := cli.TrsfPeerFor("r", "c"); err == nil {
		t.Error("--runner and --client together were accepted")
	}
	p, err := cli.TrsfPeerFor("", "")
	if err != nil || p.Target != protocol.TrsfTarget_Server {
		t.Errorf("neither flag gave %+v, %v; want the server", p, err)
	}
	if p, err := cli.TrsfPeerFor("r", ""); err != nil || p.Target != protocol.TrsfTarget_Runner || p.CID != "r" {
		t.Errorf("--runner gave %+v, %v", p, err)
	}
	if p, err := cli.TrsfPeerFor("", "c"); err != nil || p.Target != protocol.TrsfTarget_Client || p.CID != "c" {
		t.Errorf("--client gave %+v, %v", p, err)
	}
}
