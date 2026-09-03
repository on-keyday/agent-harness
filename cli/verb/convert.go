package verb

import "time"

// The generated builds read Bound's untyped maps through these, so a value
// whose type does not match reads as the zero value rather than panicking.
//
// That silence is exactly what bit `agent wait --timeout` before generation --
// `Default: 0` is an int, the FlagDuration branch's assertion failed, and the
// default became zero. The generator cannot make that mistake (it writes the
// reader from the declared FlagType) and TestNoVerbDeclaresAFlagItCannotType
// refuses the declaration that would cause it.

func uintOf(v any) uint {
	n, _ := v.(uint)
	return n
}

func uint64Of(v any) uint64 {
	n, _ := v.(uint64)
	return n
}

func durationOf(v any) time.Duration {
	d, _ := v.(time.Duration)
	return d
}

// stringsOf reads a Custom flag's accumulated values.
func stringsOf(v any) []string {
	s, _ := v.([]string)
	return s
}
