package cli

import (
	"io"
	"net"
	"path/filepath"
	"testing"
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
