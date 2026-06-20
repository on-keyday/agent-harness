package protocol

import (
	"bytes"
	"testing"
)

// TestPskAuthRequestClientRoleRoundTrip encodes a client-role PskAuthRequest
// (64-byte binder + ClientHello kind=agent + AgentInfo) and decodes it, then
// asserts all fields survive the round-trip.
func TestPskAuthRequestClientRoleRoundTrip(t *testing.T) {
	binder := make([]byte, 64)
	for i := range binder {
		binder[i] = byte(i)
	}

	// Build AgentInfo
	var agentInfo AgentInfo
	agentInfo.RunnerId.SetIpAddr([]byte{127, 0, 0, 1})
	agentInfo.RunnerId.Port = 8540
	agentInfo.RunnerId.UniqueNumber = 0x1234
	copy(agentInfo.TaskId.Id[:], []byte("task0123456789ab"))
	copy(agentInfo.AuthTicket[:], []byte("ticketticketticke"))
	agentInfo.SetHostname([]byte("myhost"))

	// Build ClientHello (kind=agent)
	hello := ClientHello{Kind: ClientKind_Agent}
	hello.SetAgentInfo(agentInfo)

	// Build PskAuthRequest
	var orig PskAuthRequest
	orig.Role = AuthRole_Client
	if !orig.SetBinder(binder) {
		t.Fatal("SetBinder returned false")
	}
	if !orig.SetClientHello(hello) {
		t.Fatal("SetClientHello returned false")
	}

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got PskAuthRequest
	remain, err := got.Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("remaining bytes after Decode: %d", len(remain))
	}

	if got.Role != AuthRole_Client {
		t.Errorf("Role: got %v, want AuthRole_Client", got.Role)
	}
	if !bytes.Equal(got.Binder, binder) {
		t.Errorf("Binder mismatch")
	}
	if got.BinderLen != uint16(len(binder)) {
		t.Errorf("BinderLen: got %d, want %d", got.BinderLen, len(binder))
	}

	ch := got.ClientHello()
	if ch == nil {
		t.Fatal("ClientHello() returned nil")
	}
	if ch.Kind != ClientKind_Agent {
		t.Errorf("ClientHello.Kind: got %v, want ClientKind_Agent", ch.Kind)
	}
	ai := ch.AgentInfo()
	if ai == nil {
		t.Fatal("AgentInfo() returned nil")
	}
	if !bytes.Equal(ai.RunnerId.IpAddr, []byte{127, 0, 0, 1}) {
		t.Errorf("RunnerId.IpAddr: got %v", ai.RunnerId.IpAddr)
	}
	if !bytes.Equal(ai.Hostname, []byte("myhost")) {
		t.Errorf("Hostname: got %q, want %q", ai.Hostname, "myhost")
	}

	// RunnerHello() must return nil for client role
	if got.RunnerHello() != nil {
		t.Errorf("RunnerHello() should be nil for client role")
	}
}

// TestPskAuthRequestRunnerRoleRoundTrip encodes a runner-role PskAuthRequest
// (64-byte binder + RunnerHello) and decodes it.
func TestPskAuthRequestRunnerRoleRoundTrip(t *testing.T) {
	binder := make([]byte, 64)
	for i := range binder {
		binder[i] = byte(255 - i)
	}

	var rh RunnerHello
	rh.Version = 3
	rh.MaxTasks = 8
	rh.SetHostname([]byte("runner-host"))
	root := AllowedRoot{}
	root.SetPath([]byte("/srv/repos/proj"))
	rh.SetAllowedRoots([]AllowedRoot{root})
	rh.SetAgentBin([]byte("claude"))

	var orig PskAuthRequest
	orig.Role = AuthRole_Runner
	if !orig.SetBinder(binder) {
		t.Fatal("SetBinder returned false")
	}
	if !orig.SetRunnerHello(rh) {
		t.Fatal("SetRunnerHello returned false")
	}

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got PskAuthRequest
	remain, err := got.Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("remaining bytes after Decode: %d", len(remain))
	}

	if got.Role != AuthRole_Runner {
		t.Errorf("Role: got %v, want AuthRole_Runner", got.Role)
	}
	if !bytes.Equal(got.Binder, binder) {
		t.Errorf("Binder mismatch")
	}

	gotRH := got.RunnerHello()
	if gotRH == nil {
		t.Fatal("RunnerHello() returned nil")
	}
	if gotRH.Version != rh.Version {
		t.Errorf("RunnerHello.Version: got %d, want %d", gotRH.Version, rh.Version)
	}
	if gotRH.MaxTasks != rh.MaxTasks {
		t.Errorf("RunnerHello.MaxTasks: got %d, want %d", gotRH.MaxTasks, rh.MaxTasks)
	}
	if !bytes.Equal(gotRH.Hostname, []byte("runner-host")) {
		t.Errorf("RunnerHello.Hostname: got %q", gotRH.Hostname)
	}

	// ClientHello() must return nil for runner role
	if got.ClientHello() != nil {
		t.Errorf("ClientHello() should be nil for runner role")
	}
}

// TestPskAuthRequestNoPskRoundTrip tests binder_len=0 (no-PSK / dev mode).
func TestPskAuthRequestNoPskRoundTrip(t *testing.T) {
	// client role, zero-length binder
	hello := ClientHello{Kind: ClientKind_Cli}

	var orig PskAuthRequest
	orig.Role = AuthRole_Client
	// leave binder empty (SetBinder with nil/empty is valid: binder_len == 0)
	if !orig.SetBinder(nil) {
		t.Fatal("SetBinder(nil) returned false")
	}
	if !orig.SetClientHello(hello) {
		t.Fatal("SetClientHello returned false")
	}

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got PskAuthRequest
	remain, err := got.Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("remaining bytes after Decode: %d", len(remain))
	}

	if got.BinderLen != 0 {
		t.Errorf("BinderLen: got %d, want 0", got.BinderLen)
	}
	if len(got.Binder) != 0 {
		t.Errorf("Binder: got %d bytes, want 0", len(got.Binder))
	}
	if got.Role != AuthRole_Client {
		t.Errorf("Role: got %v, want AuthRole_Client", got.Role)
	}
	ch := got.ClientHello()
	if ch == nil {
		t.Fatal("ClientHello() returned nil")
	}
	if ch.Kind != ClientKind_Cli {
		t.Errorf("ClientHello.Kind: got %v, want ClientKind_Cli", ch.Kind)
	}
}

// TestPskAuthResponseRoundTrip verifies all status values round-trip.
func TestPskAuthResponseRoundTrip(t *testing.T) {
	statuses := []PskAuthStatus{
		PskAuthStatus_Ok,
		PskAuthStatus_BadPsk,
		PskAuthStatus_BadTicket,
		PskAuthStatus_NoIdentity,
	}
	for _, status := range statuses {
		orig := PskAuthResponse{Status: status}
		buf, err := orig.Append(nil)
		if err != nil {
			t.Fatalf("Append(%v): %v", status, err)
		}
		var got PskAuthResponse
		remain, err := got.Decode(buf)
		if err != nil {
			t.Fatalf("Decode(%v): %v", status, err)
		}
		if len(remain) != 0 {
			t.Fatalf("remaining bytes after Decode: %d", len(remain))
		}
		if got.Status != status {
			t.Errorf("Status: got %v, want %v", got.Status, status)
		}
	}
}
