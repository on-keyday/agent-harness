package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// taskWithProfile builds the shape every live task has today: a resolved
// AgentProfile, and the skills_injected bit the server stamps at assign time.
func taskWithProfile(profile string, injected bool) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = 0x11
	t.Status = protocol.TaskStatus_Running
	t.Kind = protocol.TaskKind_Interactive
	t.SetRepoPath([]uint8("/repo"))
	t.SetAgentProfile([]uint8(profile))
	t.SetSkillsInjected(injected)
	return t
}

// The regression this field exists for: a task carrying its own AgentProfile
// took the arm that built "agent=<name>" by hand, so the +skills marker was
// dropped from every task row while surviving on the runner-lookup arm that
// nothing reaches any more.
func TestTaskLineCarriesSkillsMarkerWithAgentProfile(t *testing.T) {
	line := taskLine(taskWithProfile("claude", true), nil)
	if !strings.Contains(line, "agent=claude+skills") {
		t.Errorf("task row lost the +skills marker: %s", line)
	}
}

func TestTaskLineOmitsSkillsMarkerWhenNotInjected(t *testing.T) {
	line := taskLine(taskWithProfile("claude", false), nil)
	if !strings.Contains(line, "agent=claude") {
		t.Fatalf("task row lost the agent column: %s", line)
	}
	if strings.Contains(line, "+skills") {
		t.Errorf("task row invented a +skills marker: %s", line)
	}
}

// A confined caller receives ZERO runners (handleList gates them on
// info_global), so runnerByID is empty for exactly the readers the marker is
// for. Passing nil here is that caller, not an artificial case.
func TestTaskLineMarkerSurvivesWithoutRunners(t *testing.T) {
	line := taskLine(taskWithProfile("codex", true), map[string]protocol.RunnerInfo{})
	if !strings.Contains(line, "agent=codex+skills") {
		t.Errorf("marker needs the runners array, which a confined caller never gets: %s", line)
	}
}

// ls --json splits the marker out as its own bool rather than making a script
// re-parse "+skills" off the agent string — the same split runnerJSON already
// makes between Agents and SkillsInjected.
func TestListJSONCarriesSkillsInjectedPerTask(t *testing.T) {
	body := &protocol.ListResultBody{}
	body.SetTasks([]protocol.TaskInfo{taskWithProfile("claude", true)})
	var buf bytes.Buffer
	renderListJSON(body, &buf)

	var doc struct {
		Tasks []struct {
			Agent          string `json:"agent"`
			SkillsInjected bool   `json:"skills_injected"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, buf.String())
	}
	if len(doc.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(doc.Tasks))
	}
	if !doc.Tasks[0].SkillsInjected {
		t.Error("tasks[0].skills_injected = false, want true")
	}
	if doc.Tasks[0].Agent != "claude" {
		t.Errorf("tasks[0].agent = %q, want the bare profile name (no +skills suffix to re-parse)", doc.Tasks[0].Agent)
	}
}

// The key is NOT omitempty: false is a real answer, and an absent key would
// read as "the server does not report this" rather than "not declared".
func TestListJSONAlwaysEmitsSkillsInjectedKey(t *testing.T) {
	body := &protocol.ListResultBody{}
	body.SetTasks([]protocol.TaskInfo{taskWithProfile("bash", false)})
	var buf bytes.Buffer
	renderListJSON(body, &buf)
	if !strings.Contains(buf.String(), `"skills_injected":false`) {
		t.Errorf("false must still appear as a key: %s", buf.String())
	}
}

// session ls embeds the same taskJSON, so the field must show up there too
// without a second derivation.
func TestSessionLsCarriesSkillsInjected(t *testing.T) {
	body := &protocol.ListResultBody{}
	body.SetTasks([]protocol.TaskInfo{taskWithProfile("claude", true)})
	var buf bytes.Buffer
	renderSessionsJSON(body, &buf)
	if !strings.Contains(buf.String(), `"skills_injected":true`) {
		t.Errorf("session ls row dropped skills_injected: %s", buf.String())
	}
}
