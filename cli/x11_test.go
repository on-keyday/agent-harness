package cli

import (
	"reflect"
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
