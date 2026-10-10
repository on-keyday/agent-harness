package cli

import (
	"os"
	"strings"
	"testing"
)

// Both task-control send paths check the size BEFORE SendMessage. A path that
// skipped it would send an over-budget request to be dropped silently over udp,
// which is how Zed's remote terminal hung with no error anywhere.
func TestTaskControlSendsCheckTheSizeFirst(t *testing.T) {
	src, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"func (c *Client) RoundTripTaskControl(", "func (c *Client) BeginTaskControl("} {
		i := strings.Index(string(src), fn)
		if i < 0 {
			t.Fatalf("%s not found", fn)
		}
		body := string(src[i:])
		if j := strings.Index(body[1:], "\nfunc "); j > 0 {
			body = body[:j+1]
		}
		check := strings.Index(body, "c.checkTaskControlSize(")
		send := strings.Index(body, ".SendMessage(")
		if check < 0 || send < 0 || check > send {
			t.Errorf("%s: checkTaskControlSize must come before SendMessage (check at %d, send at %d)", fn, check, send)
		}
	}
}
