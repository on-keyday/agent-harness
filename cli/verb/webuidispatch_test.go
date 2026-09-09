package verb

import (
	"strings"
	"testing"
)

// Every verb the WebUI can type must declare HOW it is reached there. The
// WebUI's dispatch map is generated from these declarations, so a gap here is
// a verb that parses and then does nothing — which is the failure the page's
// old "declared but not dispatchable" assertion caught at startup, moved to
// where the declaration is written.
func TestEveryWebUIVerbDeclaresDispatch(t *testing.T) {
	for _, path := range PathsForSurface(WebUI) {
		sp, ok := Lookup(strings.Fields(path)...)
		if !ok {
			t.Errorf("%q is declared for the WebUI but Lookup does not find it", path)
			continue
		}
		if sp.WebUIDispatch.IsZero() {
			t.Errorf("%q is typable in the WebUI and declares no WebUIDispatch: "+
				"it would parse and then reach nothing", path)
		}
	}
}

// The reverse: a dispatch for a verb the WebUI cannot type promises something
// nobody can ask for. Cheap to write by accident when widening a mask the
// other way.
func TestNoWebUIDispatchOnUnreachableVerb(t *testing.T) {
	for _, v := range Verbs {
		if v.WebUIDispatch.IsZero() {
			continue
		}
		if !v.CmdlineSurfaces.Has(WebUI) {
			t.Errorf("%v declares a WebUIDispatch but is not typable in the WebUI "+
				"(CmdlineSurfaces = %v)", v.Path, v.CmdlineSurfaces)
		}
	}
}

// Exactly one way of answering, and it must be complete: a Cache without a
// Stale leaves the operator with no idea how old the rows are, which is the
// bound `forward ls` exists to record.
func TestWebUIDispatchShapeIsComplete(t *testing.T) {
	for _, v := range Verbs {
		d := v.WebUIDispatch
		if d.IsZero() {
			continue
		}
		if d.Fn != "" && (d.Cache != "" || d.Stale != "") {
			t.Errorf("%v declares both a Fn and a Cache — one verb answers one way", v.Path)
		}
		if (d.Cache == "") != (d.Stale == "") {
			t.Errorf("%v declares Cache=%q Stale=%q — a cached answer needs both",
				v.Path, d.Cache, d.Stale)
		}
		if d.Local && d.Fn == "" {
			t.Errorf("%v marks Local with no Fn to be local to", v.Path)
		}
	}
}
