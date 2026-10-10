package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestExecRunTrafficLinePrintsZeros(t *testing.T) {
	e := &protocol.ExecRunInfo{ExecId: 4}
	if got := ExecRunTrafficLine(e); got != "io=pipe  stdin=0  stdout=0  stderr=0  last=never  taps=0" {
		t.Fatalf("idle exec: %q", got)
	}
	e.StdinBytes, e.StdoutBytes, e.Taps = 1200, 3<<20, 1
	e.LastActivityUnixMs = uint64(time.Now().Add(-2 * time.Second).UnixMilli())
	got := ExecRunTrafficLine(e)
	for _, want := range []string{"stdin=1.2kB", "stdout=3.0MB", "stderr=0", "last=2s ago", "taps=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
}

func TestExecRunInfoLinesCarryTheTrafficLine(t *testing.T) {
	lines := ExecRunInfoLines([]protocol.ExecRunInfo{{ExecId: 4}})
	if len(lines) != 3 || !strings.Contains(lines[2], "taps=0") {
		t.Fatalf("want header, row, traffic line: %q", lines)
	}
}

func TestExecRunInfoJSONCarriesTheCounters(t *testing.T) {
	var obj map[string]any
	_ = json.Unmarshal([]byte(ExecRunInfoJSONLine(&protocol.ExecRunInfo{ExecId: 4, StderrBytes: 0, Taps: 2})), &obj)
	for _, k := range []string{"stdin_bytes", "stdout_bytes", "stderr_bytes", "last_activity_unix_ms", "taps"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("JSON row lacks %q: %v", k, obj)
		}
	}
}

// The mode prints for BOTH values: a row that showed only `pty` would leave a
// pipe row silent about it, and silence reads as "not reported".
func TestExecRunTrafficLineNamesTheIOMode(t *testing.T) {
	var e protocol.ExecRunInfo
	if !strings.HasPrefix(ExecRunTrafficLine(&e), "io=pipe ") {
		t.Fatalf("pipe exec: %q", ExecRunTrafficLine(&e))
	}
	e.SetPty(true)
	if !strings.HasPrefix(ExecRunTrafficLine(&e), "io=pty ") {
		t.Fatalf("pty exec: %q", ExecRunTrafficLine(&e))
	}
}

func TestExecRunInfoJSONCarriesPty(t *testing.T) {
	var e protocol.ExecRunInfo
	e.SetPty(true)
	var obj map[string]any
	_ = json.Unmarshal([]byte(ExecRunInfoJSONLine(&e)), &obj)
	if obj["pty"] != true {
		t.Fatalf("JSON row pty = %v, want true: %v", obj["pty"], obj)
	}
	var p protocol.ExecRunInfo
	obj = nil
	_ = json.Unmarshal([]byte(ExecRunInfoJSONLine(&p)), &obj)
	if v, ok := obj["pty"]; !ok || v != false {
		t.Fatalf("a pipe row must carry pty=false, not omit it: %v", obj)
	}
}
