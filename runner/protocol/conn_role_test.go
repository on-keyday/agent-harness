package protocol

import "testing"

// The four PRINCIPAL kinds each get their own role, and they must stay
// distinct: the mapping exists so the two ends of one connection agree about
// it, and two kinds sharing a role would make a surface unable to say which
// peer it is looking at.
func TestPrincipalClientKindsMapToDistinctRoles(t *testing.T) {
	seen := map[ConnRole]ClientKind{}
	for _, k := range []ClientKind{ClientKind_Cli, ClientKind_Tui, ClientKind_Webui, ClientKind_Agent} {
		r := ConnRoleForClientKind(k)
		if r == ConnRole_Unspecified {
			t.Errorf("%v maps to Unspecified: a connected, identified peer would read as unidentified", k)
			continue
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("%v and %v both map to %v; a surface cannot tell them apart", prev, k, r)
		}
		seen[r] = k
	}
}

// data_plane has NO role, and that is the decision rather than a gap: such a
// connection carries one authorized request's bytes and speaks for no task, so
// there is no principal for an operator surface to name. Pinned because the
// obvious "every kind should have a role" reading would quietly give it one.
func TestDataPlaneIsNotAPrincipalAndHasNoRole(t *testing.T) {
	if got := ConnRoleForClientKind(ClientKind_DataPlane); got != ConnRole_Unspecified {
		t.Errorf("data_plane mapped to %v; it speaks for one request, not for a task", got)
	}
}
