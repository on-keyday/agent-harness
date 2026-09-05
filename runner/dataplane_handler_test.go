package runner

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func testServerCID(id uint16) objproto.ConnectionID {
	return objproto.NewConnectionID("udp",
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 8539), id)
}

func liveGrant(id byte) protocol.DataPlaneGrant {
	return protocol.DataPlaneGrant{
		GrantId:       [16]uint8{id},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Kind:          protocol.TaskControlKind_ListFiles,
	}
}

func TestHandleAuthorizeDataPlaneInsertsAndAnswersOk(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x01)}
	var sent []protocol.AuthorizeDataPlaneResponse

	req := protocol.AuthorizeDataPlaneRequest{SlotId: 0x20, Grant: liveGrant(5)}
	handleAuthorizeDataPlane(context.Background(), slog.Default(), sess, nil, req,
		func(r protocol.AuthorizeDataPlaneResponse) error { sent = append(sent, r); return nil })

	if len(sent) != 1 || sent[0].Status != protocol.AuthorizeDataPlaneStatus_Ok {
		t.Fatalf("want one ok response, got %+v", sent)
	}
	if got := sess.Grants.Validate([16]byte{5}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_ListFiles, 0, time.Now()); got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("grant not stored: %v", got)
	}
}

// Same rule EstablishRelay enforces: a slot equal to the server conn's id would
// resolve an inbound dial to that conn instead of producing a new one.
func TestHandleAuthorizeDataPlaneRejectsSlotCollision(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x30)}
	var sent []protocol.AuthorizeDataPlaneResponse

	req := protocol.AuthorizeDataPlaneRequest{SlotId: 0x30, Grant: liveGrant(6)}
	handleAuthorizeDataPlane(context.Background(), slog.Default(), sess, nil, req,
		func(r protocol.AuthorizeDataPlaneResponse) error { sent = append(sent, r); return nil })

	if len(sent) != 1 || sent[0].Status != protocol.AuthorizeDataPlaneStatus_SlotCollision {
		t.Fatalf("want slot_collision, got %+v", sent)
	}
	if got := sess.Grants.Validate([16]byte{6}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_ListFiles, 0, time.Now()); got == protocol.ClientHelloStatus_Ok {
		t.Fatalf("a refused authorize still stored the grant")
	}
}

func TestHandleAuthorizeDataPlaneRejectsDuplicate(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x01)}
	var sent []protocol.AuthorizeDataPlaneResponse
	send := func(r protocol.AuthorizeDataPlaneResponse) error { sent = append(sent, r); return nil }

	req := protocol.AuthorizeDataPlaneRequest{SlotId: 0x20, Grant: liveGrant(7)}
	handleAuthorizeDataPlane(context.Background(), slog.Default(), sess, nil, req, send)
	handleAuthorizeDataPlane(context.Background(), slog.Default(), sess, nil, req, send)

	if len(sent) != 2 || sent[1].Status != protocol.AuthorizeDataPlaneStatus_DuplicateGrant {
		t.Fatalf("want duplicate_grant on the second, got %+v", sent)
	}
}

func TestHandleRevokeDataPlaneClosesAndReportsCount(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x01)}
	sess.Grants.Insert(liveGrant(9), 0x40, 0)
	closed := false
	sess.Grants.OnClose([16]byte{9}, func() { closed = true })

	var sent []protocol.RevokeDataPlaneResponse
	handleRevokeDataPlane(sess, protocol.RevokeDataPlaneRequest{GrantId: [16]uint8{9}},
		func(r protocol.RevokeDataPlaneResponse) error { sent = append(sent, r); return nil })

	if !closed {
		t.Fatalf("revoke did not close the live connection")
	}
	if len(sent) != 1 || sent[0].Closed != 1 {
		t.Fatalf("want closed=1, got %+v", sent)
	}
}

// A revoke is a message and messages arrive twice or never; the second one must
// be answered, not dropped.
func TestHandleRevokeDataPlaneUnknownGrantIsOk(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x01)}
	var sent []protocol.RevokeDataPlaneResponse
	handleRevokeDataPlane(sess, protocol.RevokeDataPlaneRequest{GrantId: [16]uint8{99}},
		func(r protocol.RevokeDataPlaneResponse) error { sent = append(sent, r); return nil })
	if len(sent) != 1 || sent[0].Closed != 0 {
		t.Fatalf("want a closed=0 answer, got %+v", sent)
	}
}

// The sweeper was written and never started once, which is a leak no unit test
// covered: expiry is enforced at validate time, so the only symptom is a map
// that grows by one per file operation. Authorizing a grant must arm it.
func TestHandleAuthorizeDataPlaneStartsTheSweeper(t *testing.T) {
	sess := &Session{Grants: newGrantStore(), ServerCID: testServerCID(0x01)}
	if sess.grantSweeper.Load() {
		t.Fatalf("the sweeper claims to be running before anything authorized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := protocol.AuthorizeDataPlaneRequest{SlotId: 0x21, Grant: liveGrant(11)}
	handleAuthorizeDataPlane(ctx, slog.Default(), sess, nil, req,
		func(protocol.AuthorizeDataPlaneResponse) error { return nil })

	if !sess.grantSweeper.Load() {
		t.Fatalf("authorizing a grant did not start the sweeper")
	}

	// Idempotent: a second grant must not start a second ticker.
	req2 := protocol.AuthorizeDataPlaneRequest{SlotId: 0x22, Grant: liveGrant(12)}
	handleAuthorizeDataPlane(ctx, slog.Default(), sess, nil, req2,
		func(protocol.AuthorizeDataPlaneResponse) error { return nil })
	if !sess.grantSweeper.Load() {
		t.Fatalf("the guard was cleared by a second authorize")
	}
}
