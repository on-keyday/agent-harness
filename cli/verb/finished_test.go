package verb

import "testing"

func TestParseFinished(t *testing.T) {
	for in, want := range map[string]uint32{"all": FinishedAll, "0": 0, "250": 250} {
		if got, err := ParseFinished(in); err != nil || got != want {
			t.Fatalf("ParseFinished(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "many", "4294967296"} {
		if _, err := ParseFinished(bad); err == nil {
			t.Fatalf("ParseFinished(%q) accepted", bad)
		}
	}
}
