package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func authorityRow(src protocol.AuthoritySource, actor byte, ageMs uint64, hidden bool) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.AuthoritySource = src
	if actor != 0 {
		for i := range t.AuthorityActor.Id {
			t.AuthorityActor.Id[i] = actor
		}
	}
	if src != protocol.AuthoritySource_Unknown {
		t.AuthorityAt = 1
	}
	t.AuthorityAgeMs = ageMs
	t.SetAuthorityActorHidden(hidden)
	return t
}

func TestAuthorityLabel(t *testing.T) {
	for _, c := range []struct {
		row  protocol.TaskInfo
		want string
	}{
		{authorityRow(protocol.AuthoritySource_Unknown, 0, 0, false), "unknown"},
		{authorityRow(protocol.AuthoritySource_Spawn, 0x3f, 2*3600*1000, false), "spawn@3f3f3f3f (2h ago)"},
		{authorityRow(protocol.AuthoritySource_OperatorCapsSet, 0, 5*60*1000, false), "operator_caps_set@operator (5m ago)"},
		{authorityRow(protocol.AuthoritySource_Cascade, 0, 60*1000, true), "cascade@hidden (1m ago)"},
		{authorityRow(protocol.AuthoritySource_Resume, 0, 30*1000, false), "resume@operator (30s ago)"},
		{authorityRow(protocol.AuthoritySource_Reparent, 0, 30*1000, false), "reparent@operator (30s ago)"},
	} {
		if got := AuthorityLabel(c.row); got != c.want {
			t.Errorf("AuthorityLabel = %q, want %q", got, c.want)
		}
	}
}

// A task written before provenance existed says so: zero is shown, not elided.
func TestListRowCarriesAuthority(t *testing.T) {
	legacy := authorityRow(protocol.AuthoritySource_Unknown, 0, 0, false)
	if line := taskLine(legacy, nil); !strings.Contains(line, "  auth=unknown") {
		t.Errorf("legacy row lacks auth=unknown: %s", line)
	}
	spawned := authorityRow(protocol.AuthoritySource_Spawn, 0x3f, 1000, false)
	if line := taskLine(spawned, nil); !strings.Contains(line, "  auth=spawn@3f3f3f3f (1s ago)") {
		t.Errorf("spawned row lacks its auth token: %s", line)
	}
	for _, row := range []protocol.TaskInfo{legacy, spawned} {
		raw, err := json.Marshal(newTaskJSON(&row, nil))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m) //nolint:errcheck
		for _, k := range []string{"authority_source", "authority_actor", "authority_actor_hidden", "authority_at", "authority_age_ms"} {
			if _, ok := m[k]; !ok {
				t.Errorf("ls --json row lacks %q: %s", k, raw)
			}
		}
	}
}
