package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func reachID(b byte) protocol.TaskID {
	var id protocol.TaskID
	for i := range id.Id {
		id.Id[i] = b
	}
	return id
}

func reachHex(b byte) string { id := reachID(b); return hex.EncodeToString(id.Id[:]) }

// P (operator-created, cancel,file_read over its subtree) and its child C.
func reachListing(cScope protocol.TaskScope) []protocol.TaskInfo {
	p := protocol.TaskInfo{Id: reachID(0x3f), Capabilities: protocol.Capability_Cancel | protocol.Capability_FileRead}
	c := protocol.TaskInfo{Id: reachID(0x8f), CreatorTaskId: reachID(0x3f), Capabilities: protocol.Capability_Cancel, Scope: cScope}
	return []protocol.TaskInfo{p, c}
}

func reachText(t *testing.T, tasks []protocol.TaskInfo, id string, focus protocol.Capability) []string {
	t.Helper()
	r, err := BuildReachReport(tasks, id, focus)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, false); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

func line(name, body string) string { return fmt.Sprintf("  %-14s %s", name, body) }

func TestReachReportLines(t *testing.T) {
	got := reachText(t, reachListing(protocol.TaskScope{}), reachHex(0x3f), 0)
	both := "act 2: 3f3f3f3f 8f8f8f8f   see +0"
	want := map[string]bool{
		"caps reach 3f3f3f3f  granter=unknown":   true,
		line("cancel", both):                     true,
		line("file_read", both):                  true,
		line("view", "see 2: 3f3f3f3f 8f8f8f8f"): true,
		line("delegate", "caps=none  "+both):     true,
	}
	if len(got) != len(want) || got[0] != "caps reach 3f3f3f3f  granter=unknown" {
		t.Fatalf("report:\n%s", strings.Join(got, "\n"))
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected line %q in:\n%s", l, strings.Join(got, "\n"))
		}
	}
	if got[len(got)-2] != line("view", "see 2: 3f3f3f3f 8f8f8f8f") || got[len(got)-1] != line("delegate", "caps=none  "+both) {
		t.Errorf("view and delegate must close the report:\n%s", strings.Join(got, "\n"))
	}
}

// Review Focus 1: a global reach is said to include what the caller cannot
// list, however few tasks it lists.
func TestReachReportGlobalSaysUnlisted(t *testing.T) {
	got := strings.Join(reachText(t, reachListing(protocol.TaskScope{Base: protocol.ScopeBase_Global}), reachHex(0x8f), 0), "\n")
	for _, want := range []string{
		line("cancel", "act global (+ tasks you cannot list)"),
		line("view", "see global (+ tasks you cannot list)"),
		"  wider than parent: cancel +vis (granter=unknown)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}

func TestReachReportFocus(t *testing.T) {
	r, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x3f), protocol.Capability_Cancel)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Bits) != 1 || r.Bits[0].Cap != "cancel" {
		t.Errorf("focused report bits = %+v, want cancel only", r.Bits)
	}
}

func TestReachReportUnknownTask(t *testing.T) {
	if _, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x11), 0); err == nil || !strings.Contains(err.Error(), "11111111") {
		t.Errorf("err = %v, want one naming the id", err)
	}
}

func TestReachReportJSON(t *testing.T) {
	r, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x3f), 0)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "null") {
		t.Errorf("JSON carries null where an empty list belongs: %s", b.String())
	}
	var got struct {
		Task    string `json:"task"`
		Granter string `json:"granter"`
		Bits    []struct {
			Cap string   `json:"cap"`
			Act []string `json:"act"`
			See []string `json:"see"`
		} `json:"bits"`
		Delegate struct {
			Caps string   `json:"caps"`
			Act  []string `json:"act"`
		} `json:"delegate"`
		Wider struct {
			Caps string `json:"caps"`
			Sees bool   `json:"sees"`
		} `json:"wider"`
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, b.String())
	}
	if got.Task != reachHex(0x3f) || got.Granter != "unknown" || len(got.Bits) != 2 ||
		len(got.Bits[0].Act) != 2 || got.Bits[0].Act[0] != reachHex(0x3f) ||
		got.Delegate.Caps != "none" || got.Wider.Caps != "none" || got.Wider.Sees {
		t.Errorf("json = %s", b.String())
	}
}

func TestWiderLine(t *testing.T) {
	if got := WiderLine(authority.Wider{}, "unknown"); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got, want := WiderLine(authority.Wider{Bits: protocol.Capability_Cancel, Sees: true}, "spawn@3f3f3f3f (1s ago)"),
		"wider than parent: cancel +vis (granter=spawn@3f3f3f3f (1s ago))"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := WiderLine(authority.Wider{Sees: true}, "unknown"), "wider than parent: +vis (granter=unknown)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
