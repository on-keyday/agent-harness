package tui

import "testing"

func TestParseSSHGateway(t *testing.T) {
	cases := []struct {
		in       []string
		wantSub  string
		wantAddr string
		wantErr  bool
	}{
		{nil, "status", "", false},
		{[]string{"start"}, "start", "127.0.0.1:2222", false},
		{[]string{"start", "127.0.0.1:2300"}, "start", "127.0.0.1:2300", false},
		{[]string{"stop"}, "stop", "", false},
		{[]string{"bogus"}, "", "", true},
		{[]string{"start", "a", "b"}, "", "", true},
		{[]string{"stop", "x"}, "", "", true},
	}
	for _, tc := range cases {
		act, err := parseSSHGateway(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSSHGateway(%v) = nil error, want one", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseSSHGateway(%v): %v", tc.in, err)
		}
		g, ok := act.(SSHGatewayAction)
		if !ok {
			t.Fatalf("parseSSHGateway(%v) returned %T", tc.in, act)
		}
		if g.Sub != tc.wantSub || g.Listen != tc.wantAddr {
			t.Errorf("parseSSHGateway(%v) = %+v, want sub=%q listen=%q", tc.in, g, tc.wantSub, tc.wantAddr)
		}
	}
}

// The gateway is one per TUI. A second start must not silently replace the
// first — the operator would be left with a listener they cannot see the
// address of and cannot stop.
func TestSSHGateway_StartStopLifecycle(t *testing.T) {
	a := New(Config{})

	a.Update(SSHGatewayStartedMsg{Listen: "127.0.0.1:2222", Cancel: func() {}})
	if a.sshGateway == nil {
		t.Fatal("after a started message the App must hold the gateway")
	}
	if got := a.sshGateway.Listen; got != "127.0.0.1:2222" {
		t.Errorf("Listen = %q, want 127.0.0.1:2222", got)
	}

	cmd := a.runSSHGatewayAction(SSHGatewayAction{Sub: "start", Listen: "127.0.0.1:2300"})
	if cmd != nil {
		t.Error("a second start must be refused, not dispatched")
	}
	if a.sshGateway.Listen != "127.0.0.1:2222" {
		t.Error("the refused start replaced the running gateway's address")
	}

	a.Update(SSHGatewayStoppedMsg{})
	if a.sshGateway != nil {
		t.Error("after a stopped message the entry must be gone")
	}
}

func TestSSHGateway_StopWithNoneRunning(t *testing.T) {
	a := New(Config{})
	if cmd := a.runSSHGatewayAction(SSHGatewayAction{Sub: "stop"}); cmd != nil {
		t.Error("stopping with nothing running must report, not dispatch")
	}
}
