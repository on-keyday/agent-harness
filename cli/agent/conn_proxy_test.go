package agent

import (
	"os"
	"testing"
)

// TestShouldUseProxyEnv verifies the env-gated dispatch decision:
// HARNESS_PROXY_VIA_RUNNER=<addr> → proxy mode with addr as the destination.
// unset / empty → direct mode.
func TestShouldUseProxyEnv(t *testing.T) {
	prev := os.Getenv("HARNESS_PROXY_VIA_RUNNER")
	t.Cleanup(func() { os.Setenv("HARNESS_PROXY_VIA_RUNNER", prev) })

	// Set
	os.Setenv("HARNESS_PROXY_VIA_RUNNER", "ws:127.0.0.1:9999-*")
	useProxy, addr := shouldUseProxy()
	if !useProxy {
		t.Fatal("expected shouldUseProxy()=true when env set")
	}
	if addr != "ws:127.0.0.1:9999-*" {
		t.Errorf("addr: got %q want %q", addr, "ws:127.0.0.1:9999-*")
	}

	// Whitespace-only treated as unset
	os.Setenv("HARNESS_PROXY_VIA_RUNNER", "   ")
	useProxy, _ = shouldUseProxy()
	if useProxy {
		t.Fatal("expected shouldUseProxy()=false for whitespace-only env")
	}

	// Unset
	os.Unsetenv("HARNESS_PROXY_VIA_RUNNER")
	useProxy, addr = shouldUseProxy()
	if useProxy {
		t.Fatal("expected shouldUseProxy()=false when env unset")
	}
	if addr != "" {
		t.Errorf("addr: got %q want empty", addr)
	}
}
