package cliopts

import (
	"strings"
	"testing"
)

func TestResolveServerCIDRefusesNamed(t *testing.T) {
	if _, err := ResolveServerCID("wss:@3f2a9c0d11b7e604-*"); err == nil || !strings.Contains(err.Error(), "ws(s):// URL") {
		t.Fatalf("err = %v, want a refusal naming why", err)
	}
	if _, err := ResolveServerCID("ws:127.0.0.1:8539-*"); err != nil {
		t.Fatalf("an IP server cid: %v", err)
	}
}
