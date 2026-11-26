package cli

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Bound enrollment (C1/C2): the binding is shown wherever a key or a runner
// on one is, and the undo an unenroll prints restores it.

func bindID(b byte) protocol.TaskID {
	var t protocol.TaskID
	for i := range t.Id {
		t.Id[i] = b
	}
	return t
}

func TestFormatRunnerKeysShowsBinding(t *testing.T) {
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	got := FormatRunnerKeys([]RunnerKeyRow{
		{Key: testKey(1), Label: "free", EnrolledAt: at, EnrolledBy: "operator"},
		{Key: testKey(2), Label: "vm", EnrolledAt: at, EnrolledBy: "operator", Bound: []protocol.TaskID{bindID(0xa1), bindID(0xb2)}},
	})
	for _, want := range []string{"label=free enrolled=2026-11-26T09:00:00Z by=operator binding=unbound", "label=vm enrolled=2026-11-26T09:00:00Z by=operator binding=a1a1a1a1,b2b2b2b2"} {
		if !strings.Contains(got, want) {
			t.Errorf("runner keys output lacks %q:\n%s", want, got)
		}
	}
}

func TestFormatRunnerKeysJSONBoundTasks(t *testing.T) {
	got := FormatRunnerKeysJSON([]RunnerKeyRow{
		{Key: testKey(1)},
		{Key: testKey(2), Bound: []protocol.TaskID{bindID(0xa1)}},
	})
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"bound_tasks":[]`) ||
		!strings.Contains(lines[1], `"bound_tasks":["`+bindHexOf(bindID(0xa1))+`"]`) {
		t.Fatalf("runner keys --json:\n%s", got)
	}
}

// The undo after an unenroll must restore the binding: without --bind it
// would put the key back unbound, wider than what was removed.
func TestFormatRunnerUnenrollRestoresBinding(t *testing.T) {
	a, b := bindID(0xa1), bindID(0xb2)
	out, ok := FormatRunnerUnenroll(protocol.RunnerKeyStatus_Ok, RunnerUnenrollResult{Key: testKey(3), Label: "vm", Bound: []protocol.TaskID{a, b}})
	want := "undo: harness-cli runner enroll " + hex.EncodeToString(testKey(3)) + " --label vm --bind " +
		hex.EncodeToString(a.Id[:]) + "," + hex.EncodeToString(b.Id[:])
	if !ok || !strings.Contains(out, want) {
		t.Fatalf("unenroll output lacks %q:\n%s", want, out)
	}
	out, _ = FormatRunnerUnenroll(protocol.RunnerKeyStatus_Ok, RunnerUnenrollResult{Key: testKey(3)})
	if strings.Contains(out, "--bind") {
		t.Fatalf("an unbound key's undo names --bind:\n%s", out)
	}
}

func TestFormatRunnerEnrollNewStatuses(t *testing.T) {
	k := testKey(0xab)
	for st, want := range map[protocol.RunnerKeyStatus]string{
		protocol.RunnerKeyStatus_OutOfReach:     "an unbound key needs runner_enroll at global scope",
		protocol.RunnerKeyStatus_BindingDiffers: "unenroll it and enroll again",
		protocol.RunnerKeyStatus_BadBinding:     "at most 16",
	} {
		if got := FormatRunnerEnroll(st, k, "", nil); !strings.Contains(got, want) {
			t.Errorf("%v: %q lacks %q", st, got, want)
		}
	}
	if got := FormatRunnerEnroll(protocol.RunnerKeyStatus_OutOfReach, k, "", []protocol.TaskID{bindID(0xa1)}); !strings.Contains(got, "a --bind task is outside your runner_enroll scope") {
		t.Errorf("out_of_reach, bound: %q", got)
	}
	if got := FormatRunnerEnroll(protocol.RunnerKeyStatus_Ok, k, "vm", []protocol.TaskID{bindID(0xa1)}); got != "enrolled abababababababab label=vm binding=a1a1a1a1" {
		t.Errorf("ok, bound: %q", got)
	}
	if got := FormatRunnerEnroll(protocol.RunnerKeyStatus_Ok, k, "gmk", nil); got != "enrolled abababababababab label=gmk binding=unbound" {
		t.Errorf("ok, unbound: %q", got)
	}
}

func TestRunnerRowShowsBinding(t *testing.T) {
	var free, bound protocol.RunnerInfo
	free.SetHostname([]byte("h1"))
	bound.SetHostname([]byte("h2"))
	bound.SetBound([]protocol.TaskID{bindID(0xa1)})
	var lr protocol.ListResultBody
	lr.SetRunners([]protocol.RunnerInfo{free, bound})

	var human bytes.Buffer
	renderList(&lr, &human)
	if !strings.Contains(human.String(), "binding=unbound") || !strings.Contains(human.String(), "binding=a1a1a1a1") {
		t.Fatalf("ls runner rows lack the binding:\n%s", human.String())
	}
	var js bytes.Buffer
	renderListJSON(&lr, &js)
	if !strings.Contains(js.String(), `"bound_tasks":[]`) || !strings.Contains(js.String(), `"bound_tasks":["`+bindHexOf(bindID(0xa1))+`"]`) {
		t.Fatalf("ls --json runner rows lack bound_tasks:\n%s", js.String())
	}
}

func bindHexOf(t protocol.TaskID) string { return hex.EncodeToString(t.Id[:]) }
