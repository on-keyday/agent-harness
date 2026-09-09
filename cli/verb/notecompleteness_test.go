package verb

import (
	"strings"
	"testing"
)

// Every verb a surface can type must SAY something there, not just show a
// synopsis. The synopsis is generated, so it always mentions the verb — which
// is why "is it listed?" stopped being an interesting question the moment the
// lists were generated, and "does the line say anything?" became the one worth
// asking.
//
// One test for every surface at once, because the per-surface version is what a
// new surface forgets: the TUI had this guard over its own description map, the
// WebUI had none, and the WebUI is where `--via <cid>` rotted.
//
// A shared Note satisfies it. SurfaceNotes is for when the answer differs by
// surface — `file pull` writes a local path on the CLI and opens a browser
// download in the WebUI.
func TestEveryReachableVerbHasANote(t *testing.T) {
	for _, s := range []struct {
		name string
		surf Surface
	}{{"CLI", CLI}, {"TUI", TUI}, {"WebUI", WebUI}} {
		for _, path := range PathsForSurface(s.surf) {
			sp, ok := Lookup(strings.Fields(path)...)
			if !ok {
				t.Errorf("%s: %q is declared but Lookup does not find it", s.name, path)
				continue
			}
			notes := sp.For(s.surf).Notes
			if len(notes) == 0 || strings.TrimSpace(notes[0]) == "" {
				t.Errorf("%s: %q has no note. Add one to Notes, or to "+
					"SurfaceNotes[%s] when it only applies there — an operator "+
					"reading a bare synopsis learns the flags and not the point.",
					s.name, path, s.name)
			}
		}
	}
}
