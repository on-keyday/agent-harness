package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func lifecycleTask(st protocol.LifecycleState, ageMs uint64, status protocol.TaskStatus) protocol.TaskInfo {
	t := protocol.TaskInfo{Status: status, Lifecycle: st, LifecycleAt: 5, LifecycleAgeMs: ageMs}
	return t
}

func TestLifecycleBadge(t *testing.T) {
	cases := []struct {
		name string
		t    protocol.TaskInfo
		want string
	}{
		{"waiting", lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Running), "wait:3m"},
		{"idle", lifecycleTask(protocol.LifecycleState_Idle, 720000, protocol.TaskStatus_Detached), "ready:12m"},
		{"error", lifecycleTask(protocol.LifecycleState_Error, 30000, protocol.TaskStatus_Running), "err:30s"},
		{"working", lifecycleTask(protocol.LifecycleState_Working, 30000, protocol.TaskStatus_Running), ""},
		{"terminal", lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Succeeded), ""},
		{"never written", protocol.TaskInfo{Status: protocol.TaskStatus_Running, Lifecycle: protocol.LifecycleState_Waiting}, ""},
	}
	for _, c := range cases {
		if got := LifecycleBadge(&c.t); got != c.want {
			t.Errorf("%s: LifecycleBadge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTaskLine_Lifecycle(t *testing.T) {
	ti := lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Running)
	if line := taskLine(ti, nil); !strings.Contains(line, "lifecycle=waiting (3m ago)") {
		t.Fatalf("taskLine = %q, want lifecycle=waiting (3m ago)", line)
	}
	ti.LifecycleAt = 0
	if line := taskLine(ti, nil); strings.Contains(line, "lifecycle=") {
		t.Fatalf("taskLine = %q, want no lifecycle= for a never-written field", line)
	}
}

func TestTaskJSON_LifecycleFields(t *testing.T) {
	b, err := json.Marshal(newTaskJSON(&protocol.TaskInfo{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lifecycle", "lifecycle_at", "lifecycle_age_ms", "program_state", "program_kind", "program_msg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ls --json lacks %q at zero; present-at-zero is the contract", k)
		}
	}
	if m["lifecycle"] != "" {
		t.Errorf("unset lifecycle = %v, want \"\"", m["lifecycle"])
	}
}

func TestLifecycleDedup(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const task = "00112233445566778899aabbccddeeff"
	skip, record := lifecycleDedup(task, protocol.LifecycleState_Working)
	if skip {
		t.Fatal("first report skipped")
	}
	record()
	if skip, _ := lifecycleDedup(task, protocol.LifecycleState_Working); !skip {
		t.Fatal("a repeat of the last sent state was not skipped")
	}
	if skip, _ := lifecycleDedup(task, protocol.LifecycleState_Idle); skip {
		t.Fatal("a different state was skipped")
	}
	if skip, _ := lifecycleDedup("", protocol.LifecycleState_Working); skip {
		t.Fatal("an empty task id must never skip")
	}

	// A cache dir that cannot be written costs a redundant send, never a skip.
	ro := t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "harness"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", ro)
	skip, record = lifecycleDedup(task, protocol.LifecycleState_Working)
	record()
	if skip, _ := lifecycleDedup(task, protocol.LifecycleState_Working); skip {
		t.Fatal("an unusable cache dir skipped a report")
	}
	_ = skip
}
