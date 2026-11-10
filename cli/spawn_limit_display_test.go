package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The unbounded case is printed too: "no limit" is what an operator scans a
// supervisor's row for.
func TestLsRowShowsUnboundedLimits(t *testing.T) {
	row := taskLine(boundTask(), nil)
	if !strings.Contains(row, "  spawn-depth=unbounded max-descendants=unbounded descendants=0") {
		t.Errorf("row = %q, want the limits printed", row)
	}
	task := boundTask()
	task.SpawnDepth, task.MaxDescendants, task.Descendants = limitOf(2), limitOf(5), 3
	if row := taskLine(task, nil); !strings.Contains(row, "  spawn-depth=2 max-descendants=5 descendants=3") {
		t.Errorf("row = %q, want 2 / 5 / 3", row)
	}
}

func TestLsJSONLimitsNullWhenUnbounded(t *testing.T) {
	for _, c := range []struct {
		depth, max protocol.SpawnLimit
		want       string
	}{
		{protocol.SpawnLimit{}, protocol.SpawnLimit{}, `"spawn_depth":null,"max_descendants":null,"descendants":4`},
		{limitOf(0), limitOf(7), `"spawn_depth":0,"max_descendants":7,"descendants":4`},
	} {
		task := boundTask()
		task.SpawnDepth, task.MaxDescendants, task.Descendants = c.depth, c.max, 4
		raw, err := json.Marshal(newTaskJSON(&task, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), c.want) {
			t.Errorf("json = %s, want %s", raw, c.want)
		}
	}
}

func TestWhoamiShowsRoom(t *testing.T) {
	resp := protocol.WhoAmIResponse{SpawnDepth: limitOf(2), MaxDescendants: limitOf(5), Descendants: 3, SpawnRoom: limitOf(2)}
	resp.PrincipalTaskId.Id[0] = 1
	var out bytes.Buffer
	if err := WriteWhoAmI(&out, resp, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "spawn-depth=2 max-descendants=5 descendants=3 room=2") {
		t.Errorf("whoami = %q", out.String())
	}
	out.Reset()
	if err := WriteWhoAmI(&out, protocol.WhoAmIResponse{}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "spawn-depth=unbounded max-descendants=unbounded descendants=0 room=unbounded") {
		t.Errorf("operator whoami = %q", out.String())
	}
	out.Reset()
	if err := WriteWhoAmI(&out, resp, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"spawn_depth":2,"max_descendants":5,"descendants":3,"spawn_room":2`) {
		t.Errorf("whoami --json = %q", out.String())
	}
}

func TestCapsCatalogHasSpawnLimits(t *testing.T) {
	var out bytes.Buffer
	if err := verb.WriteCaps(&out, false); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	i := strings.Index(s, "SPAWN LIMITS")
	if i < 0 {
		t.Fatalf("catalog has no SPAWN LIMITS section:\n%s", s)
	}
	// Compared as words: the prose is wrapped, and where it breaks is not
	// the claim.
	section := strings.Join(strings.Fields(s[i:]), " ")
	for _, want := range []string{"--spawn-depth", "--max-descendants", "resum",
		"Grant only what the work needs: an agent given spawn with limits is bounded only while it lacks prune over its subtree."} {
		if !strings.Contains(section, want) {
			t.Errorf("SPAWN LIMITS section lacks %q", want)
		}
	}
	// Wrapped like the DELEGATION prose above it.
	for _, line := range strings.Split(s[i:], "\n") {
		if len(line) > 76 {
			t.Errorf("SPAWN LIMITS line is %d bytes, wider than the rest of the catalog prose: %q", len(line), line)
		}
	}
	if !strings.Contains(s, "over a limited subtree it frees places, and pruning an ended task in the middle lifts the limits for the tasks below it") {
		t.Error("the prune entry does not say what it does to spawn limits")
	}
}

// what-if predicts the child's spawn depth as the server writes it.
func TestWhatIfPredictsSpawnDepth(t *testing.T) {
	p := boundTask()
	p.Capabilities |= protocol.Capability_Spawn
	p.SpawnDepth = limitOf(3)
	r, err := BuildWhatIf([]protocol.TaskInfo{p}, authority.Spawn{Creator: AuthorityTasks([]protocol.TaskInfo{p})[0].ID,
		SpawnDepth: limitOf(5), MaxDescendants: limitOf(4)})
	if err != nil {
		t.Fatal(err)
	}
	var head string
	for _, d := range r.Diffs {
		if d.New {
			head = WhatIfHead(d)
		}
	}
	if !strings.Contains(head, "spawn-depth=2") || !strings.Contains(head, "max-descendants=4") {
		t.Errorf("new child's head = %q, want spawn-depth=2 max-descendants=4", head)
	}
	if !strings.Contains(r.Command, "--spawn-depth 5") || !strings.Contains(r.Command, "--max-descendants 4") {
		t.Errorf("command = %q, want the limits named", r.Command)
	}
}
