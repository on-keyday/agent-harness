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
	r, err := BuildReachReport(tasks, id, focus, true)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, ReachText); err != nil {
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
	r, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x3f), protocol.Capability_Cancel, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Bits) != 1 || r.Bits[0].Cap != "cancel" {
		t.Errorf("focused report bits = %+v, want cancel only", r.Bits)
	}
}

func TestReachReportUnknownTask(t *testing.T) {
	if _, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x11), 0, true); err == nil || !strings.Contains(err.Error(), "11111111") {
		t.Errorf("err = %v, want one naming the id", err)
	}
}

func TestReachReportJSON(t *testing.T) {
	r, err := BuildReachReport(reachListing(protocol.TaskScope{}), reachHex(0x3f), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, ReachJSON); err != nil {
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

// spawn, notify, runner_admin, board_observe and board_send name no task, so
// they have no per-task reach: the report leaves them out, and a child is not
// "wider" through one of them.
func TestReachReportSkipsUntargetedBits(t *testing.T) {
	tasks := reachListing(protocol.TaskScope{Base: protocol.ScopeBase_Global})
	tasks[0].Capabilities |= protocol.Capability_Spawn | protocol.Capability_Notify
	tasks[1].Capabilities = protocol.Capability_Spawn
	got := strings.Join(reachText(t, tasks, reachHex(0x3f), 0), "\n")
	if strings.Contains(got, "spawn ") || strings.Contains(got, "notify ") {
		t.Errorf("an untargeted bit is listed as reaching tasks:\n%s", got)
	}
	r, err := BuildReachReport(tasks, reachHex(0x8f), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Wider.Bits != 0 {
		t.Errorf("wider through %s, which names no task", CapsLabel(r.Wider.Bits))
	}
	if bits := ReachBits(protocol.Capability_Spawn | protocol.Capability_Cancel); len(bits) != 1 || bits[0] != protocol.Capability_Cancel {
		t.Errorf("ReachBits = %v, want cancel only", bits)
	}
}

// A confined caller's listing has gaps — tasks it cannot see between its
// parent and its targets — so a comparison with the parent over it can claim a
// widening that does not exist. The report says it was not computed instead.
func TestReachReportPartialListingOmitsWider(t *testing.T) {
	tasks := reachListing(protocol.TaskScope{Base: protocol.ScopeBase_Global})
	r, err := BuildReachReport(tasks, reachHex(0x8f), 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, ReachText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "  wider than parent: not computed (your listing is partial)") || strings.Contains(b.String(), "cancel +vis") {
		t.Errorf("partial listing report:\n%s", b.String())
	}
}

// The delegate line's caps are what a child can actually be given: the
// holder's caps inside its bound, not the bound as written.
func TestReachReportDelegateCapsAreGrantable(t *testing.T) {
	tasks := reachListing(protocol.TaskScope{})
	tasks[0].DelegateCaps = protocol.Capability_All
	got := strings.Join(reachText(t, tasks, reachHex(0x3f), 0), "\n")
	if !strings.Contains(got, "caps=cancel,file_read") {
		t.Errorf("delegate caps should be caps ∩ bound:\n%s", got)
	}
}

// A holder of bits that name no task is told so, rather than those bits
// silently vanishing; and --cap on such a bit says why there is no line.
func TestReachReportNamesBitsWithoutPerTaskReach(t *testing.T) {
	tasks := reachListing(protocol.TaskScope{})
	tasks[0].Capabilities |= protocol.Capability_Spawn | protocol.Capability_BoardObserve
	got := strings.Join(reachText(t, tasks, reachHex(0x3f), 0), "\n")
	if !strings.Contains(got, "  also holds (no per-task reach): spawn,board_observe") {
		t.Errorf("report lacks the untargeted-bits line:\n%s", got)
	}
	got = strings.Join(reachText(t, tasks, reachHex(0x3f), protocol.Capability_Spawn), "\n")
	if !strings.Contains(got, line("spawn", "no per-task reach (it names no task)")) {
		t.Errorf("--cap spawn says nothing:\n%s", got)
	}
	got = strings.Join(reachText(t, tasks, reachHex(0x3f), protocol.Capability_ExecView), "\n")
	if !strings.Contains(got, line("exec_view", "not held")) {
		t.Errorf("--cap on a bit it does not hold says nothing:\n%s", got)
	}
}

// R (operator) created S, the subject; S created A and then B; A created G.
// X and Z are other operator tasks; S names X by id. S holds cancel over its
// subtree and X, and file_read only over itself (an override with base none).
func reachTreeListing() []protocol.TaskInfo {
	running := protocol.TaskStatus_Running
	task := func(id, creator byte, at uint64) protocol.TaskInfo {
		t := protocol.TaskInfo{Id: reachID(id), Status: running, CreatedAt: at}
		if creator != 0 {
			t.CreatorTaskId = reachID(creator)
		}
		return t
	}
	s := task(0x22, 0x11, 2)
	s.Capabilities = protocol.Capability_Cancel | protocol.Capability_FileRead
	s.Scope.SetIds([]protocol.TaskID{reachID(0x66)})
	s.Overrides = []protocol.ScopeOverride{{Caps: protocol.Capability_FileRead, Base: protocol.ScopeBase_None}}
	return []protocol.TaskInfo{task(0x11, 0, 1), s, task(0x33, 0x22, 3), task(0x44, 0x33, 4), task(0x55, 0x22, 5), task(0x66, 0, 6), task(0x77, 0, 7)}
}

func reachTreeText(t *testing.T, tasks []protocol.TaskInfo, id string, focus protocol.Capability) string {
	t.Helper()
	r, err := BuildReachReport(tasks, id, focus, true)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteReachReport(&b, r, ReachTree); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// --tree turns the lines around: each reached task, named and placed under
// its creator, with the bits that act on it. R reaches nothing and is drawn
// only to carry S; Z is reached by no line and is left out.
func TestReachTree(t *testing.T) {
	got := reachTreeText(t, reachTreeListing(), reachHex(0x22), 0)
	want := `  reach tree (by creator):
  11111111 Running  act=none see=no delegate=none
  └─ 22222222 Running  act=all 2 bits above see=yes delegate=act  (this task)
     ├─ 33333333 Running  act=cancel see=yes delegate=act
     │  └─ 44444444 Running  act=cancel see=yes delegate=act
     └─ 55555555 Running  act=cancel see=yes delegate=act
  66666666 Running  act=cancel see=yes delegate=none
`
	if !strings.HasSuffix(got, want) {
		t.Errorf("tree:\n%s\nwant it to end with:\n%s", got, want)
	}
	if strings.Contains(got, "77777777") {
		t.Errorf("a task no line reaches is drawn:\n%s", got)
	}
	// Without --tree the report is unchanged.
	if strings.Contains(strings.Join(reachText(t, reachTreeListing(), reachHex(0x22), 0), "\n"), "reach tree") {
		t.Error("the plain report draws the tree")
	}
}

// --cap keeps one bit, and the tree's act marks follow it.
func TestReachTreeFocus(t *testing.T) {
	got := reachTreeText(t, reachTreeListing(), reachHex(0x22), protocol.Capability_FileRead)
	for _, want := range []string{
		"  └─ 22222222 Running  act=file_read see=yes delegate=act  (this task)\n",
		"     ├─ 33333333 Running  act=none see=yes delegate=act\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tree lacks %q:\n%s", want, got)
		}
	}
}

// A global line reaches every listed task, so all of them are drawn, and the
// tree says the tasks the caller cannot list are reached too.
func TestReachTreeGlobal(t *testing.T) {
	tasks := reachTreeListing()
	tasks[1].Scope = protocol.TaskScope{Base: protocol.ScopeBase_Global}
	tasks[1].Overrides = nil
	got := reachTreeText(t, tasks, reachHex(0x22), 0)
	for _, want := range []string{
		"  77777777 Running  act=all 2 bits above see=yes",
		"  (a global line above also reaches tasks you cannot list; they are not drawn)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tree lacks %q:\n%s", want, got)
		}
	}
}
