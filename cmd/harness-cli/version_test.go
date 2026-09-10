package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/buildinfo"
)

// These cover the FORMATTER, and the comment here used to claim more than that:
// "the test binary is itself built from the tree, so the stamp is real". It is
// not — `go test` records no vcs.* settings and passes none of the Makefile's
// -ldflags, so buildinfo.Read() comes back with an empty revision here. Whether
// a stamp is recorded at all is a property of the BUILD, and the guard for it
// lives in buildinfo (TestReadPrefersTheLinkTimeStampAsASet) plus the Makefile
// that supplies it.
func TestWriteVersion_HumanNamesTheRevision(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVersion(&buf, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "harness-cli ") {
		t.Errorf("want a line leading with the binary name, got %q", out)
	}
	if !strings.Contains(out, "clean") && !strings.Contains(out, "DIRTY") {
		t.Errorf("want the clean/dirty state called out, got %q", out)
	}
}

func TestWriteVersion_JSONDecodes(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVersion(&buf, true); err != nil {
		t.Fatal(err)
	}
	var got buildinfo.Stamp
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if got.Go == "" {
		t.Error("go version should always be present in build info")
	}
}
