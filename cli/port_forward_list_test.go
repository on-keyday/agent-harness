package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestPortForwardSpecString(t *testing.T) {
	var l protocol.PortForwardInfo
	l.Direction = protocol.PortForwardDirection_Local
	l.SetBindAddr([]byte("127.0.0.1"))
	l.BindPort = 8080
	l.SetTargetHost([]byte("db.internal"))
	l.TargetPort = 5432
	if got, want := PortForwardSpecString(&l), "127.0.0.1:8080 -> db.internal:5432"; got != want {
		t.Errorf("local: got %q, want %q", got, want)
	}

	var r protocol.PortForwardInfo
	r.Direction = protocol.PortForwardDirection_Remote
	r.SetBindAddr([]byte("127.0.0.1"))
	r.BindPort = 6001
	r.SetTargetHost([]byte("localhost"))
	r.TargetPort = 6000
	if got, want := PortForwardSpecString(&r), "runner:127.0.0.1:6001 -> localhost:6000"; got != want {
		t.Errorf("remote: got %q, want %q", got, want)
	}
}

func TestPortForwardSpecString_InProcess(t *testing.T) {
	fi := &protocol.PortForwardInfo{
		Direction:      protocol.PortForwardDirection_Local,
		TargetPort:     6379,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	fi.SetTargetHost([]byte("127.0.0.1"))
	if got, want := PortForwardSpecString(fi), "(in-process) -> 127.0.0.1:6379"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// The DIR column still reports the direction, not the endpoint kind.
	if got := PortForwardDirFlag(fi.Direction); got != "-L" {
		t.Fatalf("dir flag = %q, want -L", got)
	}
}

func TestPortForwardInfoJSONLine_DeclaresEndpointKind(t *testing.T) {
	inproc := &protocol.PortForwardInfo{
		ForwardId:      3,
		Direction:      protocol.PortForwardDirection_Local,
		TargetPort:     6379,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	inproc.SetTargetHost([]byte("127.0.0.1"))
	line := PortForwardInfoJSONLine(inproc)
	if !strings.Contains(line, `"client_endpoint":"in_process"`) {
		t.Fatalf("in-process forward must declare its endpoint kind, got %s", line)
	}
	// The bind pair stays empty; the declaration is what stops a consumer
	// reading that as a broken registration.
	if !strings.Contains(line, `"bind_port":0`) || !strings.Contains(line, `"bind_addr":""`) {
		t.Fatalf("bind pair must stay empty, got %s", line)
	}

	sock := &protocol.PortForwardInfo{
		ForwardId:  4,
		Direction:  protocol.PortForwardDirection_Local,
		BindPort:   18080,
		TargetPort: 5432,
	}
	sock.SetBindAddr([]byte("127.0.0.1"))
	sock.SetTargetHost([]byte("db.internal"))
	if l := PortForwardInfoJSONLine(sock); !strings.Contains(l, `"client_endpoint":"os_socket"`) {
		t.Fatalf("socket-endpoint forward must say so explicitly, got %s", l)
	}
}

func TestPortForwardInfoJSONLine(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 3
	fi.Direction = protocol.PortForwardDirection_Local
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.BindPort = 9000
	fi.SetTargetHost([]byte("svc"))
	fi.TargetPort = 80
	fi.OriginKind = protocol.ClientKind_Cli
	fi.SetOriginCid([]byte("ws:127.0.0.1:1-a"))
	line := PortForwardInfoJSONLine(&fi)
	for _, want := range []string{`"forward_id":3`, `"dir":"-L"`, `"origin_kind":"cli"`, `"origin_cid":"ws:127.0.0.1:1-a"`} {
		if !strings.Contains(line, want) {
			t.Errorf("JSON line %q missing %q", line, want)
		}
	}
}

func TestPortForwardConfigSpecRoundTrips(t *testing.T) {
	local := &protocol.PortForwardInfo{
		Direction: protocol.PortForwardDirection_Local, BindPort: 3000, TargetPort: 3000,
		ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
	}
	local.SetBindAddr([]byte("127.0.0.1"))
	local.SetTargetHost([]byte("127.0.0.1"))
	got, ok := PortForwardConfigSpec(local)
	if !ok || got != "-L 127.0.0.1:3000:127.0.0.1:3000" {
		t.Fatalf("local = %q, %v", got, ok)
	}
	if _, err := ParseForwardSpec(strings.TrimPrefix(got, "-L ")); err != nil {
		t.Errorf("the rendered -L does not parse back: %v", err)
	}

	remote := &protocol.PortForwardInfo{
		Direction: protocol.PortForwardDirection_Remote, BindPort: 8080, TargetPort: 9090,
		ClientEndpoint: protocol.ClientEndpointKind_OsSocket,
	}
	remote.SetBindAddr([]byte("127.0.0.1"))
	remote.SetTargetHost([]byte("127.0.0.1"))
	got, ok = PortForwardConfigSpec(remote)
	if !ok || got != "-R 127.0.0.1:8080:127.0.0.1:9090" {
		t.Fatalf("remote = %q, %v", got, ok)
	}
	if _, err := ParseRemoteForwardSpec(strings.TrimPrefix(got, "-R ")); err != nil {
		t.Errorf("the rendered -R does not parse back: %v", err)
	}

	inproc := &protocol.PortForwardInfo{
		Direction: protocol.PortForwardDirection_Local, TargetPort: 3000,
		ClientEndpoint: protocol.ClientEndpointKind_InProcess,
	}
	inproc.SetTargetHost([]byte("127.0.0.1"))
	if _, ok := PortForwardConfigSpec(inproc); ok {
		t.Error("an in-process forward reported itself savable")
	}
}

// Every in-process kind renders as what it IS. `(in-process)` collapsed four
// different clients into one label, so a row the operator did not create said
// nothing about whether killing it was safe — the ssh gateway's row, which
// holds a remote editor's session open, read identically to a WebUI preview
// pin.
func TestPortForwardSpecStringNamesTheInProcessKind(t *testing.T) {
	for _, tc := range []struct {
		kind protocol.ClientEndpointKind
		want string
	}{
		{protocol.ClientEndpointKind_InProcess, "(in-process)"},
		{protocol.ClientEndpointKind_InProcessStdio, "(stdio)"},
		{protocol.ClientEndpointKind_InProcessHttp, "(http)"},
		{protocol.ClientEndpointKind_InProcessPane, "(pane)"},
		{protocol.ClientEndpointKind_InProcessPreview, "(preview)"},
		{protocol.ClientEndpointKind_InProcessSshGateway, "(ssh-gateway)"},
	} {
		fi := &protocol.PortForwardInfo{
			Direction: protocol.PortForwardDirection_Local, ClientEndpoint: tc.kind, TargetPort: 60565,
		}
		fi.SetTargetHost([]byte("127.0.0.1"))
		got := PortForwardSpecString(fi)
		if !strings.HasPrefix(got, tc.want+" -> ") {
			t.Errorf("%v rendered %q, want it to start with %q", tc.kind, got, tc.want+" -> ")
		}
	}
}

// The JSON is a contract consumers script against, and its old `default:` arm
// answered "os_socket" for anything it did not recognise — so every kind added
// here would have started LYING about the one property the field exists to
// report. The predicate is what closes that, not a longer switch.
func TestClientEndpointJSONNeverCallsAnInProcessKindASocket(t *testing.T) {
	for _, k := range []protocol.ClientEndpointKind{
		protocol.ClientEndpointKind_InProcess,
		protocol.ClientEndpointKind_InProcessStdio,
		protocol.ClientEndpointKind_InProcessHttp,
		protocol.ClientEndpointKind_InProcessPane,
		protocol.ClientEndpointKind_InProcessPreview,
		protocol.ClientEndpointKind_InProcessSshGateway,
	} {
		if got := clientEndpointJSON(k); got != "in_process" {
			t.Errorf("clientEndpointJSON(%v) = %q, want in_process", k, got)
		}
	}
	if got := clientEndpointJSON(protocol.ClientEndpointKind_OsSocket); got != "os_socket" {
		t.Errorf("clientEndpointJSON(os_socket) = %q", got)
	}
}

// Savability is decided by client_endpoint alone and must stay that way: a
// workspace writes a `-L`/`-R` line only for a forward with a real OS socket
// behind it, and no in-process kind has one however specifically it is named.
func TestPortForwardConfigSpecRefusesEveryInProcessKind(t *testing.T) {
	for _, k := range []protocol.ClientEndpointKind{
		protocol.ClientEndpointKind_InProcess,
		protocol.ClientEndpointKind_InProcessStdio,
		protocol.ClientEndpointKind_InProcessHttp,
		protocol.ClientEndpointKind_InProcessPane,
		protocol.ClientEndpointKind_InProcessPreview,
		protocol.ClientEndpointKind_InProcessSshGateway,
	} {
		fi := &protocol.PortForwardInfo{Direction: protocol.PortForwardDirection_Local, ClientEndpoint: k}
		if _, ok := PortForwardConfigSpec(fi); ok {
			t.Errorf("%v was reported savable", k)
		}
	}
}

// A forward that has carried nothing prints zeros. 0 is a measurement; a blank
// column reads as "this row does not report traffic", which is a different and
// false claim.
func TestForwardRowPrintsZeroCounters(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.BindPort = 8080
	fi.SetTargetHost([]byte("localhost"))
	fi.TargetPort = 3000
	fi.SetOriginCid([]byte("ws:abc"))

	joined := strings.Join(PortForwardInfoLines([]protocol.PortForwardInfo{fi}), "\n")
	for _, want := range []string{"conns=0/0", "to-target=0", "from-target=0", "taps=0", "last=never"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("row elides a zero measurement (%q missing):\n%s", want, joined)
		}
	}
}

func TestForwardTrafficLineRendersCounts(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.BytesToTarget = 1 << 20
	fi.BytesFromTarget = 2048
	fi.ConnsTotal = 41
	fi.ConnsOpen = 3
	fi.Taps = 2
	fi.LastActivityUnixMs = uint64(time.Now().Add(-5 * time.Second).UnixMilli())

	got := PortForwardTrafficLine(&fi)
	for _, want := range []string{"conns=3/41", "to-target=1.0MB", "from-target=2.0kB", "taps=2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("traffic line missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "last=never") {
		t.Fatalf("a forward with activity must not say never: %s", got)
	}
}

func TestForwardJSONCarriesCounters(t *testing.T) {
	var fi protocol.PortForwardInfo
	fi.ForwardId = 7
	fi.SetBindAddr([]byte("127.0.0.1"))
	fi.SetTargetHost([]byte("localhost"))
	fi.SetOriginCid([]byte("ws:abc"))
	fi.BytesToTarget = 1 << 20
	fi.ConnsOpen = 2
	fi.Taps = 1

	var got map[string]any
	if err := json.Unmarshal([]byte(PortForwardInfoJSONLine(&fi)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"bytes_to_target", "bytes_from_target", "conns_total", "conns_open", "taps", "last_activity_unix_ms"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("JSON contract missing %q: %v", k, got)
		}
	}
	if got["bytes_to_target"].(float64) != float64(1<<20) {
		t.Fatalf("bytes_to_target: %v", got["bytes_to_target"])
	}
}

// One renderer for byte sizes, shared by every surface. Zero renders as "0",
// not "" — same rule as the row above.
func TestFormatByteCount(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want string
	}{{0, "0"}, {512, "512B"}, {2048, "2.0kB"}, {3 << 20, "3.0MB"}} {
		if got := FormatByteCount(c.in); got != c.want {
			t.Fatalf("FormatByteCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
