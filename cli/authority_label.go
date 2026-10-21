package cli

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AuthorityLabel renders who last wrote a task's authority, and how long ago:
// `<source>@<actor> (<age>)`, the actor being the first 8 hex of a task, or
// `operator`, or `hidden` when the server withheld a task the caller cannot
// see. A task recorded before provenance existed is plain `unknown`.
//
// Every surface prints this one string — the ls row, the TUI popup, the WebUI
// sheet, the caps audit — so none of them can word the same record two ways.
func AuthorityLabel(t protocol.TaskInfo) string {
	if t.AuthoritySource == protocol.AuthoritySource_Unknown && t.AuthorityAt == 0 {
		return "unknown"
	}
	actor := "operator"
	switch {
	case t.AuthorityActorHidden():
		actor = "hidden"
	case t.AuthorityActor.Id != ([16]byte{}):
		actor = hex.EncodeToString(t.AuthorityActor.Id[:])[:8]
	}
	return t.AuthoritySource.String() + "@" + actor + " (" + AgeStr(t.AuthorityAgeMs) + ")"
}
