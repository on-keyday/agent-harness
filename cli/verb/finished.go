package verb

import (
	"fmt"
	"math"
	"strconv"
)

// FinishedDefault is how many finished tasks a listing carries when nobody
// asked for another number: ListQuery.finished_limit's value on every surface
// that does not offer a choice, and the one the choices start from.
const FinishedDefault uint32 = 100

// FinishedAll asks for every finished task. It is a limit like any other
// (ListQuery.finished_limit's maximum), named because "all" is what a reader
// of the calling code means by it.
const FinishedAll uint32 = math.MaxUint32

// ParseFinished is the grammar of ls --finished: "all", or a decimal count.
// 0 is a real answer (no finished task at all), as for --spawn-depth.
func ParseFinished(s string) (uint32, error) {
	if s == "all" {
		return FinishedAll, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q: want a number of finished tasks, or all", s)
	}
	return uint32(n), nil
}

// parseFinishedFlag is ls --finished.
func parseFinishedFlag(s string) (*uint32, error) {
	n, err := ParseFinished(s)
	if err != nil {
		return nil, err
	}
	return &n, nil
}
