package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTaskState_Identity(t *testing.T) {
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	rid.Port = 9001
	rid.UniqueNumber = 7
	var tid protocol.TaskID
	tid.Id[0] = 0xCC
	ts := newTaskState()
	ts.setIdentity(rid, tid, "host-x")

	gotRid, gotTid, gotHost := ts.identity()
	if gotHost != "host-x" {
		t.Errorf("hostname = %q", gotHost)
	}
	if gotTid.Id != tid.Id {
		t.Errorf("task = %v", gotTid.Id)
	}
	if gotRid.UniqueNumber != 7 {
		t.Errorf("rid.UniqueNumber = %d", gotRid.UniqueNumber)
	}
}
