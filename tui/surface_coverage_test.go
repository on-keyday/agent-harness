package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// The verb table's ModalSurfaces rows can be checked in one direction only from
// inside cli/verb: a named entry point must exist. This is the other direction
// for the TUI — every overlay this App can open must be named by some row, or
// be listed here as not being a verb's surface at all. Nothing else can find an
// overlay that was built and never recorded, which is how `forward ls` pointed
// at the wrong widget for weeks: the right one was never asked about.
//
// An overlay is any App field whose type has IsOpen() bool — the same predicate
// appOverlays routes keys by, so a new overlay reaches this test the moment it
// reaches the App.
var overlaysThatAreNotAVerbSurface = map[string]string{
	"DetailPopup":       "read-only detail and the `?` key list; it shows, it does nothing",
	"RunnerPickerModel": "resolves an ambiguous runner for a spawn already declared elsewhere; not a verb of its own",
}

func TestEveryOverlayIsNamedByTheVerbTable(t *testing.T) {
	named := map[string]bool{}
	for _, v := range verb.Verbs {
		for _, ms := range v.ModalSurfaces {
			if ms.Surface != verb.TUI || !strings.HasPrefix(ms.At, "tui/") {
				continue
			}
			if i := strings.LastIndex(ms.At, ":"); i > 0 {
				named[ms.At[i+1:]] = true
			}
		}
	}
	for _, name := range overlayTypeNames(t) {
		_, exempt := overlaysThatAreNotAVerbSurface[name]
		switch {
		case named[name] && exempt:
			t.Errorf("%s is named by a ModalSurfaces row AND listed here as not a verb's surface; one of the two is wrong", name)
		case !named[name] && !exempt:
			t.Errorf("%s is an overlay no verb row names.\n"+
				"  Either add {Surface: TUI, At: \"tui/<file>.go:%s\"} to the verb it reaches,\n"+
				"  or add it to overlaysThatAreNotAVerbSurface with the reason.", name, name)
		}
	}
	for name := range overlaysThatAreNotAVerbSurface {
		if !contains(overlayTypeNames(t), name) {
			t.Errorf("overlaysThatAreNotAVerbSurface lists %s, which is no longer an App overlay", name)
		}
	}
}

// overlayTypeNames is the App's overlays, by the predicate appOverlays uses.
func overlayTypeNames(t *testing.T) []string {
	t.Helper()
	var names []string
	at := reflect.TypeOf(App{})
	for i := 0; i < at.NumField(); i++ {
		ft := at.Field(i).Type
		m, ok := reflect.PointerTo(ft).MethodByName("IsOpen")
		if !ok || m.Type.NumIn() != 1 || m.Type.NumOut() != 1 || m.Type.Out(0).Kind() != reflect.Bool {
			continue
		}
		names = append(names, ft.Name())
	}
	if len(names) < 10 {
		t.Fatalf("found only %d overlay fields on App; the IsOpen predicate no longer finds them", len(names))
	}
	return names
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
