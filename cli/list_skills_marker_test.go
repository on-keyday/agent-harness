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

// --- observer counts (viewers / cowriters) ---

func taskWithObservers(status protocol.TaskStatus, viewers, cowriters uint16) protocol.TaskInfo {
	t := taskWithProfile("claude", true)
	t.Status = status
	t.Viewers = viewers
	t.Cowriters = cowriters
	return t
}

// The motivating case: a Detached task being watched through the WebUI preview.
// The status says "no control attached"; only these counts say anyone is there.
func TestTaskLineShowsObserversOnDetached(t *testing.T) {
	line := taskLine(taskWithObservers(protocol.TaskStatus_Detached, 2, 1), nil)
	if !strings.Contains(line, "cowrite=1 viewer=2") {
		t.Errorf("Detached row hides its observers, which is what makes it read as abandoned: %s", line)
	}
}

// Elided when nobody is attached — the status column already says whether a
// session exists, so a Running row with no pair means "nobody", not "unknown".
func TestTaskLineElidesZeroObservers(t *testing.T) {
	line := taskLine(taskWithObservers(protocol.TaskStatus_Running, 0, 0), nil)
	if strings.Contains(line, "viewer=") || strings.Contains(line, "cowrite=") {
		t.Errorf("row spends width on an all-zero pair: %s", line)
	}
}

// JSON carries both unconditionally: 0 is a real answer there, and an absent
// key would read as "not reported".
func TestListJSONAlwaysCarriesObserverCounts(t *testing.T) {
	body := &protocol.ListResultBody{}
	body.SetTasks([]protocol.TaskInfo{taskWithObservers(protocol.TaskStatus_Running, 0, 0)})
	var buf bytes.Buffer
	renderListJSON(body, &buf)
	out := buf.String()
	if !strings.Contains(out, `"viewers":0`) || !strings.Contains(out, `"cowriters":0`) {
		t.Errorf("zero counts must still appear as keys: %s", out)
	}
}

func TestSessionLsCarriesObserverCounts(t *testing.T) {
	body := &protocol.ListResultBody{}
	body.SetTasks([]protocol.TaskInfo{taskWithObservers(protocol.TaskStatus_Detached, 3, 2)})
	var buf bytes.Buffer
	renderSessionsJSON(body, &buf)
	out := buf.String()
	if !strings.Contains(out, `"viewers":3`) || !strings.Contains(out, `"cowriters":2`) {
		t.Errorf("session ls row dropped the observer counts: %s", out)
	}
}
