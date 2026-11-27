package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// mkRunnerInfo builds a minimal RunnerInfo for renderListJSON tests.
func mkRunnerInfo(host string, status protocol.RunnerStatus, max uint16, roots []string, profiles []string) protocol.RunnerInfo {
	r := protocol.RunnerInfo{Status: status, MaxTasks: max}
	r.Hostname = []uint8(host)
	r.HostnameLen = uint8(len(host))
	for _, p := range roots {
		var ar protocol.AllowedRoot
		ar.Path = []uint8(p)
		ar.PathLen = uint16(len(p))
		r.AllowedRoots = append(r.AllowedRoots, ar)
	}
	r.AllowedRootsLen = uint8(len(roots))
	r.AgentProfiles = profileNames(profiles...)
	r.AgentProfilesLen = uint8(len(profiles))
	return r
}

// TestRenderListJSON verifies the ls --json shape: a single object with
// "runners" and "tasks" arrays, structured agent fields, and creator id.
func TestRenderListJSON(t *testing.T) {
	lr := &protocol.ListResultBody{}
	r := mkRunnerInfo("box1", protocol.RunnerStatus_Idle, 4, []string{"/a", "/b"}, []string{"claude", "codex"})
	lr.Runners = []protocol.RunnerInfo{r}

	var task protocol.TaskInfo
	task.Id.Id = [16]byte{1, 2, 3}
	task.Status = protocol.TaskStatus_Running
	task.Kind = protocol.TaskKind_Oneshot
	task.OriginKind = protocol.ClientKind_Cli
	task.CreatorTaskId.Id = [16]byte{9, 9}
	task.Capabilities = protocol.Capability_Spawn
	task.RepoPath = []uint8("/repo")
	task.Prompt = []uint8("do it")
	task.ExitCode = 0
	lr.Tasks = []protocol.TaskInfo{task}

	var buf bytes.Buffer
	renderListJSON(lr, &buf)

	var got struct {
		Runners []map[string]any `json:"runners"`
		Tasks   []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not a single JSON object: %v\n%s", err, buf.String())
	}
	if len(got.Runners) != 1 || len(got.Tasks) != 1 {
		t.Fatalf("want 1 runner + 1 task, got %d + %d", len(got.Runners), len(got.Tasks))
	}

	rn := got.Runners[0]
	if rn["hostname"] != "box1" || rn["status"] != "idle" {
		t.Errorf("runner host/status: %v / %v", rn["hostname"], rn["status"])
	}
	agents, _ := rn["agents"].([]any)
	if len(agents) != 2 || agents[0] != "claude" || agents[1] != "codex" {
		t.Errorf("runner agents = %v, want [claude codex]", rn["agents"])
	}

	tk := got.Tasks[0]
	if tk["status"] != "running" || tk["kind"] != "oneshot" {
		t.Errorf("task status/kind: %v / %v", tk["status"], tk["kind"])
	}
	if tk["prompt"] != "do it" || tk["repo"] != "/repo" {
		t.Errorf("task prompt/repo: %v / %v", tk["prompt"], tk["repo"])
	}
	if tk["caps"] != "spawn" {
		t.Errorf("task caps = %v, want spawn", tk["caps"])
	}
	// created_by is the 32-hex creator task id.
	if cb, _ := tk["created_by"].(string); cb != "09090000000000000000000000000000" {
		t.Errorf("task created_by = %q", cb)
	}
}

// TestRenderSessionsJSON verifies session ls: only interactive tasks, each row
// carrying the shared ls --json task vocabulary PLUS is_attached/ring_buffer_bytes.
func TestRenderSessionsJSON(t *testing.T) {
	lr := &protocol.ListResultBody{}
	var cred protocol.TaskInfo
	cred.Id.Id = [16]byte{1}
	cred.Kind = protocol.TaskKind_Credential
	var sess protocol.TaskInfo
	sess.Id.Id = [16]byte{2}
	sess.Kind = protocol.TaskKind_Interactive
	sess.Status = protocol.TaskStatus_Running
	sess.CreatorTaskId.Id = [16]byte{7}
	sess.Capabilities = protocol.Capability_Spawn
	sess.RingBufferBytes = 4096
	lr.Tasks = []protocol.TaskInfo{cred, sess}

	var buf bytes.Buffer
	renderSessionsJSON(lr, &buf)

	// JSON Lines: the oneshot must be filtered out → exactly one line.
	dec := json.NewDecoder(&buf)
	var rows []map[string]any
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode session line: %v", err)
		}
		rows = append(rows, m)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 interactive row, got %d", len(rows))
	}
	row := rows[0]
	// Shared vocabulary must match ls --json exactly (lowercase status, kind,
	// caps label, created_by full hex).
	if row["status"] != "running" || row["kind"] != "interactive" {
		t.Errorf("shared status/kind: %v / %v", row["status"], row["kind"])
	}
	if row["caps"] != "spawn" {
		t.Errorf("shared caps = %v, want spawn", row["caps"])
	}
	if cb, _ := row["created_by"].(string); cb != "07000000000000000000000000000000" {
		t.Errorf("shared created_by = %q", cb)
	}
	// Session-only fields present.
	if row["is_attached"] != false {
		t.Errorf("is_attached = %v", row["is_attached"])
	}
	if rb, _ := row["ring_buffer_bytes"].(float64); rb != 4096 {
		t.Errorf("ring_buffer_bytes = %v, want 4096", row["ring_buffer_bytes"])
	}
}

