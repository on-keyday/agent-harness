package runner

import (
	"testing"
)

// TestDriveAfterConnIsExported is a compile-time guard: verify the helper
// `driveAfterConn` exists with a signature the Listen path can call. The full
// behavioural test lives in listen_test.go (Task 4) which drives a real
// in-memory endpoint pair through the new path.
func TestDriveAfterConnIsExported(t *testing.T) {
	var _ = driveAfterConn
}
