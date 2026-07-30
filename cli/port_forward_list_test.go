package cli

import (
	"strings"
	"testing"

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
