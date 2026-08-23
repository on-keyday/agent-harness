package cli

import "testing"

// ShownTo turns the server's one-watermark-per-(task,topic) into the
// per-message answer the operator surfaces render. The boundary is
// inclusive: shown IS the highest seq handed over, so a message at exactly
// that seq has been delivered.
func TestShownTo(t *testing.T) {
	subs := []BoardSubscriberRow{
		{TaskHex: "aaaa", Patterns: []BoardSubscriberPattern{
			{Name: "t.a", Shown: 20},
			{Name: "t.other", Shown: 999},
		}},
		{TaskHex: "bbbb", Patterns: []BoardSubscriberPattern{
			{Name: "t.a", Shown: 10},
		}},
		// Subscribes to something else entirely; must not be counted for t.a.
		{TaskHex: "cccc", Patterns: []BoardSubscriberPattern{
			{Name: "t.elsewhere", Shown: 100},
		}},
	}

	for _, tc := range []struct {
		name      string
		topic     string
		seq       uint64
		wantN     int
		wantTotal int
	}{
		{"below both marks", "t.a", 5, 2, 2},
		{"at the lower mark", "t.a", 10, 2, 2},
		{"between the marks", "t.a", 15, 1, 2},
		{"at the higher mark", "t.a", 20, 1, 2},
		{"above both", "t.a", 30, 0, 2},
		{"topic nobody subscribes to", "t.nobody", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, total := ShownTo(subs, tc.topic, tc.seq)
			if n != tc.wantN || total != tc.wantTotal {
				t.Errorf("ShownTo(%q, %d) = %d/%d, want %d/%d",
					tc.topic, tc.seq, n, total, tc.wantN, tc.wantTotal)
			}
		})
	}
}

// A board seq is UnixNano-seeded and far past Number.MAX_SAFE_INTEGER. The Go
// side compares uint64s exactly; this pins that adjacent seqs at that
// magnitude are distinguished, which is the property a float64 (or a JS
// mirror of this comparison) would lose.
func TestShownToKeepsFullSeqPrecision(t *testing.T) {
	const mark = uint64(1874327394971549698)
	subs := []BoardSubscriberRow{{Patterns: []BoardSubscriberPattern{{Name: "t", Shown: mark}}}}

	if n, _ := ShownTo(subs, "t", mark); n != 1 {
		t.Errorf("seq == mark: got %d, want 1 (the mark is inclusive)", n)
	}
	if n, _ := ShownTo(subs, "t", mark+1); n != 0 {
		t.Errorf("seq == mark+1: got %d, want 0 — one apart at 1.8e18 must not collapse", n)
	}
}

func TestShownToLabel(t *testing.T) {
	subs := []BoardSubscriberRow{{Patterns: []BoardSubscriberPattern{{Name: "t", Shown: 5}}}}
	if got := ShownToLabel(subs, "t", 9); got != "shown_to=0/1" {
		t.Errorf("label = %q, want shown_to=0/1", got)
	}
	if got := ShownToLabel(nil, "t", 9); got != "shown_to=0/0" {
		t.Errorf("label with no subscribers = %q, want shown_to=0/0", got)
	}
}