// TestRenderListJSONEmpty verifies empty snapshots still emit valid JSON with
// non-null arrays (jq-friendly: `.runners | length` never errors).
func TestRenderListJSONEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderListJSON(&protocol.ListResultBody{}, &buf)
	var got struct {
		Runners []any `json:"runners"`
		Tasks   []any `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty output not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Runners == nil || got.Tasks == nil {
		t.Fatalf("empty arrays must be [] not null: %s", buf.String())
	}
}

// A background task's conditions ride ls --json, so a front-end can prefill a
// resume from its last run (A-D7, A-D15); every field present at zero.
func TestRenderListJSONRunConditions(t *testing.T) {
	var task protocol.TaskInfo
	task.Kind = protocol.TaskKind_Interactive
	task.RunMode = protocol.RunMode_Background
	task.Conditions = protocol.BackgroundConditions{DeadlineMs: 3600000, TurnEnd: protocol.TurnEndSignal_Quiet, QuietMs: 60000}
	var buf bytes.Buffer
	renderListJSON(&protocol.ListResultBody{Tasks: []protocol.TaskInfo{task}}, &buf)
	var got struct {
		Tasks []struct {
			RunMode    string         `json:"run_mode"`
			Conditions map[string]any `json:"conditions"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	c := got.Tasks[0].Conditions
	if got.Tasks[0].RunMode != "background" || c["deadline_ms"] != float64(3600000) || c["turn_end"] != "quiet" ||
		c["quiet_ms"] != float64(60000) || c["blocked_ms"] != float64(0) || c["queue_timeout_ms"] != float64(0) {
		t.Fatalf("row %+v", got.Tasks[0])
	}
}

// `oneshot` is always present: true for a `session new --oneshot` run, false
// for an ordinary session. The kind stays interactive for both (A-D29), so a
// script that filters on kind keeps working.
func TestRenderListJSONOneshot(t *testing.T) {
	one := protocol.TaskInfo{Kind: protocol.TaskKind_Oneshot}
	plain := protocol.TaskInfo{Kind: protocol.TaskKind_Interactive}
	var buf bytes.Buffer
	renderListJSON(&protocol.ListResultBody{Tasks: []protocol.TaskInfo{one, plain}}, &buf)
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wants := []struct {
		oneshot bool
		kind    string
	}{
		{true, "oneshot"},
		{false, "interactive"},
	}
	for i, want := range wants {
		tk := got.Tasks[i]
		if v, ok := tk["oneshot"]; !ok || v != want.oneshot {
			t.Errorf("task %d: oneshot=%v present=%v, want %v", i, v, ok, want.oneshot)
		}
		if tk["kind"] != want.kind {
			t.Errorf("task %d: kind=%v, want %s", i, tk["kind"], want.kind)
		}
	}
}

// A-D33: `pty` is always present, and says whether the run's child has a PTY:
// an interactive run unless it said --no-pty (or was a one-shot that did not
// say --pty); never a stream session.
func TestRenderListJSONPty(t *testing.T) {
	plain := protocol.TaskInfo{Kind: protocol.TaskKind_Interactive}
	pipe := protocol.TaskInfo{Kind: protocol.TaskKind_Interactive}
	pipe.SetNoPty(true)
	stream := protocol.TaskInfo{Kind: protocol.TaskKind_Stream}
	var buf bytes.Buffer
	renderListJSON(&protocol.ListResultBody{Tasks: []protocol.TaskInfo{plain, pipe, stream}}, &buf)
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, false} {
		if v, ok := got.Tasks[i]["pty"]; !ok || v != want {
			t.Errorf("task %d: pty=%v present=%v, want %v", i, v, ok, want)
		}
	}
}

// ls --json carries finished_omitted on every listing, 0 included, and the
// human forms end on the line that says the same.
func TestListCarriesFinishedOmitted(t *testing.T) {
	for _, n := range []uint32{0, 7} {
		lr := &protocol.ListResultBody{FinishedOmitted: n}
		var buf bytes.Buffer
		renderListJSON(lr, &buf)
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if v, ok := got["finished_omitted"]; !ok || v != float64(n) {
			t.Fatalf("finished_omitted = %v (present %v), want %d", v, ok, n)
		}
		buf.Reset()
		renderList(lr, &buf)
		want := "none older left out"
		if n > 0 {
			want = "7 older left out (ls --finished all)"
		}
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("ls rows for omitted=%d:\n%s\nwant a line containing %q", n, buf.String(), want)
		}
	}
}
