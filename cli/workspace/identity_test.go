package workspace

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

const (
	idTaskA = "3f2a9c00000000000000000000000001"
	idTaskB = "3f2a9c00000000000000000000000002"
)

func mustIdentity(t *testing.T, value string) ForwardKey {
	t.Helper()
	k, err := ForwardIdentity(value)
	if err != nil {
		t.Fatalf("ForwardIdentity(%q): %v", value, err)
	}
	return k
}

// The short form an operator types, the long form, and what a save renders
// from the registry are one forward: anything comparing a declaration against
// a running forward must not see three.
func TestForwardIdentityEquatesSpellings(t *testing.T) {
	rendered, ok := cli.PortForwardConfigSpec(&protocol.PortForwardInfo{
		Direction: protocol.PortForwardDirection_Local, ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
		BindAddr: []byte("127.0.0.1"), BindPort: 3000, TargetHost: []byte("127.0.0.1"), TargetPort: 3000,
	})
	if !ok {
		t.Fatal("PortForwardConfigSpec refused an os_socket -L")
	}
	short := mustIdentity(t, "-L 3000")
	for _, v := range []string{"-L 127.0.0.1:3000:127.0.0.1:3000", rendered} {
		if got := mustIdentity(t, v); got != short {
			t.Errorf("%q = %+v, want %+v (same as -L 3000)", v, got, short)
		}
	}
}

func TestForwardIdentitySeparatesDirectionAndProtocol(t *testing.T) {
	if mustIdentity(t, "-L 3000") == mustIdentity(t, "-R 3000") {
		t.Error("-L 3000 and -R 3000 compare equal")
	}
	if mustIdentity(t, "-L 3000/udp") == mustIdentity(t, "-L 3000") {
		t.Error("-L 3000/udp and -L 3000 compare equal")
	}
}

func TestForwardKeyValueRoundTrips(t *testing.T) {
	for _, in := range []string{"-L 3000/udp", "-R 0.0.0.0:8080:127.0.0.1:80"} {
		k := mustIdentity(t, in)
		v, err := k.Value()
		if err != nil {
			t.Fatalf("%q Value: %v", in, err)
		}
		if got := mustIdentity(t, v); got != k {
			t.Errorf("%q → %q → %+v, want %+v", in, v, got, k)
		}
	}
	v, _ := mustIdentity(t, "-L 3000/udp").Value()
	if !strings.HasSuffix(v, "/udp") {
		t.Errorf("Value = %q, lost the /udp suffix", v)
	}
}

// The parsers already refuse port 0, so only a key built by hand can carry it;
// Value is the last gate before a file write and refuses it on its own.
func TestForwardKeyValueRefusesPortZero(t *testing.T) {
	k := ForwardKey{Dir: ForwardLocal, BindAddr: "127.0.0.1", TargetHost: "127.0.0.1", TargetPort: 3000}
	if _, err := k.Value(); err == nil {
		t.Error("Value accepted local port 0")
	}
}

func TestAddForwardCreatesBlock(t *testing.T) {
	ws := &Workspace{Name: "dev"}
	added, err := ws.AddForward(idTaskA, "-L 3000")
	if err != nil || !added {
		t.Fatalf("AddForward = %v, %v", added, err)
	}
	want := Task{ID: idTaskA, Resume: ResumeContinue, Runner: RunnerAssigned, Forwards: []string{"-L 3000"}}
	if len(ws.Tasks) != 1 || ws.Tasks[0].ID != want.ID || ws.Tasks[0].Resume != want.Resume ||
		ws.Tasks[0].Runner != want.Runner || strings.Join(ws.Tasks[0].Forwards, "|") != "-L 3000" {
		t.Errorf("Tasks = %+v, want [%+v]", ws.Tasks, want)
	}
}

func TestAddForwardIsIdempotentAcrossSpellings(t *testing.T) {
	ws := &Workspace{Name: "dev"}
	if _, err := ws.AddForward(idTaskA, "-L 3000"); err != nil {
		t.Fatal(err)
	}
	added, err := ws.AddForward(idTaskA, "-L 127.0.0.1:3000:127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	if added || len(ws.Tasks[0].Forwards) != 1 {
		t.Errorf("added=%v forwards=%v, want the second spelling refused as a duplicate", added, ws.Tasks[0].Forwards)
	}
	if !ws.Declares(idTaskA, "-L 127.0.0.1:3000:127.0.0.1:3000") || ws.Declares(idTaskB, "-L 3000") {
		t.Error("Declares does not compare by task and identity")
	}
}

func TestAddForwardLeavesPolicy(t *testing.T) {
	ws := &Workspace{Name: "dev", Tasks: []Task{{ID: idTaskA, Resume: ResumeNo, Runner: RunnerAny}}}
	if _, err := ws.AddForward(idTaskA, "-L 3000"); err != nil {
		t.Fatal(err)
	}
	if ws.Tasks[0].Resume != ResumeNo || ws.Tasks[0].Runner != RunnerAny {
		t.Errorf("policy = %s/%s, want no/any untouched", ws.Tasks[0].Resume, ws.Tasks[0].Runner)
	}
}

func TestRemoveForwardKeepsBlock(t *testing.T) {
	ws := &Workspace{Name: "dev", Tasks: []Task{{ID: idTaskA, Resume: ResumeFresh, Forwards: []string{"-L 3000"}}}}
	removed, err := ws.RemoveForward(idTaskA, "-L 127.0.0.1:3000:127.0.0.1:3000")
	if err != nil || !removed {
		t.Fatalf("RemoveForward = %v, %v", removed, err)
	}
	if len(ws.Tasks) != 1 || len(ws.Tasks[0].Forwards) != 0 || ws.Tasks[0].Resume != ResumeFresh {
		t.Errorf("Tasks = %+v, want the block kept with no forwards", ws.Tasks)
	}
	if removed, _ := ws.RemoveForward(idTaskA, "-L 3000"); removed {
		t.Error("second RemoveForward reported a removal")
	}
}
