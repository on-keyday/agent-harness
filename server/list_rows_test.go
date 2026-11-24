package server

import (
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// defaultListQuery is the list request every client sends unless asked for
// another bound: the tests that read a list read this one.
func defaultListQuery() protocol.ListQuery {
	return protocol.ListQuery{FinishedLimit: verb.FinishedDefault}
}

// rowsTask is one fixture task: its name, its creator's name, whether it ended.
type rowsTask struct {
	name, creator string
	ended         bool
}

func rowsStore(spec []rowsTask) ([]TaskEntry, map[string]string) {
	id := func(n string) string {
		var b [16]byte
		copy(b[:], n)
		return hex.EncodeToString(b[:])
	}
	names := map[string]string{}
	var all []TaskEntry
	for _, s := range spec {
		e := TaskEntry{ID: id(s.name), Status: protocol.TaskStatus_Running}
		if s.ended {
			e.Status = protocol.TaskStatus_Succeeded
		}
		if s.creator != "" {
			raw, _ := hex.DecodeString(id(s.creator))
			copy(e.CreatorTaskID.Id[:], raw)
		}
		names[e.ID] = s.name
		all = append(all, e)
	}
	return all, names
}

func rowNames(rows []TaskEntry, names map[string]string) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, names[r.ID])
	}
	return out
}

func everyone(string) bool { return true }

// Every live task is listed however old; finished ones are bounded, newest
// first, and the ones left out are counted.
func TestListRowsKeepsOldLiveAndBoundsFinished(t *testing.T) {
	all, names := rowsStore([]rowsTask{
		{name: "live"}, {name: "old", ended: true}, {name: "mid", ended: true}, {name: "new", ended: true},
	})
	rows, omitted := listRows(all, everyone, 1)
	if got, want := rowNames(rows, names), []string{"live", "new"}; !reflect.DeepEqual(got, want) || omitted != 2 {
		t.Fatalf("listRows(limit 1) = %v omitted %d, want %v omitted 2", got, omitted, want)
	}
	rows, omitted = listRows(all, everyone, 0xffffffff)
	if len(rows) != 4 || omitted != 0 {
		t.Fatalf("listRows(all) = %v omitted %d, want all four, omitted 0", rowNames(rows, names), omitted)
	}
}

// The bound counts what the caller may see: another tree's newer finished
// tasks do not push the caller's own history out, nor count as omitted.
func TestListRowsBoundsAfterVisibility(t *testing.T) {
	all, names := rowsStore([]rowsTask{
		{name: "mine", ended: true}, {name: "theirs1", ended: true}, {name: "theirs2", ended: true},
	})
	mine := all[0].ID
	rows, omitted := listRows(all, func(id string) bool { return id == mine }, 1)
	if got := rowNames(rows, names); !reflect.DeepEqual(got, []string{"mine"}) || omitted != 0 {
		t.Fatalf("confined listRows = %v omitted %d, want [mine] omitted 0", got, omitted)
	}
}

// A listed task's creators come along, up to the root, whatever their age:
// they neither use up the bound nor count as omitted. The walk stops at a
// creator the caller may not see.
func TestListRowsBringsCreators(t *testing.T) {
	all, names := rowsStore([]rowsTask{
		{name: "root", ended: true},
		{name: "mid", creator: "root", ended: true},
		{name: "noise", ended: true},
		{name: "child", creator: "mid"},
	})
	rows, omitted := listRows(all, everyone, 0)
	if got, want := rowNames(rows, names), []string{"root", "mid", "child"}; !reflect.DeepEqual(got, want) || omitted != 1 {
		t.Fatalf("listRows(limit 0) = %v omitted %d, want %v omitted 1 (noise)", got, omitted, want)
	}
	root := all[0].ID
	rows, _ = listRows(all, func(id string) bool { return id != root }, 0)
	if got, want := rowNames(rows, names), []string{"mid", "child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listRows with root unseen = %v, want %v", got, want)
	}
}
