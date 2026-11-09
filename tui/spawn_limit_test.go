package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Review Focus 5: a resume never re-grants the session's default limits; only
// a limit named on the command line does.
func TestResumeNeverSendsSessionDefaultLimits(t *testing.T) {
	a := App{sessionSpawnDepth: authority.LimitOf(1), sessionMaxDescendants: authority.LimitOf(2)}
	auth := a.spawnAuthority(verb.SpawnAction{ResumeTaskID: "aa"}, 0)
	if auth.SpawnDepthPresent || auth.MaxDescendantsPresent {
		t.Errorf("a resume re-grants the session defaults: %+v", auth)
	}
	fresh := a.spawnAuthority(verb.SpawnAction{}, 0)
	if !fresh.SpawnDepth.Limited() || fresh.SpawnDepth.Value() != 1 || fresh.MaxDescendants.Value() != 2 {
		t.Errorf("a fresh spawn does not carry the session defaults: %+v", fresh)
	}
	typed := authority.LimitOf(5)
	auth = a.spawnAuthority(verb.SpawnAction{ResumeTaskID: "aa", SpawnDepth: &typed, SpawnDepthPresent: true}, 0)
	if !auth.SpawnDepthPresent || auth.SpawnDepth.Value() != 5 || auth.MaxDescendantsPresent {
		t.Errorf("a typed --spawn-depth on a resume: %+v", auth)
	}
}

func cursorTo(t *testing.T, m *AuthorityPickerModel, kind pickerRowKind) {
	t.Helper()
	for i, r := range m.rows {
		if r.kind == kind {
			m.cursor = i
			return
		}
	}
	t.Fatalf("no row of kind %d", kind)
}

func TestPickerLimitsSection(t *testing.T) {
	var target protocol.TaskInfo
	target.Id.Id[0] = 0x3f
	target.SpawnDepth = authority.LimitOf(3)
	var m AuthorityPickerModel
	m.SetSize(120, 80)
	m.OpenRegrant(target, nil)

	depth, max, changed := m.Limits()
	if depth.Value() != 3 || !depth.Limited() || max.Limited() || changed {
		t.Fatalf("prefill: depth %s max %s changed %v", authority.LimitLabel(depth), authority.LimitLabel(max), changed)
	}

	cursorTo(t, &m, rowMaxDescendants)
	m.TypeDigit('1')
	m.TypeDigit('2')
	if _, max, changed = m.Limits(); max.Value() != 12 || !max.Limited() || !changed {
		t.Errorf("typed 12: max %s changed %v", authority.LimitLabel(max), changed)
	}
	m.Backspace()
	if _, max, _ = m.Limits(); max.Value() != 1 {
		t.Errorf("backspace: max %s, want 1", authority.LimitLabel(max))
	}
	m.Backspace()
	m.Backspace()
	if _, max, _ = m.Limits(); max.Limited() {
		t.Errorf("backspace past 0: max %s, want unbounded", authority.LimitLabel(max))
	}

	cursorTo(t, &m, rowSpawnDepth)
	m.Toggle()
	if depth, _, _ = m.Limits(); depth.Limited() {
		t.Errorf("space on a limited depth: %s, want unbounded", authority.LimitLabel(depth))
	}
	m.Toggle()
	if depth, _, _ = m.Limits(); !depth.Limited() || depth.Value() != 0 {
		t.Errorf("space on unbounded: %s, want 0", authority.LimitLabel(depth))
	}
	if !strings.Contains(m.View(), "spawn depth: 0") {
		t.Errorf("view does not show the depth row:\n%s", m.View())
	}
}

func TestDetailShowsLimits(t *testing.T) {
	var task protocol.TaskInfo
	got := formatTaskDetail(task, nil)
	if !strings.Contains(got, "spawn limits:  depth=unbounded max-descendants=unbounded descendants=0") {
		t.Errorf("detail:\n%s", got)
	}
}
