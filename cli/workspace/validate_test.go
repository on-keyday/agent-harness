package workspace

import (
	"strings"
	"testing"
)

func TestParseForwardValue(t *testing.T) {
	dir, l, _, err := ParseForwardValue("-L 3000:127.0.0.1:3000")
	if err != nil || dir != ForwardLocal || l.LocalPort != 3000 || l.RemotePort != 3000 {
		t.Errorf("-L = %v %+v %v", dir, l, err)
	}
	dir, _, r, err := ParseForwardValue("-R 8080:127.0.0.1:9090")
	if err != nil || dir != ForwardRemote || r.RunnerPort != 8080 || r.DialPort != 9090 {
		t.Errorf("-R = %v %+v %v", dir, r, err)
	}
	// The four-field form must survive too: it is what PortForwardConfigSpec
	// writes, so a save/apply round trip depends on it.
	if _, l, _, err := ParseForwardValue("-L 0.0.0.0:3000:127.0.0.1:3000"); err != nil || l.BindAddr != "0.0.0.0" {
		t.Errorf("four-field -L = %+v %v", l, err)
	}
	for _, bad := range []string{"-W host:port", "3000:127.0.0.1:3000", "-L", "-L not-a-spec", ""} {
		if _, _, _, err := ParseForwardValue(bad); err == nil {
			t.Errorf("ParseForwardValue(%q) succeeded, want an error", bad)
		}
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	for name, src := range map[string]string{
		"bad forward":            "[workspace w]\n[workspace w task 3f2a9c00000000000000000000000001]\nforward = -L nonsense\n",
		"bad grid":               "[workspace w]\ngrid = --nope\n",
		"grid flag needs anchor": "[workspace w]\ngrid = --descendants\n",
	} {
		f, err := Parse(strings.NewReader(src))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		ws, _ := f.Workspace("w")
		if err := ws.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded, want an error", name)
		}
	}
}

func TestValidateAcceptsTheSampleFile(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range f.Names() {
		ws, _ := f.Workspace(n)
		if err := ws.Validate(); err != nil {
			t.Errorf("workspace %s: %v", n, err)
		}
	}
}

// `grid =` with an empty value means the unnarrowed grid, which is a valid
// selection and not the same as omitting the key.
func TestValidateAcceptsEmptyGridValue(t *testing.T) {
	f, err := Parse(strings.NewReader("[workspace w]\ngrid =\n"))
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := f.Workspace("w")
	if !ws.GridSet {
		t.Fatal("`grid =` must set GridSet")
	}
	if err := ws.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
