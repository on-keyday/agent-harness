package agentboard

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTopic_AppendInRing(t *testing.T) {
	topic := newTopic("conv/x/messages", 4)
	var zeroRid protocol.RunnerID
	var zeroTid protocol.TaskID
	for i := 0; i < 6; i++ {
		topic.append(uint64(i+1), []byte{byte(i)}, zeroRid, zeroTid, "")
	}
	got := topic.since(0)
	if len(got) != 4 {
		t.Fatalf("ring should hold last 4 only, got %d", len(got))
	}
	if got[0].Seq != 3 || got[3].Seq != 6 {
		t.Errorf("ring oldest/newest seq = %d/%d, want 3/6", got[0].Seq, got[3].Seq)
	}
}

func TestTopic_SinceFiltersByCursor(t *testing.T) {
	topic := newTopic("topic/foo", 8)
	var zeroRid protocol.RunnerID
	var zeroTid protocol.TaskID
	for i := uint64(1); i <= 5; i++ {
		topic.append(i, []byte{byte(i)}, zeroRid, zeroTid, "")
	}
	got := topic.since(2)
	if len(got) != 3 {
		t.Fatalf("since=2 should yield seq 3,4,5, got len=%d", len(got))
	}
	if got[0].Seq != 3 {
		t.Errorf("first seq = %d, want 3", got[0].Seq)
	}
}

func TestTopic_LastPublishedAtUpdates(t *testing.T) {
	topic := newTopic("status/x/y", 4)
	var zeroRid protocol.RunnerID
	var zeroTid protocol.TaskID
	t0 := time.Now()
	topic.append(1, []byte("a"), zeroRid, zeroTid, "")
	if topic.lastPublishedAt.Before(t0) {
		t.Error("lastPublishedAt did not update after append")
	}
}

func TestTopic_AppendCarriesSender(t *testing.T) {
	tp := newTopic("chat/x", 4)
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	rid.Port = 9000
	rid.UniqueNumber = 1
	var tid protocol.TaskID
	tid.Id[0] = 0x42

	tp.append(1, []byte("hi"), rid, tid, "host-A")

	got := tp.since(0)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].FromHostname != "host-A" {
		t.Errorf("FromHostname = %q, want %q", got[0].FromHostname, "host-A")
	}
	if got[0].FromTask.Id != tid.Id {
		t.Errorf("FromTask.Id = %v, want %v", got[0].FromTask.Id, tid.Id)
	}
	if string(got[0].FromRunner.Transport) != "ws" {
		t.Errorf("FromRunner.Transport = %q, want %q", string(got[0].FromRunner.Transport), "ws")
	}
}
