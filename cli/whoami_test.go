package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mkTaskID(b byte) protocol.TaskID {
	var t protocol.TaskID
	for i := range t.Id {
		t.Id[i] = b
	}
	return t
}

func TestWriteWhoAmIOperator(t *testing.T) {
	var sb strings.Builder
	resp := protocol.WhoAmIResponse{Capabilities: protocol.Capability_All} // zero principal
	if err := WriteWhoAmI(&sb, resp, false); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.HasPrefix(got, "operator") {
		t.Errorf("operator line should start with 'operator', got %q", got)
	}
	if !strings.Contains(got, "caps=all") {
		t.Errorf("want caps=all, got %q", got)
	}
}

func TestWriteWhoAmIConfined(t *testing.T) {
	var sb strings.Builder
	resp := protocol.WhoAmIResponse{
		PrincipalTaskId: mkTaskID(0xab),
		CreatorTaskId:   mkTaskID(0xcd),
		Capabilities:    protocol.Capability_Spawn | protocol.Capability_FileRead,
	}
	if err := WriteWhoAmI(&sb, resp, false); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.Contains(got, "task=abababab") {
		t.Errorf("want full task hex, got %q", got)
	}
	if !strings.Contains(got, "by=cdcdcdcd") {
		t.Errorf("want creator short hex by=cdcdcdcd, got %q", got)
	}
	if !strings.Contains(got, "caps=spawn,file_read") {
		t.Errorf("want caps=spawn,file_read, got %q", got)
	}
}

func TestWriteWhoAmIJSON(t *testing.T) {
	var sb strings.Builder
	resp := protocol.WhoAmIResponse{
		PrincipalTaskId: mkTaskID(0xab),
		Capabilities:    protocol.Capability_Spawn,
	}
	resp.SetServerRevision([]byte("0f64b546cafe"))
	if err := WriteWhoAmI(&sb, resp, true); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	for _, want := range []string{`"operator":false`, `"principal_task_id":"abab`, `"creator_task_id":""`,
		`"capabilities":"spawn"`, `"server_revision":"0f64b546cafe"`, `"server_dirty":false`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON missing %q in %q", want, got)
		}
	}
}

// The server's build is reported on both forms, and an ABSENT revision is
// reported as an explicit unknown rather than omitted.
//
// This is the whole reason the field exists: the fleet's deploy rule is
// "restart the server first", and until now nothing could be asked which
// commit the server is on. A renderer that drops the field when it is empty
// makes a server built without VCS stamping indistinguishable from a server
// too old to send one — and those two want opposite actions.
func TestWriteWhoAmIReportsTheServerBuild(t *testing.T) {
	for _, tc := range []struct {
		name string
		rev  string
		dirt bool
		want string
	}{
		{"clean", "ad55e3f52700aaad9b8b441657df90dce7cb6614", false, "server=ad55e3f52700aaad9b8b441657df90dce7cb6614"},
		{"dirty", "ad55e3f52700aaad9b8b441657df90dce7cb6614", true, "DIRTY"},
		{"unstamped", "", false, "server=unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, operator := range []bool{true, false} {
				var sb strings.Builder
				resp := protocol.WhoAmIResponse{Capabilities: protocol.Capability_Spawn}
				if !operator {
					resp.PrincipalTaskId = mkTaskID(0xab)
				}
				resp.SetServerRevision([]byte(tc.rev))
				resp.SetServerDirty(tc.dirt)
				if err := WriteWhoAmI(&sb, resp, false); err != nil {
					t.Fatal(err)
				}
				// Both principal shapes: the operator line and the confined
				// line are separate format strings, which is exactly how one of
				// them ends up without the field.
				if got := sb.String(); !strings.Contains(got, tc.want) {
					t.Errorf("operator=%v: want %q in %q", operator, tc.want, got)
				}
			}
		})
	}
}
