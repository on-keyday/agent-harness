package cli

import "testing"

// Every pre-range call site passes the zero value, and none of them intends a
// range, so "zero means whole file" is what keeps them correct.
func TestFileTransferRangeZeroValueIsWholeFile(t *testing.T) {
	var r FileTransferRange
	if r.Offset != 0 || r.Length != 0 {
		t.Fatalf("zero value is %+v", r)
	}
}
