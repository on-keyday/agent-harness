package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// M2: the end of a credential publishes its task event before it drops the
// board state, and the publication may wait on subscribers. A re-key landing
// in that gap revives the credential and a new-key connection attaches; the
// end's board revoke must then leave the new holder's state (and the grants
// it has minted) alone. The seam runs the re-key and the attach at exactly
// that point.
func TestCredentialEndAfterRekeyKeepsTheNewHolder(t *testing.T) {
	s := credentialBoardServer(t)
	id, tid, _ := credentialOnBoard(t, s, authority.Grant{Caps: protocol.Capability_FileRead}, 9640)
	target := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
	runnerConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9649-1")}
	newGrant := [16]byte{0x4e}

	s.beforeCredentialEndRevoke = func() {
		s.beforeCredentialEndRevoke = nil
		_, newPub := newCredentialKey(t)
		r := s.taskHandler.credentialRekey("ws:127.0.0.1:9648-1",
			&protocol.CredentialRekeyRequest{TaskId: tid, Pubkey: newPub, TtlMs: uint64(time.Hour / time.Millisecond)}, time.Now())
		if r.Status != protocol.CredentialStatus_Ok {
			t.Fatalf("re-key: %v", r.Status)
		}
		newConn := recordCredentialConn(t, s.taskHandler, tid, newPub, 9641)
		newConn.autoSendStreams = true
		s.taskHandler.handleAgentSubscribe(newConn, 1, "ci.events", false)
		if resp := lastTaskControlResponse(t, newConn); resp.AgentSubscribe() == nil || resp.AgentSubscribe().Status != protocol.SubscribeStatus_Ok {
			t.Fatalf("new holder's subscribe answered %v", resp.Kind)
		}
		s.rememberGrant(target, issuedGrant{grantID: newGrant, entry: &RunnerEntry{Conn: runnerConn},
			requester: id, issuedUnixMs: uint64(time.Now().UnixMilli())})
	}
	s.Tasks().Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
	if s.beforeCredentialEndRevoke != nil {
		t.Fatal("the seam never ran: the end took no credential branch")
	}

	row, ok := boardRowOf(s, tid)
	if !ok || !rowHasPattern(row, "ci.events") {
		t.Errorf("the end's revoke dropped the new key holder's board state (row %+v, ok=%v)", row, ok)
	}
	if revokedGrants(t, runnerConn)[newGrant] || !grantRecorded(s, newGrant) {
		t.Error("the end withdrew a grant the new key holder minted after the re-key")
	}
}

// M3: progress_set and lifecycle_set write the principal's own row, and only
// while the principal holds anything: an old-key connection after a re-key,
// or any connection after the end, writes nothing.
func TestDeadCredentialConnWritesNoOwnRow(t *testing.T) {
	for name, end := range map[string]func(t *testing.T, h *TaskHandler, id string){
		"rekey": func(t *testing.T, h *TaskHandler, id string) {
			_, newPub := newCredentialKey(t)
			if _, err := h.Tasks.RekeyCredential(id, newPub, time.Now().Add(time.Hour), nil, AuthorityWrite{}); err != nil {
				t.Fatal(err)
			}
		},
		"revoke": func(t *testing.T, h *TaskHandler, id string) {
			h.Tasks.Cancel(id, protocol.EndReason_Cancelled, protocol.TaskID{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			tasks := NewTaskStore()
			h := newTestTaskHandler(tasks)
			_, pub := newCredentialKey(t)
			id, tid := issueCredential(t, tasks, authority.Grant{}, pub, time.Now().Add(time.Hour))
			conn := recordCredentialConn(t, h, tid, pub, 9650)
			if r := progressSetAs(t, h, conn, "live", false); r.Status != protocol.ProgressSetStatus_Ok {
				t.Fatalf("a live credential's progress_set: %v", r.Status)
			}
			end(t, h, id)
			if r := progressSetAs(t, h, conn, "after", false); r.Status != protocol.ProgressSetStatus_NotFound {
				t.Errorf("progress_set after the %s: %v, want not_found", name, r.Status)
			}
			if r := lifecycleSetAs(t, h, conn, protocol.LifecycleState_Working, 100); r.Status != protocol.LifecycleSetStatus_NotFound {
				t.Errorf("lifecycle_set after the %s: %v, want not_found", name, r.Status)
			}
			if e, _ := tasks.Get(id); e.Progress != "live" || e.Lifecycle != 0 {
				t.Errorf("the row was written after the %s: progress %q lifecycle %v", name, e.Progress, e.Lifecycle)
			}
		})
	}
}
