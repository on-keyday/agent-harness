package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitUserPromptSubmitHookOutput_EmptyBody_NoOutput(t *testing.T) {
	var buf bytes.Buffer
	emitUserPromptSubmitHookOutput(&buf, "")
	if got := buf.String(); got != "" {
		t.Errorf("expected no output for empty body, got %q", got)
	}
}

// The regression this guards: a single JSON-Lines record is itself a valid
// JSON object, so Claude Code parses it as the hook envelope and drops every
// key. The envelope must therefore be the outermost object, with the records
// carried as an opaque string.
func TestEmitUserPromptSubmitHookOutput_WrapsRecordsAsAdditionalContext(t *testing.T) {
	const body = `{"seq":1,"topic":"chat/demo","payload_b64":"aGk="}` + "\n"

	var buf bytes.Buffer
	emitUserPromptSubmitHookOutput(&buf, body)

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected output, got empty")
	}

	var rec struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("output is not a single valid JSON object: %v; raw=%q", err, out)
	}
	if rec.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want %q", rec.HookSpecificOutput.HookEventName, "UserPromptSubmit")
	}
	if rec.HookSpecificOutput.AdditionalContext != body {
		t.Errorf("additionalContext = %q, want the records verbatim %q",
			rec.HookSpecificOutput.AdditionalContext, body)
	}
}

// Two records concatenated are not parseable as one JSON value; they must
// still ride inside additionalContext rather than reaching stdout bare.
func TestEmitUserPromptSubmitHookOutput_MultipleRecords_SingleEnvelope(t *testing.T) {
	body := `{"seq":1,"topic":"a"}` + "\n" + `{"seq":2,"topic":"b"}` + "\n"

	var buf bytes.Buffer
	emitUserPromptSubmitHookOutput(&buf, body)

	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
		t.Errorf("expected exactly one output line, got %d newlines: %q", n, buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
		t.Fatalf("output is not a single valid JSON object: %v", err)
	}
}
