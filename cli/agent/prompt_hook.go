package agent

import (
	"encoding/json"
	"fmt"
	"io"
)

// emitUserPromptSubmitHookOutput writes the Claude Code UserPromptSubmit
// hook envelope carrying body as additional context.
//
// The envelope is mandatory, not cosmetic. Claude Code decides how to treat
// a hook's stdout by its first character: output that does not start with
// '{' is plain text and is shown to the model as-is, while output that does
// start with '{' is parsed as the hook-result envelope and any key the
// envelope schema does not define is discarded. Bare JSON Lines always
// start with '{', so they take the second branch and survive only by
// accident — two or more records fail JSON.parse and fall back to plain
// text, while a lone record parses cleanly and is dropped in full.
//
// Empty body writes nothing: additionalContext is a required string on this
// event, and an inbox with no messages has nothing to say.
func emitUserPromptSubmitHookOutput(w io.Writer, body string) {
	if body == "" {
		return
	}
	rec := map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": body,
		},
	}
	line, _ := json.Marshal(rec)
	fmt.Fprintln(w, string(line))
}
