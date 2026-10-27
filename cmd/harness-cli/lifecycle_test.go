package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/objtrsf/objproto"
)

// A lifecycle hook runs inside claude's hook pipeline: whatever happens to the
// server, it must exit cleanly and say nothing on stdout (spec § CLI).
func TestLifecycleCmd_ServerDownExitsZeroSilently(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HARNESS_TASK_ID", "00112233445566778899aabbccddeeff")
	// A closed loopback port: the dial is refused at once.
	h := cliVerbs{ctx: context.Background(), cid: func() objproto.ConnectionID {
		return objproto.MustParseConnectionID("ws:127.0.0.1:1-1")
	}}

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	err := h.Lifecycle(verb.LifecycleAction{State: "working"})
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	var out, errb bytes.Buffer
	io.Copy(&out, outR)
	io.Copy(&errb, errR)

	if err != nil {
		t.Fatalf("Lifecycle returned %v; a hook must exit 0", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
	if lines := strings.Split(strings.TrimRight(errb.String(), "\n"), "\n"); errb.Len() == 0 || len(lines) != 1 {
		t.Fatalf("stderr = %q, want exactly one line", errb.String())
	}
}
