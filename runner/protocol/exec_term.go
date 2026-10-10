package protocol

import "fmt"

// ValidateExecTerm checks the TERM an exec request carries (ExecRunRequest.term).
// Empty is valid and means "leave TERM alone".
//
// The value becomes ONE environment variable in the child, so the alphabet is
// printable non-space ASCII: no byte a terminfo lookup, a shell or an env block
// would read as a separator. The client checks before sending and the runner
// checks again before running — the runner is the party that applies it.
func ValidateExecTerm(term string) error {
	if len(term) > 255 {
		return fmt.Errorf("term is %d bytes, at most 255", len(term))
	}
	for i := 0; i < len(term); i++ {
		if c := term[i]; c < 0x21 || c > 0x7e {
			return fmt.Errorf("term %q: byte %#x at %d is not printable non-space ASCII", term, c, i)
		}
	}
	return nil
}
