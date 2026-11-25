package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A data-plane grant is redeemable at the runner for its TTL whoever holds it,
// so it has to be withdrawn when the authority that minted it ends — not only
// when its target's does (credential-tasks anti-goal 3: nothing a credential
// was handed outlives its revoke, expiry or re-key). These tests cover the
// server's half: the requester is recorded, and its end sends the runner a
// revoke; the runner's half (a revoked grant is refused) is
// runner/dataplane_handler_test.go.

// revokedGrants is the grant ids conn was told to revoke.
func revokedGrants(t *testing.T, conn *fakeConn) map[[16]byte]bool {
	t.Helper()
	out := map[[16]byte]bool{}
	for _, msg := range conn.Sent() {
		if len(msg) == 0 || appwire.AppKind(msg[0]) != appwire.AppKind_RunnerControl {
			continue
		}
		var rr protocol.RunnerRequest
		if _, err := rr.Decode(msg[1:]); err != nil {
			t.Fatalf("decode runner request: %v", err)
		}
		if r := rr.RevokeDataPlane(); rr.Kind == protocol.RunnerRequestType_RevokeDataPlane && r != nil {
			out[r.GrantId] = true
		}
	}
	return out
}

func grantRecorded(s *Server, id [16]byte) bool {
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	for _, gs := range s.issuedGrants {
		for _, g := range gs {
			if g.grantID == id {
				return true
			}
		}
	}
	return false
}

// openDataPlane names the requesting connection's principal to the hook that
// records the grant: a credential's grant into another task's worktree is
// remembered under the credential, not only under that task.
func TestDataPlaneGrantRecordsItsRequester(t *testing.T) {
	tasks := NewTaskStore()
	h := newTestTaskHandler(tasks)
	_, pub := newCredentialKey(t)
	credID, tid := issueCredential(t, tasks, authority.Grant{Caps: protocol.Capability_FileRead}, pub, time.Now().Add(time.Hour))
	conn := recordCredentialConn(t, h, tid, pub, 9601)
	target := taskIDFromHex(tasks.Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, ""))
	runner := &RunnerEntry{Conn: &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9602-1")}}

	var requester *string
	h.SetupDataPlane = func(_ context.Context, _ objproto.ConnectionID, _ *RunnerEntry, _ protocol.DataPlaneGrant, _ bool, req string) (uint16, error) {
		requester = &req
		return 7, nil
	}
	if _, err := h.openDataPlane(conn, runner, protocol.TaskControlKind_OpenFileTransfer,
		protocol.FileTransferDirection_Pull, target, protocol.DataPlaneRoute_Forwarded); err != nil {
		t.Fatalf("openDataPlane: %v", err)
	}
	if requester == nil {
		t.Fatal("the hook was not called")
	}
	if *requester != credID {
		t.Fatalf("grant recorded with requester %q, want the credential %s", *requester, credID)
	}
}

// Every end of a credential withdraws the grants it requested, and only
// those: another principal's grant into the same task stays.
func TestCredentialEndWithdrawsItsDataPlaneGrants(t *testing.T) {
	ends := map[string]func(t *testing.T, s *Server, id string, tid protocol.TaskID){
		"revoke": func(t *testing.T, s *Server, id string, _ protocol.TaskID) {
			s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
		},
		"expiry": func(t *testing.T, s *Server, id string, _ protocol.TaskID) {
			if !s.Tasks().ExpireCredential(id, time.Now().Add(2*time.Hour)) {
				t.Fatal("ExpireCredential ended nothing")
			}
		},
		"rekey": func(t *testing.T, s *Server, _ string, tid protocol.TaskID) {
			_, newPub := newCredentialKey(t)
			r := s.taskHandler.credentialRekey(tcid("ws:127.0.0.1:9619-1"),
				&protocol.CredentialRekeyRequest{TaskId: tid, Pubkey: newPub, TtlMs: uint64(time.Hour / time.Millisecond)}, time.Now(), protocol.GrantConditions{})
			if r.Status != protocol.CredentialStatus_Ok {
				t.Fatalf("re-key: %v", r.Status)
			}
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			s := credentialBoardServer(t)
			_, pub := newCredentialKey(t)
			credID, tid := issueCredential(t, s.Tasks(), authority.Grant{Caps: protocol.Capability_FileRead}, pub, time.Now().Add(time.Hour))
			target := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
			runnerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9610-1")}
			entry := &RunnerEntry{Conn: runnerConn}
			now := uint64(time.Now().UnixMilli())
			mine, other := [16]byte{0xc1}, [16]byte{0x0f}
			s.rememberGrant(target, issuedGrant{grantID: mine, entry: entry, requester: credID, issuedUnixMs: now})
			s.rememberGrant(target, issuedGrant{grantID: other, entry: entry, requester: "", issuedUnixMs: now})

			end(t, s, credID, tid)

			revoked := revokedGrants(t, runnerConn)
			if !revoked[mine] {
				t.Errorf("the runner was not told to revoke the credential's grant after its %s", name)
			}
			if grantRecorded(s, mine) {
				t.Errorf("the credential's grant is still recorded after its %s", name)
			}
			if revoked[other] || !grantRecorded(s, other) {
				t.Errorf("the operator's grant into the same task was withdrawn too (revoked=%v)", revoked[other])
			}
		})
	}
}

// The same withdrawal serves a narrowing caps change, which names the
// narrowed task: what it requested goes as well as what targets its worktree.
func TestRevokeDataPlaneForTaskCoversRequesterAndTarget(t *testing.T) {
	s := &Server{}
	now := uint64(time.Now().UnixMilli())
	s.rememberGrant("T", issuedGrant{grantID: [16]byte{1}, requester: "A", issuedUnixMs: now})
	s.rememberGrant("A", issuedGrant{grantID: [16]byte{2}, requester: "", issuedUnixMs: now})
	s.rememberGrant("T", issuedGrant{grantID: [16]byte{3}, requester: "B", issuedUnixMs: now})
	if n := s.revokeDataPlaneForTask("A"); n != 2 {
		t.Fatalf("revoked %d grants for A, want 2 (one requested by A, one into A)", n)
	}
	if grantRecorded(s, [16]byte{1}) || grantRecorded(s, [16]byte{2}) {
		t.Fatal("a grant of A is still recorded")
	}
	if !grantRecorded(s, [16]byte{3}) {
		t.Fatal("B's grant into T went with A's")
	}
}
