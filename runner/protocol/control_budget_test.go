package protocol

import (
	"strings"
	"testing"
)

func TestCheckControlMessage(t *testing.T) {
	err := CheckControlMessage("udp", 1170, 1171)
	if err == nil || !strings.Contains(err.Error(), "1171") || !strings.Contains(err.Error(), "1170") {
		t.Fatalf("over budget on udp: %v, want an error naming 1171 and 1170", err)
	}
	for _, c := range []struct {
		tr        string
		budget, n int
	}{{"udp", 1170, 1170}, {"udp", 1470, 1300}, {"ws", 1170, 64 << 10}, {"wss", 1170, 64 << 10}} {
		if err := CheckControlMessage(c.tr, c.budget, c.n); err != nil {
			t.Errorf("%s budget=%d n=%d: %v, want nil", c.tr, c.budget, c.n, err)
		}
	}
}
