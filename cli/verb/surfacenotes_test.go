package verb

import "testing"

// A SurfaceNotes entry for a surface the verb is not reachable on describes
// something nobody can type there. This is the failure mode that REPLACED the
// one the surfaces' own description maps had: a map keyed by path could name a
// path that no longer existed, and a note inside the VerbSpec cannot — but it
// can still be scoped to the wrong surface.
//
// Checked here rather than per surface because it is a property of the table,
// and one test covering every surface cannot be the one a new surface forgets.
func TestSurfaceNotesOnlyForReachableSurfaces(t *testing.T) {
	for _, v := range Verbs {
		for s, notes := range v.SurfaceNotes {
			if len(notes) == 0 {
				t.Errorf("%v declares an EMPTY SurfaceNotes entry for %v — drop the key instead",
					v.Path, s)
				continue
			}
			if !v.CmdlineSurfaces.Has(s) {
				t.Errorf("%v has SurfaceNotes for %v but is not reachable there "+
					"(CmdlineSurfaces = %v): nobody can type it to read that note",
					v.Path, s, v.CmdlineSurfaces)
			}
		}
	}
}
