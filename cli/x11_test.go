package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseXauthCookie(t *testing.T) {
	out := "myhost/unix:0  MIT-MAGIC-COOKIE-1  0123456789abcdef0123456789abcdef\n" +
		"myhost/unix:1  MIT-MAGIC-COOKIE-1  ffffffffffffffffffffffffffffffff\n"
	cookie, err := parseXauthCookie(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	if !reflect.DeepEqual(cookie, want) {
		t.Fatalf("cookie = %x, want %x", cookie, want)
	}
}

func TestParseXauthCookie_NoMatch(t *testing.T) {
	if _, err := parseXauthCookie("otherhost/unix:5  MIT-MAGIC-COOKIE-1  abcd\n", 0); err == nil {
		t.Fatal("expected error for missing display :0")
	}
}

func TestParseXauthCookie_NoFalseSuffixMatch(t *testing.T) {
	// display :21 must NOT satisfy a lookup for n=1 (the ":N" suffix is
	// colon-anchored, so :21 does not match :1).
	in := "host/unix:21  MIT-MAGIC-COOKIE-1  0123456789abcdef0123456789abcdef\n"
	if _, err := parseXauthCookie(in, 1); err == nil {
		t.Fatal("display :21 wrongly matched n=1")
	}
}

func TestX11DisplayNumber(t *testing.T) {
	cases := []struct {
		display string
		want    int
		wantErr bool
	}{
		{":0", 0, false},
		{"unix:2", 2, false},
		{"localhost:10.0", 10, false},
		{"192.168.0.5:1", 1, false},
		{"", 0, true},
		{"garbage", 0, true},
	}
	for _, tc := range cases {
		got, err := x11DisplayNumber(tc.display)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want err", tc.display)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got (%d,%v) want %d", tc.display, got, err, tc.want)
		}
	}
}

func TestLocalXServerDialSpec(t *testing.T) {
	cases := []struct {
		display     string
		wantNetwork string
		wantHost    string
		wantPort    int
		wantErr     bool
	}{
		{":0", "unix", "/tmp/.X11-unix/X0", 0, false},
		{"unix:0", "unix", "/tmp/.X11-unix/X0", 0, false},
		{":2", "unix", "/tmp/.X11-unix/X2", 0, false},
		{"localhost:0", "tcp", "localhost", 6000, false},
		{"192.168.0.5:1", "tcp", "192.168.0.5", 6001, false},
		{"", "", "", 0, true},
	}
	for _, tc := range cases {
		network, host, port, err := localXServerDialSpec(tc.display)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want err", tc.display)
			}
			continue
		}
		if err != nil || network != tc.wantNetwork || host != tc.wantHost || port != tc.wantPort {
			t.Errorf("%q: got (%q,%q,%d,%v)", tc.display, network, host, port, err)
		}
	}
}

// The TUI asks this before it cancels a live session to restart it with X11:
// a restart that cannot open X afterwards must not kill the agent first.
func TestLocalX11Display(t *testing.T) {
	t.Setenv("DISPLAY", "")
	if _, err := LocalX11Display(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty DISPLAY: err = %v", err)
	}
	t.Setenv("DISPLAY", "localhost:0")
	if d, err := LocalX11Display(); err != nil || d != "localhost:0" {
		t.Fatalf("localhost:0: %q, %v", d, err)
	}
	t.Setenv("DISPLAY", "nonsense")
	if _, err := LocalX11Display(); err == nil {
		t.Fatal("a DISPLAY without a display number was accepted")
	}
}

// On Windows ":0" means the path /tmp/.X11-unix/X0, where no X server is reached: it passed the check and
// then failed to dial, after the TUI had already restarted the agent for it.
func TestLocalX11DisplayForWindowsNeedsAHost(t *testing.T) {
	if _, err := localX11DisplayFor(":0", "windows"); err == nil || !strings.Contains(err.Error(), "localhost:0") {
		t.Fatalf(":0 on windows: err = %v; want a refusal naming localhost:0", err)
	}
	if d, err := localX11DisplayFor("localhost:0", "windows"); err != nil || d != "localhost:0" {
		t.Fatalf("localhost:0 on windows: %q, %v", d, err)
	}
	if _, err := localX11DisplayFor(":0", "linux"); err != nil {
		t.Fatalf(":0 on linux: %v", err)
	}
}
