package runner

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func testGrant(id byte, exp time.Time) protocol.DataPlaneGrant {
	g := protocol.DataPlaneGrant{
		GrantId:       [16]uint8{id},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(exp.UnixMilli()),
		Kind:          protocol.TaskControlKind_OpenFileTransfer,
	}
	g.SetDirection(protocol.FileTransferDirection_Pull)
	return g
}

func TestGrantStoreValidateHappyPath(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	if st := s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0); st != protocol.AuthorizeDataPlaneStatus_Ok {
		t.Fatalf("insert: %v", st)
	}
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now)
	if got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("want ok, got %v", got)
	}
}

func TestGrantStoreRefusesWrongDirection(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Push, now)
	if got != protocol.ClientHelloStatus_NotPermitted {
		t.Fatalf("want not_permitted, got %v", got)
	}
}

func TestGrantStoreRefusesWrongKind(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_ListFiles, 0, now)
	if got != protocol.ClientHelloStatus_NotPermitted {
		t.Fatalf("want not_permitted, got %v", got)
	}
}

func TestGrantStoreRefusesExpired(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(-time.Second)), 0x10, 0)
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now)
	if got != protocol.ClientHelloStatus_Expired {
		t.Fatalf("want expired, got %v", got)
	}
}

func TestGrantStoreRefusesUnknownAndWrongTask(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	if got := s.Validate([16]byte{2}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("unknown grant: want bad_ticket, got %v", got)
	}
	if got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{8}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_UnknownTask {
		t.Fatalf("wrong task: want unknown_task, got %v", got)
	}
}

// Overwriting would invalidate a credential a live connection may be holding —
// the mistake agentboard/registry.go's Ticket() comment records.
func TestGrantStoreDuplicateInsertRefused(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	if st := s.Insert(testGrant(1, now.Add(time.Minute)), 0x11, 0); st != protocol.AuthorizeDataPlaneStatus_DuplicateGrant {
		t.Fatalf("want duplicate_grant, got %v", st)
	}
}

func TestGrantStoreRevokeIsIdempotentAndCloses(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	closed := 0
	s.OnClose([16]byte{1}, func() { closed++ })
	if n := s.Revoke([16]byte{1}); n != 1 {
		t.Fatalf("want 1 closed, got %d", n)
	}
	if closed != 1 {
		t.Fatalf("closer not called")
	}
	if n := s.Revoke([16]byte{1}); n != 0 {
		t.Fatalf("second revoke should be a no-op, got %d", n)
	}
	if closed != 1 {
		t.Fatalf("closer called twice")
	}
}

// A revoke for a grant nobody has redeemed yet must still remove it, so the
// credential cannot be used after the authority behind it was withdrawn.
func TestGrantStoreRevokeUnredeemedStillRemoves(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	s.Revoke([16]byte{1})
	if got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("revoked grant is still redeemable: %v", got)
	}
}

func TestGrantStoreSweepDropsExpired(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(-time.Second)), 0x10, 0)
	s.Insert(testGrant(2, now.Add(time.Minute)), 0x11, 0)
	if n := s.Sweep(now); n != 1 {
		t.Fatalf("want 1 swept, got %d", n)
	}
	if got := s.Validate([16]byte{2}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("live grant was swept")
	}
}

// A redeemed entry is the connection's, and its own end removes it (Forget).
// Sweeping it on the TTL would take the OnClose hook with it, so a transfer
// running longer than the TTL would stop being reachable by a narrowing caps
// change -- which is the one thing revocation promises.
func TestGrantStoreSweepLeavesARedeemedEntryAlone(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(-time.Minute)), 0x10, 0) // expired
	s.OnClose([16]byte{1}, func() {})                      // ...but redeemed

	if n := s.Sweep(now); n != 0 {
		t.Fatalf("sweep took a redeemed entry: %d", n)
	}
	closed := 0
	s.OnClose([16]byte{1}, func() { closed++ })
	if got := s.Revoke([16]byte{1}); got != 1 || closed != 1 {
		t.Fatalf("a redeemed entry must still be revocable after its TTL: revoked=%d closed=%d", got, closed)
	}
}

// Forget is the connection reporting its own end: it drops the entry without
// closing anything, because the thing it would close is already going.
func TestGrantStoreForgetDropsWithoutClosing(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(testGrant(1, now.Add(time.Minute)), 0x10, 0)
	closed := 0
	s.OnClose([16]byte{1}, func() { closed++ })

	s.Forget([16]byte{1})
	if closed != 0 {
		t.Fatalf("Forget closed the connection it was told had already ended")
	}
	if got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("a forgotten grant is still redeemable: %v", got)
	}
}
