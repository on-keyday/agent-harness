package protocol

import (
	"bytes"
	"testing"
)

func TestDataPlaneGrantRoundTripFileTransfer(t *testing.T) {
	g := DataPlaneGrant{
		GrantId:       [16]uint8{1, 2, 3},
		TaskId:        TaskID{Id: [16]uint8{9, 9}},
		ExpiresUnixMs: 1_700_000_000_000,
		Kind:          TaskControlKind_OpenFileTransfer,
	}
	// The variant is a getter/setter pair, not a field: the setter refuses
	// unless kind selects the arm.
	if !g.SetDirection(FileTransferDirection_Pull) {
		t.Fatalf("SetDirection refused on kind=open_file_transfer")
	}
	b, err := g.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back DataPlaneGrant
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Kind != TaskControlKind_OpenFileTransfer {
		t.Fatalf("kind lost: %v", back.Kind)
	}
	if d := back.Direction(); d == nil || *d != FileTransferDirection_Pull {
		t.Fatalf("direction lost: %v", d)
	}
	if back.ExpiresUnixMs != g.ExpiresUnixMs || back.GrantId != g.GrantId {
		t.Fatalf("fixed fields lost")
	}
	if back.TaskId.Id != g.TaskId.Id {
		t.Fatalf("task id lost")
	}
}

// A kind with no arm must encode shorter, and every field BEFORE the variant
// must sit at the same offset either way. That is the whole reason the variant
// is the tail: adding an arm later must not move grant_id, task_id or
// expires_unix_ms.
func TestDataPlaneGrantVariantIsTail(t *testing.T) {
	base := DataPlaneGrant{ExpiresUnixMs: 7, Kind: TaskControlKind_GitQuery}
	withArm := DataPlaneGrant{ExpiresUnixMs: 7, Kind: TaskControlKind_OpenFileTransfer}
	withArm.SetDirection(FileTransferDirection_Pull)
	// The armless kind must refuse the setter, which is the same fact the
	// length comparison below measures from the other side.
	if base.SetDirection(FileTransferDirection_Pull) {
		t.Fatalf("SetDirection accepted on a kind with no arm")
	}
	a, err := base.Append(nil)
	if err != nil {
		t.Fatalf("encode git_query: %v", err)
	}
	b, err := withArm.Append(nil)
	if err != nil {
		t.Fatalf("encode open_file_transfer: %v", err)
	}
	if len(a) >= len(b) {
		t.Fatalf("expected the armless kind to be shorter: %d vs %d", len(a), len(b))
	}
	// Identical up to the kind byte, which is the last byte of the shorter one.
	if !bytes.Equal(a[:len(a)-1], b[:len(a)-1]) {
		t.Fatalf("fields before the variant moved:\n a=%v\n b=%v", a, b)
	}
}

// transport_len == 0 is how "do not punch" is spelled — the same encoding
// DialRunnerRequest.via uses for "not specified", and the only value v1's
// server ever writes.
func TestAuthorizeDataPlaneAbsentPunchTarget(t *testing.T) {
	req := AuthorizeDataPlaneRequest{SlotId: 0x4242}
	req.Grant.Kind = TaskControlKind_ListFiles
	b, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back AuthorizeDataPlaneRequest
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.PunchTarget.TransportLen != 0 {
		t.Fatalf("absent punch target did not round-trip: %d", back.PunchTarget.TransportLen)
	}
	if back.SlotId != 0x4242 {
		t.Fatalf("slot lost: %d", back.SlotId)
	}
	if back.Grant.Kind != TaskControlKind_ListFiles {
		t.Fatalf("embedded grant lost: %v", back.Grant.Kind)
	}
}

func TestDataPlaneInfoRoundTrip(t *testing.T) {
	in := DataPlaneInfo{GrantId: [16]uint8{4, 5}, TaskId: TaskID{Id: [16]uint8{6}}}
	b, err := in.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back DataPlaneInfo
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.GrantId != in.GrantId || back.TaskId.Id != in.TaskId.Id {
		t.Fatalf("round-trip lost a field")
	}
}

func TestRevokeDataPlaneRoundTrip(t *testing.T) {
	req := RevokeDataPlaneRequest{GrantId: [16]uint8{8}}
	b, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode req: %v", err)
	}
	var backReq RevokeDataPlaneRequest
	if _, err := backReq.Decode(b); err != nil {
		t.Fatalf("decode req: %v", err)
	}
	if backReq.GrantId != req.GrantId {
		t.Fatalf("grant id lost")
	}

	resp := RevokeDataPlaneResponse{Closed: 3}
	rb, err := resp.Append(nil)
	if err != nil {
		t.Fatalf("encode resp: %v", err)
	}
	var backResp RevokeDataPlaneResponse
	if _, err := backResp.Decode(rb); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if backResp.Closed != 3 {
		t.Fatalf("closed count lost: %d", backResp.Closed)
	}
}
