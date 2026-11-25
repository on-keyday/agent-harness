package protocol

import "fmt"

// The protocol's words for its enums: what a flag accepts, `ls --json`
// prints, the WebUI shows and the server writes into notifications and board
// payloads. Spelled out rather than taken from the generated String(), which
// capitalizes ("Blocked"). Here rather than in package cli because the server
// writes them too, and it does not import cli.

// ParseLifecycleState accepts the wire words only.
func ParseLifecycleState(s string) (LifecycleState, bool) {
	switch s {
	case "working":
		return LifecycleState_Working, true
	case "waiting":
		return LifecycleState_Waiting, true
	case "idle":
		return LifecycleState_Idle, true
	case "error":
		return LifecycleState_Error, true
	}
	return LifecycleState_Unset, false
}

// LifecycleWord is the wire word for st, "" for unset.
func LifecycleWord(st LifecycleState) string {
	switch st {
	case LifecycleState_Working:
		return "working"
	case LifecycleState_Waiting:
		return "waiting"
	case LifecycleState_Idle:
		return "idle"
	case LifecycleState_Error:
		return "error"
	}
	return ""
}

// ProgramWord is the wire word for a program state, "" for none.
func ProgramWord(s ProgramState) string {
	// Spelled out: the generated String() capitalizes ("Blocked"), and these
	// are the protocol's words, shared with ls --json and the WebUI.
	switch s {
	case ProgramState_Idle:
		return "idle"
	case ProgramState_Working:
		return "working"
	case ProgramState_Done:
		return "done"
	case ProgramState_Blocked:
		return "blocked"
	case ProgramState_Error:
		return "error"
	}
	return ""
}

// ProgramKindWord is the wire word for a blocked kind, "" for none.
func ProgramKindWord(k ProgramKind) string {
	switch k {
	case ProgramKind_Permission:
		return "permission"
	case ProgramKind_Question:
		return "question"
	case ProgramKind_Auth:
		return "auth"
	}
	return ""
}

// awaitIdleSignalWords is the schema's word for each signal, the one spelling
// the flag, the JSON output, the board payload and the watcher lists share.
var awaitIdleSignalWords = map[AwaitIdleSignal]string{
	AwaitIdleSignal_Quiet:     "quiet",
	AwaitIdleSignal_Lifecycle: "lifecycle",
	AwaitIdleSignal_Program:   "program",
	AwaitIdleSignal_Any:       "any",
}

// AwaitIdleSignalString is the signal's word, unknown(N) outside the enum.
func AwaitIdleSignalString(s AwaitIdleSignal) string {
	if w, ok := awaitIdleSignalWords[s]; ok {
		return w
	}
	return fmt.Sprintf("unknown(%d)", int(s))
}

// ParseAwaitIdleSignal reads a --signal word; "" is the default, quiet.
func ParseAwaitIdleSignal(word string) (AwaitIdleSignal, error) {
	if word == "" {
		return AwaitIdleSignal_Quiet, nil
	}
	for s, w := range awaitIdleSignalWords {
		if w == word {
			return s, nil
		}
	}
	return 0, fmt.Errorf("unknown signal %q (want quiet, lifecycle, program or any)", word)
}

// AwaitIdleFiredDetail is what a fire saw, for the leg that fired: "program
// done", "program blocked:permission", "lifecycle waiting". "" for the quiet
// leg, whose surfaces keep their "last output" wording. One spelling for the
// notification, the TUI result line and the WebUI.
func AwaitIdleFiredDetail(firedBy AwaitIdleSignal, lc LifecycleState, ps ProgramState, pk ProgramKind) string {
	switch firedBy {
	case AwaitIdleSignal_Program:
		state := ProgramWord(ps)
		if ps == ProgramState_Blocked && pk != ProgramKind_None {
			state += ":" + ProgramKindWord(pk)
		}
		return "program " + state
	case AwaitIdleSignal_Lifecycle:
		return "lifecycle " + LifecycleWord(lc)
	}
	return ""
}
