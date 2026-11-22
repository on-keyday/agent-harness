package authority

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// MaySatisfy is the tool list's rule: show a tool when SOME request of a kind
// it sends could pass with the bits held. Its errors must all be on the shown
// side -- a tool hidden that the caller could use fails silently.
func TestMaySatisfy(t *testing.T) {
	rows := []struct {
		name string
		kind protocol.TaskControlKind
		held protocol.Capability
		want bool
	}{
		{"agent send needs only reach", protocol.TaskControlKind_AgentSend, 0, true},
		{"list files with file_write alone", protocol.TaskControlKind_ListFiles, cWrite, true},
		{"file transfer: a pull with file_read", protocol.TaskControlKind_OpenFileTransfer, cRead, true},
		{"file transfer with nothing", protocol.TaskControlKind_OpenFileTransfer, 0, false},
		{"cancel with nothing", protocol.TaskControlKind_Cancel, 0, false},
		{"cancel with cancel", protocol.TaskControlKind_Cancel, protocol.Capability_Cancel, true},
		{"list needs nothing", protocol.TaskControlKind_List, 0, true},
		{"a detached spawn with spawn", protocol.TaskControlKind_OpenInteractive, cSpawn, true},
		{"an open with nothing", protocol.TaskControlKind_OpenInteractive, 0, false},
		{"set caps is identity, not a bit", protocol.TaskControlKind_SetCaps, 0, true},
		{"restore: listing needs nothing", protocol.TaskControlKind_RestoreTasks, 0, true},
		{"attach view with exec_control", protocol.TaskControlKind_AttachSession, cControl, true},
		{"record keep with record_delete", protocol.TaskControlKind_RecordKeep, cDelete, true},
	}
	for _, r := range rows {
		if got := MaySatisfy(r.kind, r.held); got != r.want {
			t.Errorf("%s: MaySatisfy(%v, %v) = %t, want %t", r.name, r.kind, r.held, got, r.want)
		}
	}
}
