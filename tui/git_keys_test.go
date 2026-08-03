package tui

import (
	"reflect"
	"testing"
)

// viewportScrollKeys is bubbles/viewport's own default keymap. The App
// intercepts modal keys BEFORE forwarding to the viewport, so a modal key that
// collides here does not merely shadow scrolling — it silently runs the
// modal's action while the operator is trying to scroll.
//
// This bit twice: `b` was set-base while being the viewport's page-up, and `u`
// was up-one-repo while being its half-page-up. Paging through a long diff
// moved the baseline; half-paging up left the nested repository.
var viewportScrollKeys = map[string]bool{
	"pgdown": true, " ": true, "f": true,
	"pgup": true, "b": true,
	"u": true, "ctrl+u": true,
	"d": true, "ctrl+d": true,
	"k": true, "j": true,
	"h": true, "l": true,
}

func TestModalKeysDoNotShadowScrolling(t *testing.T) {
	v := reflect.ValueOf(modalKeys)
	ty := v.Type()
	for i := 0; i < v.NumField(); i++ {
		key := v.Field(i).String()
		if viewportScrollKeys[key] {
			t.Errorf("modalKeys.%s = %q, which is a viewport scroll key: pressing it to scroll would run the modal action instead",
				ty.Field(i).Name, key)
		}
	}
}

// Up and Down are deliberately taken by the row picker, so the content pane is
// scrolled with the other bindings. That is a choice, not an oversight — but
// it means at least one page and one half-page key must survive.
func TestScrollKeysSurviveForTheContentPane(t *testing.T) {
	taken := map[string]bool{}
	v := reflect.ValueOf(modalKeys)
	for i := 0; i < v.NumField(); i++ {
		taken[v.Field(i).String()] = true
	}
	for _, group := range [][]string{
		{"pgdown", " ", "f"}, // page down
		{"pgup", "b"},        // page up
		{"d", "ctrl+d"},      // half down
		{"u", "ctrl+u"},      // half up
	} {
		free := false
		for _, k := range group {
			if !taken[k] {
				free = true
			}
		}
		if !free {
			t.Errorf("every key in %v is taken by a modal action; that scroll direction is unreachable", group)
		}
	}
}
