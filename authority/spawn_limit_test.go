package authority

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func sameLimit(a, b protocol.SpawnLimit) bool {
	return a.Limited() == b.Limited() && a.Value() == b.Value()
}

func TestDecLimit(t *testing.T) {
	for _, c := range []struct{ in, want protocol.SpawnLimit }{
		{Unbounded(), Unbounded()},
		{LimitOf(0), LimitOf(0)},
		{LimitOf(1), LimitOf(0)},
		{LimitOf(3), LimitOf(2)},
	} {
		if got := DecLimit(c.in); !sameLimit(got, c.want) {
			t.Errorf("DecLimit(%s) = %s, want %s", LimitLabel(c.in), LimitLabel(got), LimitLabel(c.want))
		}
	}
}

func TestMinLimit(t *testing.T) {
	for _, c := range []struct{ a, b, want protocol.SpawnLimit }{
		{Unbounded(), Unbounded(), Unbounded()},
		{Unbounded(), LimitOf(4), LimitOf(4)},
		{LimitOf(4), Unbounded(), LimitOf(4)},
		{LimitOf(4), LimitOf(2), LimitOf(2)},
		{LimitOf(0), LimitOf(2), LimitOf(0)},
	} {
		if got := MinLimit(c.a, c.b); !sameLimit(got, c.want) {
			t.Errorf("MinLimit(%s, %s) = %s, want %s", LimitLabel(c.a), LimitLabel(c.b), LimitLabel(got), LimitLabel(c.want))
		}
	}
}

func TestLimitLabel(t *testing.T) {
	if LimitLabel(Unbounded()) != "unbounded" || LimitLabel(LimitOf(0)) != "0" || LimitLabel(LimitOf(12)) != "12" {
		t.Errorf("labels: %q %q %q", LimitLabel(Unbounded()), LimitLabel(LimitOf(0)), LimitLabel(LimitOf(12)))
	}
}

// A child's depth is min(requested, held - 1): the limit only ever shrinks
// down a chain, and an omitted request takes the creator's minus one.
func TestChildSpawnDepth(t *testing.T) {
	held := []protocol.SpawnLimit{Unbounded(), LimitOf(0), LimitOf(1), LimitOf(3)}
	req := []protocol.SpawnLimit{Unbounded(), LimitOf(0), LimitOf(5)}
	want := [][]protocol.SpawnLimit{
		{Unbounded(), LimitOf(0), LimitOf(5)},
		{LimitOf(0), LimitOf(0), LimitOf(0)},
		{LimitOf(0), LimitOf(0), LimitOf(0)},
		{LimitOf(2), LimitOf(0), LimitOf(2)},
	}
	for i, h := range held {
		g := NewGranter(delegP, Grant{SpawnDepth: h}, delegTree)
		for j, r := range req {
			if got := g.ChildSpawnDepth(r); !sameLimit(got, want[i][j]) {
				t.Errorf("held %s, requested %s: got %s, want %s",
					LimitLabel(h), LimitLabel(r), LimitLabel(got), LimitLabel(want[i][j]))
			}
		}
	}
	var operator *Granter
	if got := operator.ChildSpawnDepth(LimitOf(5)); !sameLimit(got, LimitOf(5)) {
		t.Errorf("operator: got %s, want the request", LimitLabel(got))
	}
}

// A cascade rewrites a descendant's caps, scope and bound; it never touches
// its spawn limits (spec D13), and it builds a fresh Grant, so they must be
// carried over explicitly.
func TestClampDescendantKeepsLimits(t *testing.T) {
	g := NewGranter(delegP, Grant{Caps: protocol.Capability_FileRead, Scope: DefaultScope()}, delegTree)
	d := Grant{Caps: protocol.Capability_FileRead, SpawnDepth: LimitOf(2), MaxDescendants: LimitOf(4)}
	got := g.ClampDescendant(d, delegS)
	if !sameLimit(got.SpawnDepth, LimitOf(2)) || !sameLimit(got.MaxDescendants, LimitOf(4)) {
		t.Errorf("cascade changed the limits: depth %s max %s", LimitLabel(got.SpawnDepth), LimitLabel(got.MaxDescendants))
	}
}
