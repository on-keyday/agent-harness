package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func orderFixture() *protocol.ListResultBody {
	waiting := withLife(ot(2, protocol.TaskStatus_Running, 1, 1, 0), protocol.LifecycleState_Waiting)
	done := ot(1, protocol.TaskStatus_Succeeded, 2, 2, 3)
	done.RepoPath = []byte("/w/b")
	return &protocol.ListResultBody{Tasks: []protocol.TaskInfo{done, waiting}}
}

// `ls --order state` prints a header per non-empty group, in group order, with
// that group's rows under it.
func TestRenderListOrderedPrintsGroupHeaders(t *testing.T) {
	var out bytes.Buffer
	renderListOrdered(orderFixture(), &out, OrderState, orderNow)
	s := out.String()
	needs, fin := strings.Index(s, "== needs you (1) =="), strings.Index(s, "== finished (1) ==")
	if needs < 0 || fin < 0 || needs > fin {
		t.Fatalf("headers missing or out of order:\n%s", s)
	}
	if !strings.Contains(s[needs:fin], "02000000") || strings.Contains(s[needs:fin], "01000000") {
		t.Errorf("the waiting task's row is not under its header:\n%s", s)
	}
	if !strings.Contains(s, "RUNNERS") {
		t.Errorf("the runner section is gone:\n%s", s)
	}
}

func TestRenderListOrderedCreatedHasNoHeaders(t *testing.T) {
	var out bytes.Buffer
	renderListOrdered(orderFixture(), &out, OrderCreated, orderNow)
	if strings.Contains(out.String(), "==") {
		t.Errorf("created mode printed a header:\n%s", out.String())
	}
}

// --json --order: rows in that order, each naming its group; without --order
// the JSON carries no group key at all.
func TestListJSONOrderedCarriesGroup(t *testing.T) {
	var out bytes.Buffer
	renderListJSONOrdered(orderFixture(), &out, OrderRepo, orderNow)
	var doc struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tasks) != 2 || doc.Tasks[0]["group"] != "a" || doc.Tasks[1]["group"] != "b" {
		t.Errorf("rows = %v, want group a then b", doc.Tasks)
	}
	out.Reset()
	renderListJSON(orderFixture(), &out)
	if strings.Contains(out.String(), `"group"`) {
		t.Errorf("plain --json carries a group key: %s", out.String())
	}
}
