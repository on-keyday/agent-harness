package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitStopHookOutput_EmptyReason_NoOutput(t *testing.T) {
	var buf bytes.Buffer
	emitStopHookOutput(&buf, "")
	if got := buf.String(); got != "" {
		t.Errorf("expected no output for empty reason, got %q", got)
	}
}

func TestEmitStopHookOutput_NonEmptyReason_BlockDecision(t *testing.T) {
	var buf bytes.Buffer
	emitStopHookOutput(&buf, `{"seq":1,"topic":"chat/demo","payload_b64":"aGk="}`+"\n")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected output, got empty")
	}

	var rec map[string]string
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v; raw=%q", err, line)
	}
	if rec["decision"] != "block" {
		t.Errorf("decision = %q, want %q", rec["decision"], "block")
	}
	if !strings.Contains(rec["reason"], `"topic":"chat/demo"`) {
		t.Errorf("reason missing topic: %q", rec["reason"])
	}
}
