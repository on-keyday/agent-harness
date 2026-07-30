package cli

import (
	"io"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestDialAndSplice_UnixTarget(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "echo.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c) // echo
		c.Close()
	}()

	sp := RemoteForwardSpec{DialNetwork: "unix", DialHost: sock}
	conn, err := dialForwardTarget(sp)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", buf)
	}
}

func TestDialAndSplice_TCPTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	sp := RemoteForwardSpec{DialNetwork: "tcp", DialHost: "127.0.0.1", DialPort: port}
	conn, err := dialForwardTarget(sp)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q", buf)
	}
}

func TestParseForwardSpec(t *testing.T) {
	cases := []struct {
		in      string
		bind    string
		lport   int
		rhost   string
		rport   int
		wantErr bool
	}{
		{"3000:127.0.0.1:3000", "127.0.0.1", 3000, "127.0.0.1", 3000, false},
		{"0.0.0.0:8080:10.0.0.5:80", "0.0.0.0", 8080, "10.0.0.5", 80, false},
		{"3000:localhost:3000", "127.0.0.1", 3000, "localhost", 3000, false},
		{"badspec", "", 0, "", 0, true},
		{"3000:host", "", 0, "", 0, true},
		{"notaport:host:80", "", 0, "", 0, true},
		{"3000:host:notaport", "", 0, "", 0, true},
	}
	for _, c := range cases {
		got, err := ParseForwardSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.in, err)
			continue
		}
		if got.BindAddr != c.bind || got.LocalPort != c.lport ||
			got.RemoteHost != c.rhost || got.RemotePort != c.rport {
			t.Errorf("%q: got %+v", c.in, got)
		}
	}
}

func TestParsePortForwardEvents_SplitAndCoalesced(t *testing.T) {
	var n protocol.PortForwardEvent
	n.Kind = protocol.PortForwardEventKind_ConnNotify
	n.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: 11})
	var cl protocol.PortForwardEvent
	cl.Kind = protocol.PortForwardEventKind_Closed
	cl.SetClosed(protocol.PortForwardClosed{Reason: protocol.PortForwardCloseReason_Killed})

	full := cl.MustAppend(n.MustAppend(nil))

	// Coalesced: both events in one buffer.
	evs, rest := parsePortForwardEvents(full)
	if len(evs) != 2 || len(rest) != 0 {
		t.Fatalf("coalesced: got %d events, %d rest bytes", len(evs), len(rest))
	}
	if evs[0].ConnNotify().StreamId != 11 || evs[1].Closed().Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("coalesced: wrong payloads: %+v", evs)
	}

	// Split: one byte at a time; events must appear exactly once, in order.
	var got []protocol.PortForwardEvent
	var buf []byte
	for i := 0; i < len(full); i++ {
		buf = append(buf, full[i])
		var evs []protocol.PortForwardEvent
		evs, buf = parsePortForwardEvents(buf)
		got = append(got, evs...)
	}
	if len(got) != 2 || len(buf) != 0 {
		t.Fatalf("split: got %d events, %d leftover bytes", len(got), len(buf))
	}
	if got[0].Kind != protocol.PortForwardEventKind_ConnNotify || got[1].Kind != protocol.PortForwardEventKind_Closed {
		t.Fatalf("split: wrong order: %+v", got)
	}
}

// TestParsePortForwardEvents_CoalescedSameKind guards against payload
// aliasing between records of the SAME kind coalesced in one read. Unlike
// TestParsePortForwardEvents_SplitAndCoalesced's ConnNotify+Closed pair —
// where the two kinds force a fresh variant allocation on the second decode
// and so cannot detect a `var ev` hoisted out of the decode loop — two
// ConnNotify records reuse the same underlying *tmp3915 pointer if `ev` is
// reused across iterations (PortForwardEvent.DecodeSlice keeps the existing
// union value when its concrete type already matches). That would silently
// rewrite the first event's StreamId to the second's. This is the
// production hot path: two accepted -R connections landing in one 64KiB
// ReadDirect.
func TestParseStdioForwardSpec(t *testing.T) {
	host, port, err := ParseStdioForwardSpec("127.0.0.1:6379")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if host != "127.0.0.1" || port != 6379 {
		t.Fatalf("got %s:%d", host, port)
	}
	if h, p, err := ParseStdioForwardSpec("localhost:3000"); err != nil || h != "localhost" || p != 3000 {
		t.Fatalf("hostname form: %s:%d err=%v", h, p, err)
	}
	for _, bad := range []string{"", "nope", "127.0.0.1", ":3000", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:70000", "a:b:c"} {
		if _, _, err := ParseStdioForwardSpec(bad); err == nil {
			t.Fatalf("expected error on %q", bad)
		}
	}
}

func TestParsePortForwardEvents_CoalescedSameKind(t *testing.T) {
	var a protocol.PortForwardEvent
	a.Kind = protocol.PortForwardEventKind_ConnNotify
	a.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: 22})
	var b protocol.PortForwardEvent
	b.Kind = protocol.PortForwardEventKind_ConnNotify
	b.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: 33})

	full := b.MustAppend(a.MustAppend(nil))

	evs, rest := parsePortForwardEvents(full)
	if len(evs) != 2 || len(rest) != 0 {
		t.Fatalf("got %d events, %d rest bytes", len(evs), len(rest))
	}
	id0, id1 := evs[0].ConnNotify().StreamId, evs[1].ConnNotify().StreamId
	if id0 != 22 || id1 != 33 {
		t.Fatalf("same-kind coalesced: ids = [%d %d], want [22 33] (payload aliasing between records)", id0, id1)
	}
}
