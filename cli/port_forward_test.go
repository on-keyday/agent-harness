package cli

import "testing"

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
		got, err := parseForwardSpec(c.in)
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
