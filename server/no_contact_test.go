package server

import (
	"fmt"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AgentSendRequest.no_contact reaches the retained entry, so reading the
// message adds no contact (reach spec D17). A self-send is used because it is
// the one send a single handler can both publish and read; its reply_to is the
// sender's own side channel, which it may declare without board_peer. The send
// without the bit is the control that the same path does add the route.
func TestAgentSendNoContactMakesNoContact(t *testing.T) {
	for _, tc := range []struct {
		noContact bool
		want      []string
	}{
		{noContact: false, want: []string{"side"}},
		{noContact: true, want: nil},
	} {
		t.Run(fmt.Sprintf("no_contact=%v", tc.noContact), func(t *testing.T) {
			h, conn, st, me := reachHandler(t, 0)
			if err := h.Board.Subscribe(st, agentboard.SelfTopic(me)); err != nil {
				t.Fatal(err)
			}
			side := agentboard.SelfTopic(me) + ".side"
			r := &protocol.AgentSendRequest{}
			r.SetTopic([]byte(agentboard.SelfTopic(me)))
			r.SetReplyToTopic([]byte(side))
			r.SetNoContact(tc.noContact)
			if resp := sendThroughHandler(t, h, conn, 7, r); resp.Kind != protocol.TaskControlKind_AgentSend || resp.AgentSend().Status != protocol.SendStatus_Ok {
				t.Fatalf("send answered %v", resp.Kind)
			}
			h.handleAgentInboxAdvance(conn, 8)
			var want []string
			for range tc.want {
				want = append(want, side)
			}
			if got := h.Board.Contacts(st); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("contacts after reading the message = %v, want %v", got, want)
			}
		})
	}
}
