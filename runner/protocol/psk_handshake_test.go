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

	var agentInfo AgentInfo
	agentInfo.RunnerId.Id = [16]byte{0x12, 0x34, 0x56, 0x78}
	copy(agentInfo.TaskId.Id[:], []byte("task0123456789ab"))
	agentInfo.SetHostname([]byte("myhost"))

	hello := ClientHello{Kind: ClientKind_Agent}
	hello.SetAgentInfo(agentInfo)

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
	if ai.RunnerId != agentInfo.RunnerId || ai.TaskId != agentInfo.TaskId {
		t.Errorf("AgentInfo ids: got %s/%x want %s/%x", ai.RunnerId.Hex(), ai.TaskId.Id, agentInfo.RunnerId.Hex(), agentInfo.TaskId.Id)
	}
	if !bytes.Equal(ai.Hostname, []byte("myhost")) {
		t.Errorf("Hostname: got %q, want %q", ai.Hostname, "myhost")
	}

	// RunnerAuth() must return nil for client role
	if got.RunnerAuth() != nil {
		t.Errorf("RunnerAuth() should be nil for client role")
	}
}

// TestPskAuthRequestRunnerRoleRoundTrip encodes a runner-role PskAuthRequest
// (no binder + RunnerAuth: key, signature, RunnerHello) and decodes it.
func TestPskAuthRequestRunnerRoleRoundTrip(t *testing.T) {
	var rh RunnerHello
	rh.Version = 3
	rh.MaxTasks = 8
	rh.SetHostname([]byte("runner-host"))
	root := AllowedRoot{}
	root.SetPath([]byte("/srv/repos/proj"))
	rh.SetAllowedRoots([]AllowedRoot{root})
	rh.SetAgentBin([]byte("claude"))

	auth := RunnerAuth{Hello: rh}
	for i := range auth.Key {
		auth.Key[i] = byte(i)
	}
	for i := range auth.Sig {
		auth.Sig[i] = byte(255 - i)
	}

	var orig PskAuthRequest
	orig.Role = AuthRole_Runner
	if !orig.SetBinder(nil) {
		t.Fatal("SetBinder returned false")
	}
	if !orig.SetRunnerAuth(auth) {
		t.Fatal("SetRunnerAuth returned false")
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
	ga := got.RunnerAuth()
	if ga == nil {
		t.Fatal("RunnerAuth() returned nil")
	}
	if ga.Key != auth.Key || ga.Sig != auth.Sig {
		t.Errorf("RunnerAuth key/sig lost in the round-trip")
	}
	if ga.Hello.Version != rh.Version {
		t.Errorf("RunnerHello.Version: got %d, want %d", ga.Hello.Version, rh.Version)
	}
	if ga.Hello.MaxTasks != rh.MaxTasks {
		t.Errorf("RunnerHello.MaxTasks: got %d, want %d", ga.Hello.MaxTasks, rh.MaxTasks)
	}
	if !bytes.Equal(ga.Hello.Hostname, []byte("runner-host")) {
		t.Errorf("RunnerHello.Hostname: got %q", ga.Hello.Hostname)
	}

	// ClientHello() must return nil for runner role
	if got.ClientHello() != nil {
		t.Errorf("ClientHello() should be nil for runner role")
	}
}

// TestPskAuthRequestNoPskRoundTrip tests binder_len=0 (no-PSK / dev mode).
func TestPskAuthRequestNoPskRoundTrip(t *testing.T) {
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

// The retired roles (0 = client_legacy, 1 = runner_legacy) have no arm: a
// request an old binary sends does not decode, which the server's gate turns
// into no_identity.
func TestPskAuthRequestRetiredRolesDoNotDecode(t *testing.T) {
	for _, role := range []byte{0, 1} {
		// binder_len=0, the role, then a byte of what used to be its hello.
		var got PskAuthRequest
		if err := got.DecodeExact([]byte{0x00, 0x00, role, 0x01}); err == nil {
			t.Errorf("role %d decoded: %+v", role, got)
		}
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
