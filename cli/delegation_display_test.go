package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func boundTask() protocol.TaskInfo {
	t := protocol.TaskInfo{Status: protocol.TaskStatus_Running, Capabilities: protocol.Capability_Cancel | protocol.Capability_FileRead}
	t.Id.Id[0] = 0x3f
	return t
}

// Zero is a measurement: the default bound is printed, never elided.
func TestLsRowShowsDefaultBound(t *testing.T) {
	row := taskLine(boundTask(), nil)
	if !strings.Contains(row, "  delegate=none delegate-scope=subtree") {
		t.Errorf("row = %q, want the default bound printed", row)
	}
}

func TestLsJSONCarriesBound(t *testing.T) {
	task := boundTask()
	task.DelegateCaps = protocol.Capability_FileRead
	task.DelegateScope = protocol.TaskScope{Base: protocol.ScopeBase_None}
	raw, err := json.Marshal(newTaskJSON(&task, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["delegate_caps"] != "file_read" || got["delegate_scope"] != "none" {
		t.Errorf("json = %s, want delegate_caps=file_read delegate_scope=none", raw)
	}
}

func TestWhoamiOperatorBound(t *testing.T) {
	resp := protocol.WhoAmIResponse{Capabilities: protocol.Capability_All, DelegateCaps: protocol.Capability_All,
		DelegateScope: protocol.TaskScope{Base: protocol.ScopeBase_Global}}
	var out bytes.Buffer
	if err := WriteWhoAmI(&out, resp, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "delegate=all delegate-scope=global") {
		t.Errorf("operator whoami = %q", out.String())
	}
	out.Reset()
	if err := WriteWhoAmI(&out, resp, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"delegate_caps":"all"`) || !strings.Contains(out.String(), `"delegate_scope":"global"`) {
		t.Errorf("operator whoami --json = %q", out.String())
	}
}

func TestCapsCatalogHasDelegation(t *testing.T) {
	var out bytes.Buffer
	if err := verb.WriteCaps(&out, false); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(out.String(), "DELEGATION")
	if i < 0 {
		t.Fatalf("catalog has no DELEGATION section:\n%s", out.String())
	}
	for _, want := range []string{"--delegate-caps", "--delegate-scope", "caps set", "whoami"} {
		if !strings.Contains(out.String()[i:], want) {
			t.Errorf("DELEGATION section lacks %q", want)
		}
	}
}

// "Who can spread X": a holder is marked only when it holds the bit AND may
// delegate it.
func TestAuditMarksDelegators(t *testing.T) {
	spreads := boundTask()
	spreads.DelegateCaps = protocol.Capability_Cancel
	holds := boundTask()
	holds.Id.Id[0] = 0x40
	rows := CapsAudit([]protocol.TaskInfo{spreads, holds}, false, false)
	var cancelRow *AuditRow
	for i := range rows {
		if rows[i].Cap == protocol.Capability_Cancel {
			cancelRow = &rows[i]
		}
	}
	if cancelRow == nil || len(cancelRow.Holders) != 2 {
		t.Fatalf("cancel row = %+v", cancelRow)
	}
	for _, h := range cancelRow.Holders {
		want := h.TaskID == "3f000000000000000000000000000000"
		if h.Delegates != want {
			t.Errorf("%s: Delegates = %v, want %v", h.TaskID[:2], h.Delegates, want)
		}
		if strings.Contains(auditHolderText(h), " deleg") != want {
			t.Errorf("%s: line %q, deleg mark want %v", h.TaskID[:2], auditHolderText(h), want)
		}
	}
}
