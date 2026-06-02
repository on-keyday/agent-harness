package cli

import "testing"

func TestAgentStr(t *testing.T) {
	cases := []struct {
		bin      string
		injected bool
		want     string
	}{
		{"claude", true, "agent=claude+skills"},
		{"gemini", false, "agent=gemini"},
		{"bash", false, "agent=bash"},
		{"", false, "agent=?"},
		{"", true, "agent=?+skills"},
	}
	for _, c := range cases {
		if got := agentStr(c.bin, c.injected); got != c.want {
			t.Errorf("agentStr(%q,%v)=%q want %q", c.bin, c.injected, got, c.want)
		}
	}
}
