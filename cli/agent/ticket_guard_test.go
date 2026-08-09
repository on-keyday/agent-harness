package agent

import (
	"strings"
	"testing"
)

const testTicket = "0123456789abcdef0123456789abcdef"

func TestRefuseIfOwnTicket(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		payload string
		refuse  bool
	}{
		{"no env", "", "HARNESS_AUTH_TICKET=" + testTicket, false},
		{"short env is not a ticket", "abc", "abc", false},
		{"clean payload", testTicket, `{"msg":"hello"}`, false},
		{"bare ticket", testTicket, testTicket, true},
		{"env dump line", testTicket, "PATH=/usr/bin\nHARNESS_AUTH_TICKET=" + testTicket + "\nHOME=/root\n", true},
		{"recased", testTicket, "ticket: " + strings.ToUpper(testTicket), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HARNESS_AUTH_TICKET", c.env)
			err := refuseIfOwnTicket([]byte(c.payload))
			if c.refuse && err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !c.refuse && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
