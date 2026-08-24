package cli

import (
	"strings"
	"testing"
)

const gridTestID = "3f2a9c00000000000000000000000001"

func TestParseGridArgs(t *testing.T) {
	mode, anchor, ids, err := ParseGridArgs(nil)
	if err != nil || mode != GridAll || anchor != "" || len(ids) != 0 {
		t.Errorf("no args = %v %q %v %v; want GridAll", mode, anchor, ids, err)
	}
	mode, anchor, _, err = ParseGridArgs([]string{"--under", gridTestID})
	if err != nil || mode != GridSubtree || anchor != gridTestID {
		t.Errorf("--under = %v %q %v; want GridSubtree/%s", mode, anchor, err, gridTestID)
	}
	mode, _, _, err = ParseGridArgs([]string{"--under", gridTestID, "--descendants"})
	if err != nil || mode != GridDescendants {
		t.Errorf("--under --descendants = %v %v; want GridDescendants", mode, err)
	}
	mode, _, ids, err = ParseGridArgs([]string{gridTestID})
	if err != nil || mode != GridIds || len(ids) != 1 {
		t.Errorf("bare id = %v %v %v; want GridIds", mode, ids, err)
	}
	for _, bad := range [][]string{
		{"--under"},
		{"--under", gridTestID, gridTestID},
		{"--nope"},
		{"--descendants"},
		{gridTestID, "--descendants"},
	} {
		if _, _, _, err := ParseGridArgs(bad); err == nil {
			t.Errorf("ParseGridArgs(%q) succeeded, want an error", bad)
		}
	}
}

// The renderer and the parser are two halves of one grammar; a serializer whose
// output its own parser rejects is the recurring defect this pins.
func TestGridArgsRoundTrip(t *testing.T) {
	cases := []struct {
		mode   GridScopeMode
		anchor string
		ids    []string
	}{
		{GridAll, "", nil},
		{GridSubtree, gridTestID, nil},
		{GridDescendants, gridTestID, nil},
		{GridIds, "", []string{gridTestID, "7b1e000000000000000000000000000f"}},
	}
	for _, c := range cases {
		s := GridArgsString(c.mode, c.anchor, c.ids)
		mode, anchor, ids, err := ParseGridArgs(strings.Fields(s))
		if err != nil {
			t.Errorf("%v rendered %q, which does not parse: %v", c.mode, s, err)
			continue
		}
		if mode != c.mode || anchor != c.anchor || len(ids) != len(c.ids) {
			t.Errorf("%v round-tripped through %q as %v/%q/%v", c.mode, s, mode, anchor, ids)
		}
	}
}
