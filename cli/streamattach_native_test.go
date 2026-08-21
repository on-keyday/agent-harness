//go:build !js

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/streamagent"
)

// The follow view and the task log must render one message identically — both
// go through streamagent.RenderText, and this pins the contract from the CLI
// side so a local re-wording here would fail loudly rather than drift.
func TestRenderStreamLineMatchesTheSharedRenderer(t *testing.T) {
	lines := []string{
		`{"v":1,"kind":"event","event":{"kind":"text","text":"hello"}}`,
		`{"v":1,"kind":"event","event":{"kind":"tool_start","tool":"Bash","args":"{\"command\":\"ls\"}"}}`,
		`{"v":1,"kind":"request","request":{"id":"req-1","tool":"Write"}}`,
	}
	for _, l := range lines {
		var out, errOut bytes.Buffer
		renderStreamLine([]byte(l), &out, &errOut)
		m, err := streamagent.DecodeMsg([]byte(l))
		if err != nil {
			t.Fatalf("test line does not decode: %v", err)
		}
		want, ok := streamagent.RenderText(m)
		if !ok {
			t.Fatalf("shared renderer has no line for %s", l)
		}
		if got := strings.TrimSuffix(out.String(), "\n"); got != want {
			t.Errorf("follow view drifted from the shared renderer:\n  follow %q\n  shared %q", got, want)
		}
	}
}

// A line that is not the protocol must be SHOWN and marked, never dropped:
// `session send` can lawfully put raw bytes on the stream, and a follower who
// cannot see what a cowriter injected cannot explain what the adapter does
// next. Negative control for the marker: a valid event line must NOT carry it.
func TestRenderStreamLineShowsNonProtocolLinesMarked(t *testing.T) {
	var out, errOut bytes.Buffer
	renderStreamLine([]byte("hello, typed raw"), &out, &errOut)
	if !strings.Contains(out.String(), "(not the protocol)") || !strings.Contains(out.String(), "hello, typed raw") {
		t.Fatalf("raw line not surfaced: %q", out.String())
	}

	out.Reset()
	renderStreamLine([]byte(`{"v":1,"kind":"event","event":{"kind":"text","text":"ok"}}`), &out, &errOut)
	if strings.Contains(out.String(), "(not the protocol)") {
		t.Fatalf("valid line wrongly marked: %q", out.String())
	}
}

// Hello is informational (errOut), exit is part of the followed history (out).
func TestRenderStreamLineRoutesHelloAndExit(t *testing.T) {
	var out, errOut bytes.Buffer
	renderStreamLine([]byte(`{"v":1,"kind":"hello","hello":{"protocol":1,"vendor":"claude"}}`), &out, &errOut)
	if out.Len() != 0 || !strings.Contains(errOut.String(), "vendor=claude") {
		t.Fatalf("hello misrouted: out=%q err=%q", out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	renderStreamLine([]byte(`{"v":1,"kind":"exit","exit":{"code":3,"err":"boom"}}`), &out, &errOut)
	if !strings.Contains(out.String(), "code=3") || !strings.Contains(out.String(), "boom") {
		t.Fatalf("exit not rendered: %q", out.String())
	}
}
