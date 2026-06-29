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
	if err := WriteWhoAmI(&sb, resp, true); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	for _, want := range []string{`"operator":false`, `"principal_task_id":"abab`, `"creator_task_id":""`, `"capabilities":"spawn"`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON missing %q in %q", want, got)
		}
	}
}
