package cli

import (
	"bufio"
	"strings"
	"testing"
)

// A capped replay starts wherever the cap fell, which is a frame boundary and
// not a line boundary: the first line is dropped when it is a fragment, and
// kept when the cap happened to land on a line start.
func TestStreamSessionCutDropsLeadingFragment(t *testing.T) {
	whole := `{"v":1,"kind":"progress","progress":{}}`
	for _, tc := range []struct {
		name  string
		input string
		cut   bool
		want  []bool // Decoded, per line returned
	}{
		{"fragment dropped", `ss":{}}` + "\n" + whole + "\n", true, []bool{true}},
		{"whole first line kept", whole + "\n" + whole + "\n", true, []bool{true, true}},
		{"uncapped keeps everything", `ss":{}}` + "\n" + whole + "\n", false, []bool{false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &StreamSession{rd: bufio.NewReader(strings.NewReader(tc.input)), cut: tc.cut}
			var got []bool
			for {
				l, err := s.ReadLine()
				if l.Raw != nil {
					got = append(got, l.Decoded)
				}
				if err != nil {
					break
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("lines = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("lines = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
