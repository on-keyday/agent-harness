package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func progressRow(progress string, at, ageMs uint64, live bool, title string) protocol.TaskInfo {
	var ti protocol.TaskInfo
	ti.Id.Id[0] = 0xab
	ti.Status = protocol.TaskStatus_Running
	ti.SetProgress([]byte(progress))
	ti.ProgressAt = at
	ti.ProgressAgeMs = ageMs
	ti.SetLiveTerminal(live)
	ti.SetTitle([]byte(title))
	return ti
}

func TestTaskLine_ProgressAndTitle(t *testing.T) {
	line := taskLine(progressRow("stage 2", 1, 720000, true, ""), nil)
	if !strings.Contains(line, `progress="stage 2" (12m ago)`) {
		t.Errorf("line lacks the progress column: %s", line)
	}
	if !strings.Contains(line, `title=""`) {
		t.Errorf("a live terminal with no title must print title=\"\" (a measurement, not a gap): %s", line)
	}
	bare := taskLine(progressRow("", 0, 0, false, ""), nil)
	if strings.Contains(bare, "progress=") || strings.Contains(bare, "title=") {
		t.Errorf("never-written progress / no terminal must print neither column: %s", bare)
	}
	cleared := taskLine(progressRow("", 5, 3000, false, ""), nil)
	if !strings.Contains(cleared, `progress="" (3s ago)`) {
		t.Errorf("a cleared line is a write and prints with its age: %s", cleared)
	}
}

func TestTaskJSON_ProgressFields(t *testing.T) {
	ti := progressRow("", 0, 0, false, "")
	b, err := json.Marshal(newTaskJSON(&ti, nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"progress", "progress_at", "progress_age_ms", "live_terminal", "title"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ls --json row lacks %q even at zero: %s", k, b)
		}
	}
}

func TestFilterWatchers(t *testing.T) {
	ws := []protocol.AwaitIdleWatcherInfo{
		{WatcherId: 1, Condition: protocol.AwaitCondition_Idle},
		{WatcherId: 2, Condition: protocol.AwaitCondition_Progress},
		{WatcherId: 3, Condition: protocol.AwaitCondition_Idle},
	}
	got := FilterWatchers(ws, protocol.AwaitCondition_Idle)
	if len(got) != 2 || got[0].WatcherId != 1 || got[1].WatcherId != 3 {
		t.Errorf("idle filter = %+v", got)
	}
	if got := FilterWatchers(ws, protocol.AwaitCondition_Progress); len(got) != 1 || got[0].WatcherId != 2 {
		t.Errorf("progress filter = %+v", got)
	}
}
