package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// What a verb shows on stderr goes to the writer its caller hands it, not to
// the process's fd 2: in the MCP server that writer is the tool result, and
// fd 2 is a log the model never sees (spec D20).

func TestRenderThreadsJSONWindowLineGoesToStderrSink(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := RenderThreads(&out, nil, ThreadRenderOptions{JSON: true, Window: "the window line", Stderr: &errOut}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "the window line") {
		t.Errorf("--json window line went elsewhere: stderr sink %q, stdout %q", errOut.String(), out.String())
	}
	if strings.Contains(out.String(), "the window line") {
		t.Errorf("--json window line joined the feed: %q", out.String())
	}
}

func TestNotifyTruncationNoteGoesToStderrSink(t *testing.T) {
	nr := newNotifyRequest(protocol.NotifyLevel_Info, "", strings.Repeat("あ", 2000)) // ~6 KB, over budget
	var errOut bytes.Buffer
	mtuGuardNotify(nr, &errOut)
	if !strings.Contains(errOut.String(), "notify: text truncated") {
		t.Errorf("truncation note: %q, want it on the given writer", errOut.String())
	}
}
