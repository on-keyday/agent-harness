package verb

import "testing"

// Every file verb offers the route, and every one of them defaults to the
// splice. A verb that shipped with the flag defaulted true would route silently
// on a surface nobody thought to check -- which is how the route became the
// default the first time.
func TestDataPlaneFlagIsOptInOnEveryFileVerb(t *testing.T) {
	seen := 0
	for _, v := range Verbs {
		if len(v.Path) == 0 || v.Path[0] != "file" {
			continue
		}
		var found *Flag
		for i := range v.Flags {
			if v.Flags[i].Name == "data-plane" {
				found = &v.Flags[i]
			}
		}
		if found == nil {
			t.Errorf("%v has no data-plane flag: the route cannot be asked for there", v.Path)
			continue
		}
		seen++
		if found.Type != FlagBool {
			t.Errorf("%v: data-plane is %v, want a bool", v.Path, found.Type)
		}
		if d, _ := found.Default.(bool); d {
			t.Errorf("%v: data-plane defaults to true, so it routes without being asked", v.Path)
		}
	}
	if seen != 7 {
		t.Errorf("checked %d file verbs, expected the 7 that take the flag", seen)
	}
}
