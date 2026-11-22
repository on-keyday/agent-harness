package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A task's refused re-grant carries the authority package's own text.
func TestSetCapsErrorCarriesServerMessage(t *testing.T) {
	err := setCapsStatusError(protocol.SetCapsStatus_InvalidScope, "scope id 0123abcd is outside what you may delegate")
	if err == nil || !strings.Contains(err.Error(), "outside what you may delegate") {
		t.Fatalf("err = %v, want the server's text", err)
	}
	if err := setCapsStatusError(protocol.SetCapsStatus_NotFound, ""); err == nil || !strings.Contains(err.Error(), "descendants") {
		t.Fatalf("not_found err = %v, want the descendant rule named", err)
	}
}

func TestTaskRenameStatusErrors(t *testing.T) {
	if err := taskRenameStatusError(protocol.TaskRenameStatus_Ok); err != nil {
		t.Fatal(err)
	}
	if err := taskRenameStatusError(protocol.TaskRenameStatus_NotFound); err == nil || !strings.Contains(err.Error(), "descendants") {
		t.Fatalf("not_found = %v", err)
	}
	if err := taskRenameStatusError(protocol.TaskRenameStatus_BadName); err == nil || !strings.Contains(err.Error(), "64 bytes") {
		t.Fatalf("bad_name = %v", err)
	}
}

// R9: a task's what-if predicts with the task as actor; the operator's has none.
func TestWhatIfChangeAsSetsActor(t *testing.T) {
	a := verb.CapsWhatIfAction{TaskID: strings.Repeat("a", 32), Caps: CapsPtr(protocol.Capability_FileRead)}
	r, ok := WhatIfChangeAs(a, strings.Repeat("b", 32)).(authority.Regrant)
	if !ok || r.Actor != strings.Repeat("b", 32) {
		t.Fatalf("change = %+v", WhatIfChangeAs(a, strings.Repeat("b", 32)))
	}
	if r, _ := WhatIfChange(a).(authority.Regrant); r.Actor != "" {
		t.Fatalf("operator change has actor %q", r.Actor)
	}
}

// Final review #1: a name the wire cannot carry (u8 length) must not reach the
// server as an empty name, which would CLEAR the task's name and report ok.
func TestTaskRenameRefusesAnOverlongNameBeforeSending(t *testing.T) {
	err := taskRenameOn(t.Context(), nil, strings.Repeat("a", 32), strings.Repeat("n", 300))
	if err == nil || !strings.Contains(err.Error(), "64 bytes") {
		t.Fatalf("err = %v, want a refusal naming the 64-byte limit", err)
	}
}
