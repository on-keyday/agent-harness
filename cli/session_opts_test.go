package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Caps is a plain value, not a pointer, and that is a security property rather
// than a simplification: the default is Capability_None, which IS the zero
// value, so "unset" and "explicitly --caps none" mean the same thing and there
// is nothing left for a presence bit to disambiguate. The consequence pinned
// here is that a SessionOpts nobody filled in spawns a task with NO
// capabilities — a caller that forgets the field fails closed. (It was a
// pointer while the default was Capability_All = 4095, where unset and zero
// had to be told apart or "--caps none" would have read as inherit-all.)
func TestSessionOptsCapsResolution(t *testing.T) {
	narrow := protocol.Capability_Spawn | protocol.Capability_FileRead

	tests := []struct {
		name string
		opts SessionOpts
		want protocol.Capability
	}{
		{"zero value grants nothing", SessionOpts{}, protocol.Capability_None},
		{"explicit none stays none", SessionOpts{Caps: protocol.Capability_None}, protocol.Capability_None},
		{"explicit narrow mask preserved", SessionOpts{Caps: narrow}, narrow},
		{"explicit all is all", SessionOpts{Caps: protocol.Capability_All}, protocol.Capability_All},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Both wire builders must carry the value, not re-derive it.
			if got := buildSubmitRequest("/repo", "p", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildSubmitRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
			if got := buildOpenInteractiveRequest("/repo", tc.opts).RequestedCaps; got != tc.want {
				t.Errorf("buildOpenInteractiveRequest.RequestedCaps = %v, want %v", got, tc.want)
			}
		})
	}
}

// A detached open carries its size in the request (there is no stream to carry
// it); an attached one does not, because its client sends the size itself.
func TestBuildOpenInteractiveRequestDetached(t *testing.T) {
	det := buildOpenInteractiveRequest("/r", SessionOpts{Detached: true, InitialRows: 40, InitialCols: 150})
	if !det.Detached() {
		t.Fatal("Detached not set on the request")
	}
	if r, c := det.InitialRows(), det.InitialCols(); r == nil || c == nil || *r != 40 || *c != 150 {
		t.Fatalf("initial size %v x %v, want 40 x 150", r, c)
	}
	if _, err := det.Append(nil); err != nil {
		t.Fatalf("detached request does not encode: %v", err)
	}
	att := buildOpenInteractiveRequest("/r", SessionOpts{InitialRows: 40, InitialCols: 150})
	if att.Detached() || att.InitialRows() != nil {
		t.Fatal("an attached open carries a size block")
	}
}

func TestBuildRequestsCarryResumeExact(t *testing.T) {
	opts := SessionOpts{ResumeExact: true}
	sub := buildSubmitRequest("/r", "", opts)
	if !sub.ResumeExact() {
		t.Error("buildSubmitRequest dropped ResumeExact")
	}
	oi := buildOpenInteractiveRequest("/r", opts)
	if !oi.ResumeExact() {
		t.Error("buildOpenInteractiveRequest dropped ResumeExact")
	}
}

// The two new refusals name what to do about them.
func TestStatusErrorsNameTheFix(t *testing.T) {
	sub := &protocol.SubmitResponse{Status: protocol.SubmitStatus_ResumeExactMismatch}
	sub.SetErrorMsg([]byte("resume_exact: --agent-arg is a setting"))
	err := submitStatusError(nil, nil, sub)
	if err == nil || !strings.Contains(err.Error(), "--exact") || !strings.Contains(err.Error(), "--agent-arg") {
		t.Errorf("submit mismatch: %v, want it to name --exact and the server's field", err)
	}
	if err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_ResumeExactMismatch); err == nil || !strings.Contains(err.Error(), "--exact") {
		t.Errorf("open mismatch: %v, want it to name --exact", err)
	}
	if err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_ScopeNotPermitted); err == nil || !strings.Contains(err.Error(), "scope_not_permitted") || !strings.Contains(err.Error(), "resume") {
		t.Errorf("scope_not_permitted: %v, want it named, with the spawn-override hint naming resume", err)
	}
	if err := openInteractiveStatusError("/r", "", protocol.OpenInteractiveStatus_DetachRequired); err == nil || !strings.Contains(err.Error(), "-d") {
		t.Errorf("detach_required: %v, want it to name -d", err)
	}
}

// caps set's invalid-scope answer covers the spawn override too, which is not
// an overlap or an empty mask.
func TestSetCapsInvalidScopeNamesSpawn(t *testing.T) {
	if err := setCapsStatusError(protocol.SetCapsStatus_InvalidScope); err == nil || !strings.Contains(err.Error(), "spawn") {
		t.Fatalf("invalid_scope: %v, want it to name the spawn override", err)
	}
}
