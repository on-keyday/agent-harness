package cli

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestWriteCapsAuditLine(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_Spawn|protocol.Capability_ExecControl, protocol.ScopeBase_Subtree)
	b := auditTask(0xb2, protocol.TaskStatus_Running, protocol.Capability_ExecControl, protocol.ScopeBase_Global)
	b.Overrides = []protocol.ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_Global}}
	tasks := []protocol.TaskInfo{a, b}
	var buf bytes.Buffer
	if err := WriteCapsAudit(&buf, CapsAudit(tasks, false, false), 0, tasks); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var spawn, ctl string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "spawn "):
			spawn = l
		case strings.HasPrefix(l, "exec_control "):
			ctl = l
		}
	}
	if !strings.Contains(spawn, "強") || !strings.Contains(spawn, " 1 ") || !strings.Contains(spawn, "[scope 対象外]") {
		t.Errorf("spawn line = %q, want tier, count 1 and the scope-none mark", spawn)
	}
	if !strings.Contains(ctl, "a1000000") || !strings.Contains(ctl, "[subtree]") || !strings.Contains(ctl, "[global*]") {
		t.Errorf("exec_control line = %q, want both holders with [subtree] and the override [global*]", ctl)
	}
}

func TestWriteCapsAuditFocus(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_Spawn|protocol.Capability_FileRead, protocol.ScopeBase_Subtree)
	tasks := []protocol.TaskInfo{a}
	var buf bytes.Buffer
	if err := WriteCapsAudit(&buf, CapsAudit(tasks, false, true), protocol.Capability_Spawn, tasks); err != nil {
		t.Fatal(err)
	}
	want := "revoke: harness-cli caps set " + hex.EncodeToString(a.Id.Id[:]) + " --caps file_read"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("focus output:\n%s\nwant a line containing %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "file_read ") && strings.Contains(buf.String(), "\nfile_read") {
		t.Fatal("focus output lists other bits")
	}
}

// Review Focus 3.
func TestWriteCapsAuditEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCapsAudit(&buf, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no task holds a capability (use --all to include ended tasks)") {
		t.Fatalf("empty output = %q", buf.String())
	}
}

func TestWriteCapsAuditImpliedNote(t *testing.T) {
	v := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_ExecView, protocol.ScopeBase_Subtree)
	var buf bytes.Buffer
	_ = WriteCapsAudit(&buf, CapsAudit([]protocol.TaskInfo{v}, false, false), 0, []protocol.TaskInfo{v})
	if !strings.Contains(buf.String(), "(also: exec_cowrite, exec_control)") {
		t.Fatalf("exec_view line lacks the implication note:\n%s", buf.String())
	}
}

func TestWriteCapsAuditJSONLines(t *testing.T) {
	a := auditTask(0xa1, protocol.TaskStatus_Running, protocol.Capability_Spawn|protocol.Capability_FileRead, protocol.ScopeBase_Subtree)
	var buf bytes.Buffer
	if err := WriteCapsAuditJSON(&buf, CapsAudit([]protocol.TaskInfo{a}, false, false)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"cap":"spawn"`) || !strings.Contains(lines[0], `"scope_kind":"none"`) {
		t.Fatalf("json lines = %v", lines)
	}
}
