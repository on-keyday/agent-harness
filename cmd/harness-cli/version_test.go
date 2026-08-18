package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The test binary is itself built from the tree, so the stamp is real: a
// dedicated fixture would only prove the formatter, not that go build actually
// records vcs.* — which is the whole premise of the command.
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
	var got buildStamp
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
	}
	if got.Go == "" {
		t.Error("go version should always be present in build info")
	}
}
