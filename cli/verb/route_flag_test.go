package verb

import "testing"

// Every file verb names its route, and every one of them defaults to the
// splice. A verb shipping with a different default would route on a surface
// nobody thought to check -- which is how the end-to-end path became the
// default the first time.
func TestRouteFlagDefaultsToSpliceOnEveryFileVerb(t *testing.T) {
	seen := 0
	for _, v := range Verbs {
		if len(v.Path) == 0 || v.Path[0] != "file" {
			continue
		}
		var found *Flag
		for i := range v.Flags {
			if v.Flags[i].Name == "route" {
				found = &v.Flags[i]
			}
		}
		if found == nil {
			t.Errorf("%v has no route flag: the other two paths cannot be asked for there", v.Path)
			continue
		}
		seen++
		if found.Type != FlagString {
			t.Errorf("%v: route is %v, want a string naming one of three paths", v.Path, found.Type)
		}
		if d, _ := found.Default.(string); d != "splice" {
			t.Errorf("%v: route defaults to %q, want splice", v.Path, d)
		}
	}
	if seen != 7 {
		t.Errorf("checked %d file verbs, expected the 7 that take the flag", seen)
	}
}

// A bad route word must be refused at BIND time, before anything dials. The
// first version parsed it inside the action, so `--route bogus` opened a
// connection and then complained -- a round trip spent on a request that was
// never going to be sent.
func TestABadRouteWordIsRefusedBeforeAnythingRuns(t *testing.T) {
	for _, v := range Verbs {
		if len(v.Path) == 0 || v.Path[0] != "file" {
			continue
		}
		if v.Validate == nil {
			t.Errorf("%v has no Validate: a typo would reach the action", v.Path)
		}
	}
	got, err := ParseFileTransferRoute("bogus")
	if err == nil {
		t.Fatal("a typo was accepted")
	}
	if got != 0 {
		t.Fatalf("a refused word still produced route %v", got)
	}
}
