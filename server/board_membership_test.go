package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func membership(t *testing.T, h *TaskHandler, conn *fakeConn, task protocol.TaskID, topic string, op protocol.BoardMembershipOp) protocol.TaskControlResponse {
	t.Helper()
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardMembership, RequestId: 3}
	r := protocol.BoardMembershipRequest{Task: task, Op: op}
	r.SetTopic([]byte(topic))
	req.SetBoardMembership(r)
	h.Handle(conn, encodeTaskControlRequest(t, req))
	return lastTaskControlResponse(t, conn)
}

func membershipStatus(t *testing.T, resp protocol.TaskControlResponse) protocol.BoardMembershipStatus {
	t.Helper()
	got := resp.BoardMembership()
	if got == nil {
		t.Fatalf("response kind %v, want board_membership", resp.Kind)
	}
	return got.Status
}

func TestBoardMembership_Operator(t *testing.T) {
	h, conn, _ := retainHandler(t, nil)
	none := protocol.Capability_None
	target := hexToTaskID(t, h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: none, Scope: authority.Scope{}}, "bash"))
	st := h.Board.Attach(protocol.RunnerID{}, target, "h", "bash")

	for _, c := range []struct {
		op   protocol.BoardMembershipOp
		want bool // subscribed afterwards
	}{
		{protocol.BoardMembershipOp_Subscribe, true},
		{protocol.BoardMembershipOp_Unsubscribe, false},
		{protocol.BoardMembershipOp_Join, true},
		{protocol.BoardMembershipOp_Leave, false},
	} {
		if s := membershipStatus(t, membership(t, h, conn, target, "grp", c.op)); s != protocol.BoardMembershipStatus_Ok {
			t.Fatalf("%v: %v, want ok", c.op, s)
		}
		if got := h.Board.Subscribes(st, "grp"); got != c.want {
			t.Errorf("after %v: subscribed %v, want %v", c.op, got, c.want)
		}
		if c.op == protocol.BoardMembershipOp_Join && !h.Board.Publishable(st, "grp") {
			t.Error("join did not let the task publish to grp")
		}
	}
	var unknown protocol.TaskID
	unknown.Id[0] = 0xEE
	if s := membershipStatus(t, membership(t, h, conn, unknown, "grp", protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_NotFound {
		t.Errorf("join of a task the server does not know: %v, want not_found", s)
	}
	if s := membershipStatus(t, membership(t, h, conn, target, "chat.0123abcd", protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_NotJoinable {
		t.Errorf("join of a chat topic: %v, want not_joinable", s)
	}

	// board subscribers marks the membership.
	if s := membershipStatus(t, membership(t, h, conn, target, "grp", protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_Ok {
		t.Fatal(s)
	}
	h.handleBoardSubscribers(conn, 1, "grp")
	resp := lastTaskControlResponse(t, conn)
	var body protocol.BoardSubscribersBody
	conn.streamBody(t, resp.BoardSubscribers().StreamId, &body)
	joined := false
	for _, row := range body.Rows {
		for _, p := range row.Patterns {
			if string(p.Name) == "grp" && row.Task == target {
				joined = p.Joined()
			}
		}
	}
	if !joined {
		t.Error("board subscribers does not mark the membership joined")
	}
}

func TestBoardMembership_TaskNeedsBit(t *testing.T) {
	none := protocol.Capability_None
	h, conn, self := retainHandler(t, &none)
	resp := membership(t, h, conn, self, "grp", protocol.BoardMembershipOp_Subscribe)
	if resp.Kind != protocol.TaskControlKind_PermissionDenied || resp.PermissionDenied().RequiredCap != protocol.Capability_BoardMember {
		t.Fatalf("response %v, want permission denied naming board_member", resp.Kind)
	}
}

// A task caller is bounded three ways: the target is in its board_member
// scope; join needs the caller itself to publish there; subscribe needs the
// target to read the topic already.
func TestBoardMembership_TaskBounds(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	h.Board = newTestBoard(t)
	h.Board.SetReach(storeReach{h.Tasks})
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9703-1")}
	h.principals = map[string]protocol.TaskID{conn.ConnectionID().String(): hexToTaskID(t, c)}
	gid, uid := hexToTaskID(t, g), hexToTaskID(t, u)
	h.Board.Attach(protocol.RunnerID{}, gid, "h", "bash")
	h.Board.Attach(protocol.RunnerID{}, uid, "h", "bash")

	if s := membershipStatus(t, membership(t, h, conn, uid, "grp", protocol.BoardMembershipOp_Subscribe)); s != protocol.BoardMembershipStatus_NotFound {
		t.Errorf("target outside the caller's scope: %v, want not_found", s)
	}
	if s := membershipStatus(t, membership(t, h, conn, gid, "grp", protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_NotPublishable {
		t.Errorf("join of a free-form topic by a caller without global board_peer: %v, want not_publishable", s)
	}
	own := "tree." + c + ".sns"
	if s := membershipStatus(t, membership(t, h, conn, gid, own, protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_Ok {
		t.Errorf("join of the caller's own tree topic for its descendant: %v, want ok", s)
	}
	if !h.Board.IsMember(gid, own) {
		t.Error("the ok join recorded no membership")
	}
	if s := membershipStatus(t, membership(t, h, conn, gid, agentboard.SelfTopic(uid), protocol.BoardMembershipOp_Subscribe)); s != protocol.BoardMembershipStatus_NotReadable {
		t.Errorf("subscribe of a descendant to another task's chat: %v, want not_readable", s)
	}
	if s := membershipStatus(t, membership(t, h, conn, gid, "grp", protocol.BoardMembershipOp_Subscribe)); s != protocol.BoardMembershipStatus_Ok {
		t.Errorf("subscribe of a descendant to a free-form topic: %v, want ok", s)
	}

	// A task never changes its own standing: a self-join would turn what it
	// may publish now (a contact, a board_peer that can be revoked) into a
	// membership that outlives it.
	cid := hexToTaskID(t, c)
	h.Board.Attach(protocol.RunnerID{}, cid, "h", "bash")
	if s := membershipStatus(t, membership(t, h, conn, cid, own, protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_NotFound {
		t.Errorf("self-join: %v, want not_found", s)
	}
	// A contact is no ground for a join: it answers someone who spoke first.
	cst := h.Board.Attach(protocol.RunnerID{}, cid, "h", "bash")
	var peer protocol.TaskID
	peer.Id[0] = 0x77
	h.Board.NoteDelivered(cst, []agentboard.RetainedMessage{{Topic: agentboard.SelfTopic(cid), FromTask: peer, ReplyToTopic: "talk"}})
	if !h.Board.PublishableBy(cid, "talk") {
		t.Fatal("setup: the contact did not open talk")
	}
	if s := membershipStatus(t, membership(t, h, conn, gid, "talk", protocol.BoardMembershipOp_Join)); s != protocol.BoardMembershipStatus_NotPublishable {
		t.Errorf("join on a contact's ground: %v, want not_publishable", s)
	}
}
